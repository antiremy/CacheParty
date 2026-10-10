package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"1000g", 1000 << 30, false},
		{"10g", 10 << 30, false},
		{"1m", 1 << 20, false},
		{"1M", 1 << 20, false},
		{"512k", 512 << 10, false},
		{"2t", 2 << 40, false},
		{"4096", 4096, false},
		{" 8m ", 8 << 20, false},
		{"", 0, true},
		{"g", 0, true},
		{"10x", 0, true},
		{"-5m", 0, true},
		{"99999999999t", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseSize(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseRate(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"0", 0, false},
		{"500m", 62_500_000, false},
		{"500mbit", 62_500_000, false},
		{"1G", 125_000_000, false},
		{"1gbit", 125_000_000, false},
		{"800k", 100_000, false},
		{"8000", 1000, false},
		{" 100m ", 12_500_000, false},
		{"7", 0, true},
		{"", 0, true},
		{"m", 0, true},
		{"10mb", 0, true},
		{"-5m", 0, true},
		{"99999999999999g", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseRate(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseRate(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseDuration(t *testing.T) {
	day := 24 * time.Hour
	tests := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"3560d", 3560 * day, false},
		{"30", 30 * time.Second, false},
		{"90s", 90 * time.Second, false},
		{"5m", 5 * time.Minute, false},
		{"250ms", 250 * time.Millisecond, false},
		{"1h30m", 90 * time.Minute, false},
		{"2w", 14 * day, false},
		{"1M", 30 * day, false},
		{"1y", 365 * day, false},
		{"", 0, true},
		{"d", 0, true},
		{"5q", 0, true},
		{"99999999999y", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseDuration(tt.in)
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}

func TestParseDNSServers(t *testing.T) {
	got, err := ParseDNSServers("8.8.8.8 1.1.1.1:5353,2001:4860:4860::8888")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"8.8.8.8:53", "1.1.1.1:5353", "[2001:4860:4860::8888]:53"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	if _, err := ParseDNSServers("  "); err == nil {
		t.Fatal("expected error for empty list")
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if c.CacheDiskSize != 1000<<30 || c.MinFreeDisk != 10<<30 || c.SliceSize != 1<<20 {
		t.Errorf("bad size defaults: %+v", c)
	}
	if c.MaxPrefetches != 64 {
		t.Errorf("bad max prefetches default: %d", c.MaxPrefetches)
	}
	if c.CacheMaxAge != 3560*24*time.Hour {
		t.Errorf("bad max age: %v", c.CacheMaxAge)
	}
	if !reflect.DeepEqual(c.UpstreamDNS, []string{"8.8.8.8:53", "8.8.4.4:53"}) {
		t.Errorf("bad dns default: %v", c.UpstreamDNS)
	}
	if c.LogFormat != "cachelog" || c.NoFetch || c.CacheRoot != "/data/cache" || c.LogDir != "/data/logs" {
		t.Errorf("bad defaults: %+v", c)
	}
}

func TestLoadOverridesAndErrors(t *testing.T) {
	env := map[string]string{"NOFETCH": "true", "NGINX_LOG_FORMAT": "cachelog-json", "CACHE_SLICE_SIZE": "2m", "CACHE_MAX_PREFETCHES": "0"}
	c, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !c.NoFetch || c.LogFormat != "cachelog-json" || c.SliceSize != 2<<20 || c.MaxPrefetches != 0 {
		t.Errorf("overrides not applied: %+v", c)
	}
	for _, bad := range []map[string]string{
		{"NGINX_LOG_FORMAT": "xml"},
		{"CACHE_DISK_SIZE": "lots"},
		{"CACHE_SLICE_SIZE": "0"},
		{"CACHE_MAX_AGE": "forever"},
		{"CACHE_MAX_PREFETCHES": "-1"},
		{"CACHE_MAX_PREFETCHES": "many"},
	} {
		if _, err := Load(func(k string) string { return bad[k] }); err == nil {
			t.Errorf("expected error for %v", bad)
		}
	}
}

func TestListenerAddrs(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := Load(get(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.SNIAddr != ":443" || c.MetricsAddr != ":8080" {
		t.Fatalf("defaults: %q %q", c.SNIAddr, c.MetricsAddr)
	}
	c, err = Load(get(map[string]string{"SNI_ADDR": "OFF", "METRICS_ADDR": "127.0.0.1:9100"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.SNIAddr != "" || c.MetricsAddr != "127.0.0.1:9100" {
		t.Fatalf("overrides: %q %q", c.SNIAddr, c.MetricsAddr)
	}
}

func TestLoadBypassDomains(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := Load(get(nil))
	if err != nil || c.BypassDomains != "" {
		t.Fatalf("default: %q, %v", c.BypassDomains, err)
	}
	// Entries are validated by the proxy, which logs and skips bad ones, so
	// a typo here never stops startup.
	c, err = Load(get(map[string]string{"BYPASS_DOMAINS": " a.com, *.b.net,bad host "}))
	if err != nil || c.BypassDomains != "a.com, *.b.net,bad host" {
		t.Fatalf("set: %q, %v", c.BypassDomains, err)
	}
}
