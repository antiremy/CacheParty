// Package domains maps request hostnames to cache identifiers using the
// uklans/cache-domains lists.
//
// Lookup is a hash-map probe for exact entries plus a walk of a
// reversed-label trie for "*.example.com" wildcards. Neither allocates for
// lower-case input.
package domains

import (
	"fmt"
	"sort"
	"strings"
)

const steamUAPrefix = "Valve/Steam HTTP Client 1.0"

// Matcher is immutable once built; Add must not race with Lookup.
type Matcher struct {
	exact map[string]string
	root  *node
}

type node struct {
	children map[string]*node
	wild     string // identifier for "*.<labels down to this node>"
	hasWild  bool
}

// NewMatcher returns an empty matcher.
func NewMatcher() *Matcher {
	return &Matcher{exact: make(map[string]string), root: &node{}}
}

// Add registers a hostname pattern (exact or "*.suffix") under name. The
// first registration of a pattern wins, which keeps results deterministic
// when two lists claim the same host.
func (m *Matcher) Add(name, pattern string) error {
	pattern = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(pattern), "."))
	if pattern == "" {
		return fmt.Errorf("empty pattern")
	}
	if rest, ok := strings.CutPrefix(pattern, "*."); ok {
		if rest == "" || strings.Contains(rest, "*") {
			return fmt.Errorf("unsupported wildcard %q", pattern)
		}
		n := m.root
		for end := len(rest); end > 0; {
			i := strings.LastIndexByte(rest[:end], '.')
			label := rest[i+1 : end]
			if n.children == nil {
				n.children = make(map[string]*node)
			}
			child := n.children[label]
			if child == nil {
				child = &node{}
				n.children[label] = child
			}
			n = child
			end = i
		}
		if !n.hasWild {
			n.wild, n.hasWild = name, true
		}
		return nil
	}
	if strings.Contains(pattern, "*") {
		return fmt.Errorf("unsupported wildcard %q", pattern)
	}
	if _, dup := m.exact[pattern]; !dup {
		m.exact[pattern] = name
	}
	return nil
}

// Lookup returns the cache identifier for host (no port). An exact entry
// beats a wildcard; among wildcards the most specific wins. "*.x.com" matches
// any subdomain of x.com but not x.com itself.
func (m *Matcher) Lookup(host string) (string, bool) {
	host = normalize(host)
	if name, ok := m.exact[host]; ok {
		return name, true
	}
	var best string
	var found bool
	n := m.root
	for end := len(host); end > 0; {
		i := strings.LastIndexByte(host[:end], '.')
		n = n.children[host[i+1:end]]
		if n == nil {
			break
		}
		if i <= 0 {
			break // no label left of this one, so a wildcard cannot apply
		}
		if n.hasWild {
			best, found = n.wild, true
		}
		end = i
	}
	return best, found
}

// Identify returns the cache identifier for a request: "steam" for the
// Steam client, the matching list name for a known host, else the host
// itself (lower-cased, without port).
func (m *Matcher) Identify(hostport, userAgent string) string {
	if strings.HasPrefix(userAgent, steamUAPrefix) {
		return "steam"
	}
	host := normalize(StripPort(hostport))
	if name, ok := m.Lookup(host); ok {
		return name
	}
	return host
}

// StripPort removes an optional :port (and IPv6 brackets) from a Host value.
func StripPort(h string) string {
	if strings.HasPrefix(h, "[") {
		if i := strings.IndexByte(h, ']'); i > 0 {
			return h[1:i]
		}
		return h
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && strings.IndexByte(h, ':') == i {
		return h[:i]
	}
	return h
}

func normalize(h string) string {
	h = strings.TrimSuffix(h, ".")
	for i := 0; i < len(h); i++ {
		if c := h[i]; c >= 'A' && c <= 'Z' {
			return strings.ToLower(h)
		}
	}
	return h
}

// Len reports the number of registered patterns.
func (m *Matcher) Len() int {
	return len(m.exact) + countWild(m.root)
}

func countWild(n *node) int {
	c := 0
	if n.hasWild {
		c++
	}
	for _, ch := range n.children {
		c += countWild(ch)
	}
	return c
}

// Pattern is one registered hostname pattern and the list it came from.
type Pattern struct {
	Service string
	Pattern string // "host" or "*.suffix"
}

// Patterns lists every registered pattern, sorted by service then pattern.
func (m *Matcher) Patterns() []Pattern {
	var out []Pattern
	for host, name := range m.exact {
		out = append(out, Pattern{name, host})
	}
	var walk func(n *node, suffix string)
	walk = func(n *node, suffix string) {
		if n.hasWild {
			out = append(out, Pattern{n.wild, "*." + suffix})
		}
		for label, ch := range n.children {
			if suffix == "" {
				walk(ch, label)
			} else {
				walk(ch, label+"."+suffix)
			}
		}
	}
	walk(m.root, "")
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return out[i].Pattern < out[j].Pattern
	})
	return out
}
