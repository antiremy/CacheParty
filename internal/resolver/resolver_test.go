package resolver

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"cacheparty/internal/testutil"
)

func TestLookupCachesAndExpires(t *testing.T) {
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "10.1.2.3"})
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	r, err := New([]string{dns.Addr}, Options{TTL: time.Minute, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		got, err := r.Lookup(ctx, "cdn.test")
		if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("10.1.2.3") {
			t.Fatalf("lookup %d = %v, %v", i, got, err)
		}
	}
	if q := dns.Queries.Load(); q != 2 {
		t.Fatalf("queries = %d, want 2 (A+AAAA, then cached)", q)
	}

	mu.Lock()
	now = now.Add(2 * time.Minute)
	mu.Unlock()
	dns.Set("cdn.test", "10.9.9.9")
	got, _ := r.Lookup(ctx, "cdn.test")
	if got[0] != netip.MustParseAddr("10.9.9.9") || dns.Queries.Load() != 4 {
		t.Fatalf("after expiry got %v queries=%d", got, dns.Queries.Load())
	}
}

func TestLookupLiteralsAndNXDOMAIN(t *testing.T) {
	dns := testutil.NewFakeDNS(t, nil)
	r, _ := New([]string{dns.Addr}, Options{})
	ctx := context.Background()
	if got, err := r.Lookup(ctx, "192.0.2.7"); err != nil || got[0].String() != "192.0.2.7" {
		t.Errorf("literal: %v %v", got, err)
	}
	if got, err := r.Lookup(ctx, "2001:db8::1"); err != nil || got[0].String() != "2001:db8::1" {
		t.Errorf("IPv6 literal: %v %v", got, err)
	}
	if _, err := r.Lookup(ctx, "nope.test"); err == nil {
		t.Error("NXDOMAIN should error")
	}
}

func TestLookupServesStaleOnFailure(t *testing.T) {
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "10.1.2.3"})
	now := time.Unix(1000, 0)
	var mu sync.Mutex
	r, _ := New([]string{dns.Addr}, Options{TTL: time.Second, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	if _, err := r.Lookup(context.Background(), "cdn.test"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(time.Hour)
	mu.Unlock()
	dns.Delete("cdn.test")
	got, err := r.Lookup(context.Background(), "cdn.test")
	if err != nil || got[0] != netip.MustParseAddr("10.1.2.3") {
		t.Fatalf("want stale answer, got %v, %v", got, err)
	}
}

func TestLookupCoalescesConcurrentQueries(t *testing.T) {
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "10.1.2.3"})
	r, _ := New([]string{dns.Addr}, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Lookup(context.Background(), "cdn.test"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if q := dns.Queries.Load(); q != 2 {
		t.Fatalf("queries = %d, want 2", q)
	}
}

func TestDialContext(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "127.0.0.1"})
	r, _ := New([]string{dns.Addr}, Options{})

	c, err := r.DialContext(context.Background(), "tcp", net.JoinHostPort("cdn.test", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := r.DialContext(context.Background(), "udp", "cdn.test:80"); err == nil {
		t.Error("udp should be rejected")
	}
	if _, err := r.DialContext(context.Background(), "tcp", "missing.test:80"); err == nil {
		t.Error("NXDOMAIN should fail the dial")
	}
}

func TestServerFailover(t *testing.T) {
	dead, _ := net.ListenPacket("udp4", "127.0.0.1:0") // never answers
	defer dead.Close()
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "10.1.2.3"})
	r, _ := New([]string{dead.LocalAddr().String(), dns.Addr}, Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := r.Lookup(ctx, "cdn.test"); err != nil {
		t.Fatalf("failover to second server failed: %v", err)
	}
}

func TestAvoidLast(t *testing.T) {
	a, b, c := netip.MustParseAddr("10.0.0.1"), netip.MustParseAddr("10.0.0.2"), netip.MustParseAddr("10.0.0.3")
	got := avoidLast([]netip.Addr{b, a, c}, "10.0.0.1")
	if len(got) != 3 || got[0] != b || got[1] != c || got[2] != a {
		t.Errorf("got %v", got)
	}
	if got := avoidLast([]netip.Addr{a}, "10.0.0.1"); len(got) != 1 || got[0] != a {
		t.Errorf("sole address must stay: %v", got)
	}
	if got := avoidLast([]netip.Addr{a, b}, "10.9.9.9"); got[0] != a || got[1] != b {
		t.Errorf("unknown ip must not reorder: %v", got)
	}
}

func TestDialAvoidsAddressFromContext(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	r, err := New([]string{"127.0.0.1:1"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// A literal has a single address, so avoiding it still connects.
	ctx := WithAvoid(context.Background(), map[string]string{"127.0.0.1": "127.0.0.1"})
	c, err := r.DialContext(ctx, "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestLookupReturnsAAAA(t *testing.T) {
	dns := testutil.NewFakeDNS(t, map[string]string{"v6.test": "2001:db8::5"})
	r, _ := New([]string{dns.Addr}, Options{})
	got, err := r.Lookup(context.Background(), "v6.test")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("2001:db8::5") {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestInterleaveAlternatesFamiliesIPv4First(t *testing.T) {
	a := []netip.Addr{
		netip.MustParseAddr("2001:db8::1"), netip.MustParseAddr("2001:db8::2"),
		netip.MustParseAddr("10.0.0.1"),
	}
	got := interleave(a, 0)
	want := []string{"10.0.0.1", "2001:db8::1", "2001:db8::2"}
	for i, w := range want {
		if got[i].String() != w {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if got := filterNetwork(a, "tcp6"); len(got) != 2 {
		t.Errorf("tcp6 filter: %v", got)
	}
	if got := filterNetwork(a, "tcp4"); len(got) != 1 {
		t.Errorf("tcp4 filter: %v", got)
	}
}

func TestDialIPv6(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skip("no IPv6 loopback:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	dns := testutil.NewFakeDNS(t, map[string]string{"v6.test": "::1"})
	r, _ := New([]string{dns.Addr}, Options{HasIPv6: func() bool { return true }})
	c, err := r.DialContext(context.Background(), "tcp", net.JoinHostPort("v6.test", port))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if _, err := r.DialContext(context.Background(), "tcp4", net.JoinHostPort("v6.test", port)); err == nil {
		t.Error("tcp4 must not dial an IPv6-only host")
	}
}

func TestDialSkipsIPv6WithoutLocalIPv6(t *testing.T) {
	dns := testutil.NewFakeDNS(t, map[string]string{"v6.test": "::1"})
	r, _ := New([]string{dns.Addr}, Options{HasIPv6: func() bool { return false }})
	if _, err := r.DialContext(context.Background(), "tcp", "v6.test:80"); err == nil || !strings.Contains(err.Error(), "no tcp4 addresses") {
		t.Fatalf("err = %v, want no tcp4 addresses", err)
	}
}
