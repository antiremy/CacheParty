package proxy

import (
	"net"
	"net/http"
	"strings"
)

// bypassSuffixes are matched case-insensitively against the end of the path
// (Windows Update certificate trust lists, which must never be stale).
var bypassSuffixes = []string{"authrootstl.cab", "pinrulesstl.cab", "disallowedcertstl.cab"}

// bypass reports whether a request must skip the cache and be proxied
// straight to the origin, as the nginx lancache configuration does.
//
//   - a query argument nocache=1
//   - /latest64* (Guild Wars 2 manifest)
//   - WSUS trust-list .cab files
//   - releaselisting_ anywhere in the path, or a .version suffix (League of Legends)
//   - exactly /server-status
func bypass(r *http.Request) bool {
	if hasNoCache(r.URL.RawQuery) {
		return true
	}
	p := r.URL.Path
	switch {
	case p == "/server-status", strings.HasPrefix(p, "/latest64"),
		strings.Contains(p, "releaselisting_"), strings.HasSuffix(p, ".version"):
		return true
	}
	for _, s := range bypassSuffixes {
		if len(p) >= len(s) && strings.EqualFold(p[len(p)-len(s):], s) {
			return true
		}
	}
	return false
}

// hasNoCache reports whether the raw query has an argument nocache=1. Like
// nginx's $arg_nocache it looks at the first nocache argument only.
func hasNoCache(q string) bool {
	for q != "" {
		var kv string
		kv, q, _ = strings.Cut(q, "&")
		if k, v, _ := strings.Cut(kv, "="); k == "nocache" {
			return v == "1"
		}
	}
	return false
}

// bypassSet holds the BYPASS_DOMAINS entries. A nil set matches nothing.
type bypassSet struct {
	exact    map[string]bool // "example.com": the host itself and its subdomains
	suffixes []string        // ".example.com" from "*.example.com": subdomains only
}

// parseBypassDomains parses a comma-separated list of domains. An entry
// "example.com" covers example.com and every subdomain of it; "*.example.com"
// covers the subdomains only. Entries are case-insensitive and trimmed; empty
// ones are skipped and malformed ones (ports, paths, stray wildcards, bad
// characters) are returned in bad.
func parseBypassDomains(list string) (set *bypassSet, bad []string) {
	set = &bypassSet{exact: map[string]bool{}}
	for _, e := range strings.Split(list, ",") {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		host, wild := strings.CutPrefix(strings.ToLower(e), "*.")
		if !validBypassHost(host) {
			bad = append(bad, e)
		} else if wild {
			set.suffixes = append(set.suffixes, "."+host)
		} else {
			set.exact[host] = true
		}
	}
	return set, bad
}

// validBypassHost reports whether s is a plain DNS name: dot-separated
// labels of letters, digits, '-' and '_'.
func validBypassHost(s string) bool {
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

// match reports whether a request Host (any case, with or without a port)
// is covered by the set.
func (s *bypassSet) match(host string) bool {
	if s == nil || (len(s.exact) == 0 && len(s.suffixes) == 0) {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	for h := host; h != ""; {
		if s.exact[h] {
			return true
		}
		_, rest, ok := strings.Cut(h, ".")
		if !ok {
			break
		}
		h = rest
	}
	for _, suf := range s.suffixes {
		if strings.HasSuffix(host, suf) {
			return true
		}
	}
	return false
}
