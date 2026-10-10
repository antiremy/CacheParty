package config

import (
	"fmt"
	"math"
	"net"
	"strings"
	"time"
)

// ParseSize parses an nginx-style size: an integer with an optional k, m, g
// or t suffix (case-insensitive, powers of 1024). A bare number is bytes.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	n, rest, err := leadingInt(s)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	mult := int64(1)
	switch strings.ToLower(rest) {
	case "":
	case "k":
		mult = 1 << 10
	case "m":
		mult = 1 << 20
	case "g":
		mult = 1 << 30
	case "t":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("invalid size unit in %q", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return n * mult, nil
}

// durationUnits uses nginx's unit names. They are case-sensitive because
// "m" is minutes and "M" is months.
var durationUnits = []struct {
	name string
	d    time.Duration
}{
	{"ms", time.Millisecond},
	{"s", time.Second},
	{"m", time.Minute},
	{"h", time.Hour},
	{"d", 24 * time.Hour},
	{"w", 7 * 24 * time.Hour},
	{"M", 30 * 24 * time.Hour},
	{"y", 365 * 24 * time.Hour},
}

// ParseRate parses a network speed in bits per second, the way connection
// plans are sold: an integer with an optional k, m or g suffix (powers of
// 1000), optionally followed by "bit", e.g. "500m" or "1gbit". It returns
// bytes per second; 0 means no limit.
func ParseRate(s string) (int64, error) {
	s = strings.TrimSpace(s)
	n, rest, err := leadingInt(s)
	if err != nil {
		return 0, fmt.Errorf("invalid rate %q", s)
	}
	mult := int64(1)
	switch strings.TrimSuffix(strings.ToLower(rest), "bit") {
	case "":
	case "k":
		mult = 1e3
	case "m":
		mult = 1e6
	case "g":
		mult = 1e9
	default:
		return 0, fmt.Errorf("invalid rate unit in %q (use k, m or g, in bits per second)", s)
	}
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("rate %q overflows", s)
	}
	if n > 0 && n*mult < 8 {
		return 0, fmt.Errorf("rate %q is below one byte per second", s)
	}
	return n * mult / 8, nil
}

// ParseDuration parses an nginx-style time such as "3560d" or "1h30m".
// A bare number is seconds.
func ParseDuration(s string) (time.Duration, error) {
	orig := strings.TrimSpace(s)
	s = orig
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	var total time.Duration
	for s != "" {
		n, rest, err := leadingInt(s)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", orig)
		}
		unit := time.Second
		if rest != "" {
			found := false
			for _, u := range durationUnits {
				// Prefer the longest unit name ("ms" before "m").
				if strings.HasPrefix(rest, u.name) && (!found || len(u.name) > 1) {
					unit, s, found = u.d, rest[len(u.name):], true
				}
			}
			if !found {
				return 0, fmt.Errorf("invalid duration unit in %q", orig)
			}
		} else {
			s = ""
		}
		if n > int64(math.MaxInt64/int64(unit)) {
			return 0, fmt.Errorf("duration %q overflows", orig)
		}
		total += time.Duration(n) * unit
	}
	return total, nil
}

// ParseDNSServers splits a whitespace/comma separated resolver list and
// ensures every entry has a port (default 53).
func ParseDNSServers(s string) ([]string, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == ',' || r == '\t' })
	if len(fields) == 0 {
		return nil, fmt.Errorf("no resolvers given")
	}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if _, _, err := net.SplitHostPort(f); err != nil {
			f = net.JoinHostPort(strings.Trim(f, "[]"), "53")
		}
		out = append(out, f)
	}
	return out, nil
}

// leadingInt splits "123abc" into (123, "abc").
func leadingInt(s string) (int64, string, error) {
	i := 0
	var n int64
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		d := int64(s[i] - '0')
		if n > (math.MaxInt64-d)/10 {
			return 0, "", fmt.Errorf("overflow")
		}
		n = n*10 + d
	}
	if i == 0 {
		return 0, "", fmt.Errorf("no digits")
	}
	return n, s[i:], nil
}
