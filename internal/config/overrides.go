package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// Live lists the variables that can be changed while running (from the
// dashboard). Everything else needs a restart and comes only from the
// environment.
var Live = []string{"CACHE_DISK_SIZE", "MIN_FREE_DISK", "CACHE_MAX_AGE", "CACHE_MAX_PREFETCHES", "UPSTREAM_RATE_LIMIT", "BYPASS_DOMAINS"}

// Overrides are values saved from the dashboard, kept in a JSON file
// (SETTINGS_FILE) of variable name to value, written the same way as the
// environment variable. They take precedence over the environment.
type Overrides struct {
	path   string
	getenv func(string) string

	mu   sync.Mutex
	vals map[string]string
}

// LoadOverrides reads path; a missing file means no overrides. Entries that
// are not Live, or whose value does not parse, are dropped and returned as
// warnings rather than failing startup over a hand-edited file.
func LoadOverrides(path string, getenv func(string) string) (*Overrides, []string, error) {
	o := &Overrides{path: path, getenv: getenv, vals: map[string]string{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return o, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var vals map[string]string
	if err := json.Unmarshal(b, &vals); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	var warnings []string
	for _, k := range slices.Sorted(maps.Keys(vals)) {
		if !slices.Contains(Live, k) {
			warnings = append(warnings, fmt.Sprintf("%s: %s cannot be set here; ignored", path, k))
			continue
		}
		if _, err := Load(o.with(map[string]*string{k: ptr(vals[k])})); err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v; ignored", path, err))
			continue
		}
		o.vals[k] = vals[k]
	}
	return o, warnings, nil
}

func ptr(s string) *string { return &s }

// Getenv reads a variable with the overrides applied; pass it to Load.
func (o *Overrides) Getenv(key string) string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if v, ok := o.vals[key]; ok {
		return v
	}
	return o.getenv(key)
}

// Source says where key's value comes from: "dashboard", "env" or "default".
func (o *Overrides) Source(key string) string {
	o.mu.Lock()
	_, ok := o.vals[key]
	o.mu.Unlock()
	switch {
	case ok:
		return "dashboard"
	case Raw(o.getenv, key) != Defaults[key]:
		return "env"
	}
	return "default"
}

// Set applies changes (a nil value removes the override, falling back to
// the environment), validates the result and saves it. Only Live variables
// may be changed. It returns the configuration now in effect.
func (o *Overrides) Set(changes map[string]*string) (*Config, error) {
	for k := range changes {
		if !slices.Contains(Live, k) {
			return nil, fmt.Errorf("%s cannot be changed while running", k)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	next := maps.Clone(o.vals)
	for k, v := range changes {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = *v
		}
	}
	cfg, err := Load(func(k string) string {
		if v, ok := next[k]; ok {
			return v
		}
		return o.getenv(k)
	})
	if err != nil {
		return nil, err
	}
	if err := o.save(next); err != nil {
		return nil, err
	}
	o.vals = next
	return cfg, nil
}

// with returns a getenv that applies changes on top of the saved overrides
// without storing them.
func (o *Overrides) with(changes map[string]*string) func(string) string {
	return func(k string) string {
		if v, ok := changes[k]; ok && v != nil {
			return *v
		}
		if v, ok := o.vals[k]; ok {
			return v
		}
		return o.getenv(k)
	}
}

// save writes vals atomically.
func (o *Overrides) save(vals map[string]string) error {
	b, err := json.MarshalIndent(vals, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(o.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(o.path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), o.path)
}
