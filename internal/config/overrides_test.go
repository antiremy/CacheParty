package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestOverridesPrecedenceAndPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "settings.json")
	env := envOf(map[string]string{"CACHE_DISK_SIZE": "500g"})
	o, warns, err := LoadOverrides(path, env)
	if err != nil || len(warns) > 0 {
		t.Fatal(err, warns)
	}
	if got := o.Source("CACHE_DISK_SIZE"); got != "env" {
		t.Fatalf("source before override = %q", got)
	}
	if got := o.Source("CACHE_MAX_AGE"); got != "default" {
		t.Fatalf("CACHE_MAX_AGE source = %q", got)
	}

	cfg, err := o.Set(map[string]*string{"CACHE_DISK_SIZE": ptr("2t"), "CACHE_MAX_AGE": ptr("30d")})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CacheDiskSize != 2<<40 {
		t.Fatalf("CacheDiskSize = %d", cfg.CacheDiskSize)
	}
	if got := o.Getenv("CACHE_DISK_SIZE"); got != "2t" {
		t.Fatalf("override not applied: %q", got)
	}

	// A fresh load sees what was saved.
	o2, _, err := LoadOverrides(path, env)
	if err != nil {
		t.Fatal(err)
	}
	if got := o2.Getenv("CACHE_MAX_AGE"); got != "30d" {
		t.Fatalf("after reload CACHE_MAX_AGE = %q", got)
	}

	// Resetting falls back to the environment.
	if _, err := o2.Set(map[string]*string{"CACHE_DISK_SIZE": nil}); err != nil {
		t.Fatal(err)
	}
	if got, src := o2.Getenv("CACHE_DISK_SIZE"), o2.Source("CACHE_DISK_SIZE"); got != "500g" || src != "env" {
		t.Fatalf("after reset: %q from %s", got, src)
	}
}

func TestOverridesRejectInvalidAndNonLive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	o, _, err := LoadOverrides(path, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Set(map[string]*string{"CACHE_DISK_SIZE": ptr("lots")}); err == nil {
		t.Fatal("invalid size accepted")
	}
	if _, err := o.Set(map[string]*string{"CACHE_SLICE_SIZE": ptr("2m")}); err == nil {
		t.Fatal("non-live variable accepted")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a rejected change was saved")
	}
	if got := o.Getenv("CACHE_DISK_SIZE"); got != "" {
		t.Fatalf("rejected value applied: %q", got)
	}
}

func TestLoadOverridesDropsBadEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(path, []byte(`{"CACHE_MAX_AGE":"7d","MIN_FREE_DISK":"x","HTTP_ADDR":":81"}`), 0o644)
	o, warns, err := LoadOverrides(path, envOf(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 2 || !strings.Contains(warns[0], "HTTP_ADDR") || !strings.Contains(warns[1], "MIN_FREE_DISK") {
		t.Fatalf("warnings = %q", warns)
	}
	if o.Getenv("CACHE_MAX_AGE") != "7d" || o.Getenv("MIN_FREE_DISK") != "" || o.Getenv("HTTP_ADDR") != "" {
		t.Fatal("wrong entries kept")
	}
}
