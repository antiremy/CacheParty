package proxy

import (
	"bytes"
	"net/http/httptest"
	"strings"
	"testing"

	"cacheparty/internal/evict"
	"cacheparty/internal/testutil"
)

func TestBypassRules(t *testing.T) {
	cases := []struct {
		uri  string
		want bool
	}{
		{"/depot/1/chunk/abc", false},
		{"/x?nocache=1", true},
		{"/x?a=b&nocache=1", true},
		{"/x?nocache=1&a=b", true},
		{"/x?nocache=0", false},
		{"/x?nocache=10", false},
		{"/x?nocache", false},
		{"/x?xnocache=1", false},
		{"/latest64", true},
		{"/latest64/manifest", true},
		{"/x/latest64", false},
		{"/msdownload/update/v3/static/trustedr/en/authrootstl.cab", true},
		{"/a/AuthRootSTL.CAB", true},
		{"/a/pinrulesstl.cab", true},
		{"/a/disallowedcertstl.cab", true},
		{"/a/disallowedcertstl.cab?x=1", true},
		{"/a/other.cab", false},
		{"/a/authrootstl.cab.bak", false},
		{"/RiotGamesApi/releaselisting_NA", true},
		{"/x/releaselisting_EUW/y", true},
		{"/releaseListing_NA", false},
		{"/a/b/lol.version", true},
		{"/a/b/lol.versions", false},
		{"/server-status", true},
		{"/server-status/", false},
		{"/server-status?auto", true},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", c.uri, nil)
		if got := bypass(r); got != c.want {
			t.Errorf("bypass(%q) = %v, want %v", c.uri, got, c.want)
		}
	}
}

func TestBypassedRequestsAreNeverCached(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500)
	paths := []string{"/game.bin?nocache=1", "/latest64", "/trust/authrootstl.cab", "/x/lol.version", "/server-status"}
	for _, p := range paths {
		e.origin.Set(requestPath(p), &testutil.Obj{Body: body})
	}
	for _, p := range paths {
		for i := 0; i < 2; i++ {
			resp, got := e.get(t, p, nil)
			eq(t, p+" status", resp.StatusCode, 200)
			eq(t, p+" cache status", resp.Header.Get("X-Upstream-Cache-Status"), "BYPASS")
			eq(t, p+" body length", len(got), len(body))
		}
		eq(t, p+" origin requests", e.origin.Count(requestPath(p)), 2)
	}
	eq(t, "chunks stored", e.chunks(), int64(0))
}

func TestBypassForwardsRangeUntouched(t *testing.T) {
	e := newCachedEnv(t)
	e.origin.Set("/latest64", &testutil.Obj{Body: testutil.Pattern(2500)})
	resp, got := e.get(t, "/latest64", map[string]string{"Range": "bytes=10-19"})
	eq(t, "status", resp.StatusCode, 206)
	eq(t, "length", len(got), 10)
	eq(t, "origin saw", e.origin.Ranges("/latest64")[0], "bytes=10-19")
}

func TestPartlyEvictedObjectRefetchesOnlyMissingChunks(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(3000) // 3 chunks
	e.origin.Set("/g.bin", &testutil.Obj{Body: body})
	e.get(t, "/g.bin", nil)
	eq(t, "chunks stored", e.chunks(), int64(3))

	// 3000 bytes held, limit 2000: the sweep frees the oldest chunk(s).
	evict.New(evict.Config{Store: e.st, SliceSize: testSlice, MaxBytes: 2000}).Sweep()
	left := e.chunks()
	if left == 3 || left == 0 {
		t.Fatalf("chunks after sweep = %d, want some evicted but not all", left)
	}
	before := len(e.origin.Requests())
	resp, got := e.get(t, "/g.bin", nil)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch after eviction")
	}
	eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status") != "", true)
	eq(t, "refetched chunks", len(e.origin.Requests())-before, int(3-left))
	eq(t, "chunks stored again", e.chunks(), int64(3))
}

func TestBypassDomainMatching(t *testing.T) {
	set, bad := parseBypassDomains(" Example.COM , ,*.cdn.Other.net,, bad host,example.org:8080,a/b,*.,*.*.x.com,.lead.com ")
	eq(t, "invalid entries", strings.Join(bad, "|"), "bad host|example.org:8080|a/b|*.|*.*.x.com|.lead.com")
	cases := []struct {
		host string
		want bool
	}{
		{"example.com", true},
		{"EXAMPLE.com", true},
		{"example.com:8080", true},
		{"example.com.", true},
		{"cdn.example.com", true},
		{"a.b.example.com:443", true},
		{"notexample.com", false},
		{"example.com.evil.net", false},
		{"example.org", false},
		{"cdn.other.net", false}, // a wildcard excludes the apex
		{"y.cdn.other.net", true},
		{"x.cdn.other.net", true},
		{"other.net", false},
		{"cdn.other.net.evil.com", false},
		{"notcdn.other.net", false},
		{"", false},
	}
	for _, c := range cases {
		if got := set.match(c.host); got != c.want {
			t.Errorf("match(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	var none *bypassSet
	eq(t, "nil set", none.match("example.com"), false)
	empty, bad := parseBypassDomains("")
	eq(t, "empty set", empty.match("example.com"), false)
	eq(t, "empty list invalid", len(bad), 0)
	v6, _ := parseBypassDomains("example.com")
	eq(t, "ipv6 literal", v6.match("[::1]:80"), false)
}

func TestBypassDomainsSkipCacheOnly(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500)
	e.origin.Set("/game.bin", &testutil.Obj{Body: body})

	e.h.SetBypassDomains("cdn.test")
	for i := 0; i < 2; i++ {
		resp, got := e.get(t, "/game.bin", nil)
		eq(t, "bypassed status", resp.Header.Get("X-Upstream-Cache-Status"), "BYPASS")
		eq(t, "bypassed body", len(got), len(body))
	}
	eq(t, "origin requests", e.origin.Count("/game.bin"), 2)
	eq(t, "chunks stored", e.chunks(), int64(0))

	// Ranges go to the origin untouched.
	resp, got := e.get(t, "/game.bin", map[string]string{"Range": "bytes=10-19"})
	eq(t, "range status", resp.StatusCode, 206)
	eq(t, "range length", len(got), 10)
	eq(t, "origin saw", e.origin.Ranges("/game.bin")[2], "bytes=10-19")
	eq(t, "chunks stored after range", e.chunks(), int64(0))

	// Removing the domain at runtime puts the host back in the cache.
	e.h.SetBypassDomains("")
	resp, _ = e.get(t, "/game.bin", nil)
	eq(t, "cached status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
	resp, _ = e.get(t, "/game.bin", nil)
	eq(t, "cached status", resp.Header.Get("X-Upstream-Cache-Status"), "HIT")
	eq(t, "chunks stored", e.chunks() > 0, true)

	// A cached object is not served to a request that is now bypassed.
	e.h.SetBypassDomains("test")
	before := e.origin.Count("/game.bin")
	resp, _ = e.get(t, "/game.bin", nil)
	eq(t, "bypassed after cached", resp.Header.Get("X-Upstream-Cache-Status"), "BYPASS")
	eq(t, "went to origin", e.origin.Count("/game.bin"), before+1)

	// A lookalike domain does not bypass.
	e.h.SetBypassDomains("cdn.te, dn.test")
	resp, _ = e.get(t, "/game.bin", nil)
	eq(t, "lookalike", resp.Header.Get("X-Upstream-Cache-Status"), "HIT")
}
