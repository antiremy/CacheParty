// Package config parses the environment-variable configuration, which uses
// the same names and defaults as lancachenet/monolithic.
package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the fully parsed runtime configuration.
type Config struct {
	CacheDiskSize int64         // CACHE_DISK_SIZE, bytes
	MinFreeDisk   int64         // MIN_FREE_DISK, bytes
	CacheMaxAge   time.Duration // CACHE_MAX_AGE
	SliceSize     int64         // CACHE_SLICE_SIZE, bytes
	MaxPrefetches int           // CACHE_MAX_PREFETCHES; 0 disables read-ahead
	RateLimit     int64         // UPSTREAM_RATE_LIMIT, bytes per second; 0 is unlimited
	UpstreamDNS   []string      // UPSTREAM_DNS, each "host:port"
	BypassDomains string        // BYPASS_DOMAINS as written; the proxy parses it (bad entries are logged, not fatal)

	DomainsRepo   string // CACHE_DOMAINS_REPO
	DomainsBranch string // CACHE_DOMAINS_BRANCH
	NoFetch       bool   // NOFETCH
	LogFormat     string // NGINX_LOG_FORMAT: cachelog | cachelog-json

	CacheRoot  string // CACHE_ROOT
	LogDir     string // LOG_DIR
	DomainsDir string // DOMAINS_DIR (where cache-domains lives; same default as the original)

	HTTPAddr    string // HTTP_ADDR (listener for the caching proxy)
	SNIAddr     string // SNI_ADDR (TLS passthrough listener); "" disables it
	MetricsAddr string // METRICS_ADDR (/metrics, /healthz and the dashboard); "" disables it

	SettingsFile      string // SETTINGS_FILE: values saved from the dashboard
	StatsFile         string // STATS_FILE: dashboard traffic totals kept across restarts; "" disables it
	DNSSyncFile       string // DNS_SYNC_FILE: DNS servers the dashboard syncs cache-domains to; "" disables it
	DashboardPassword string // DASHBOARD_PASSWORD; "" makes the dashboard read-only
}

// Defaults holds the value each variable takes when it is unset.
var Defaults = map[string]string{
	"CACHE_DISK_SIZE":      "1000g",
	"MIN_FREE_DISK":        "10g",
	"CACHE_MAX_AGE":        "3560d",
	"CACHE_SLICE_SIZE":     "1m",
	"CACHE_MAX_PREFETCHES": "64",
	"UPSTREAM_RATE_LIMIT":  "0",
	"UPSTREAM_DNS":         "8.8.8.8 8.8.4.4",
	"BYPASS_DOMAINS":       "",
	"CACHE_DOMAINS_REPO":   "https://github.com/uklans/cache-domains.git",
	"CACHE_DOMAINS_BRANCH": "master",
	"NOFETCH":              "false",
	"NGINX_LOG_FORMAT":     "cachelog",
	"CACHE_ROOT":           "/data/cache",
	"LOG_DIR":              "/data/logs",
	"DOMAINS_DIR":          "/data/cachedomains",
	"HTTP_ADDR":            ":80",
	"SNI_ADDR":             ":443",
	"METRICS_ADDR":         ":8080",
	"SETTINGS_FILE":        "/data/settings.json",
	"STATS_FILE":           "/data/stats.json",
	"DNS_SYNC_FILE":        "/data/dns-sync.json",
	"DASHBOARD_PASSWORD":   "",
}

// Load reads the configuration through getenv (os.Getenv in production).
func Load(getenv func(string) string) (*Config, error) {
	get := func(key string) string { return Raw(getenv, key) }
	var c Config
	var err error

	if c.CacheDiskSize, err = ParseSize(get("CACHE_DISK_SIZE")); err != nil {
		return nil, fmt.Errorf("CACHE_DISK_SIZE: %w", err)
	}
	if c.MinFreeDisk, err = ParseSize(get("MIN_FREE_DISK")); err != nil {
		return nil, fmt.Errorf("MIN_FREE_DISK: %w", err)
	}
	if c.CacheMaxAge, err = ParseDuration(get("CACHE_MAX_AGE")); err != nil {
		return nil, fmt.Errorf("CACHE_MAX_AGE: %w", err)
	}
	if c.SliceSize, err = ParseSize(get("CACHE_SLICE_SIZE")); err != nil {
		return nil, fmt.Errorf("CACHE_SLICE_SIZE: %w", err)
	}
	if c.SliceSize <= 0 || c.CacheDiskSize <= 0 {
		return nil, fmt.Errorf("CACHE_SLICE_SIZE and CACHE_DISK_SIZE must be positive")
	}
	if c.MaxPrefetches, err = strconv.Atoi(get("CACHE_MAX_PREFETCHES")); err != nil || c.MaxPrefetches < 0 {
		return nil, fmt.Errorf("CACHE_MAX_PREFETCHES: must be a non-negative integer")
	}
	if c.RateLimit, err = ParseRate(get("UPSTREAM_RATE_LIMIT")); err != nil {
		return nil, fmt.Errorf("UPSTREAM_RATE_LIMIT: %w", err)
	}
	if c.UpstreamDNS, err = ParseDNSServers(get("UPSTREAM_DNS")); err != nil {
		return nil, fmt.Errorf("UPSTREAM_DNS: %w", err)
	}

	c.BypassDomains = get("BYPASS_DOMAINS")

	c.DomainsRepo = get("CACHE_DOMAINS_REPO")
	c.DomainsBranch = get("CACHE_DOMAINS_BRANCH")
	c.NoFetch = parseBool(get("NOFETCH"))
	c.LogFormat = get("NGINX_LOG_FORMAT")
	if c.LogFormat != "cachelog" && c.LogFormat != "cachelog-json" {
		return nil, fmt.Errorf("NGINX_LOG_FORMAT: must be cachelog or cachelog-json, got %q", c.LogFormat)
	}
	c.CacheRoot = get("CACHE_ROOT")
	c.LogDir = get("LOG_DIR")
	c.DomainsDir = get("DOMAINS_DIR")
	c.HTTPAddr = get("HTTP_ADDR")
	c.SNIAddr = listenAddr(get("SNI_ADDR"))
	c.MetricsAddr = listenAddr(get("METRICS_ADDR"))
	c.SettingsFile = get("SETTINGS_FILE")
	c.StatsFile = listenAddr(get("STATS_FILE")) // "off" disables it
	c.DNSSyncFile = listenAddr(get("DNS_SYNC_FILE"))
	c.DashboardPassword = get("DASHBOARD_PASSWORD")
	return &c, nil
}

// Raw returns key's value as written: from getenv, else its default.
func Raw(getenv func(string) string, key string) string {
	if v := strings.TrimSpace(getenv(key)); v != "" {
		return v
	}
	return Defaults[key]
}

func parseBool(s string) bool {
	switch strings.ToLower(s) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// listenAddr maps "off" (any case) to "", which disables the listener.
func listenAddr(s string) string {
	if strings.EqualFold(s, "off") {
		return ""
	}
	return s
}
