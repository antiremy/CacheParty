// Package resolver resolves upstream hostnames through the configured
// UPSTREAM_DNS servers only. The system resolver is never used for the
// lookup itself: on a LAN running lancache-dns it would point CDN names
// straight back at this cache.
package resolver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// Options tunes a Resolver; zero values select defaults.
type Options struct {
	TTL         time.Duration    // cache lifetime of an answer (default 60s)
	DialTimeout time.Duration    // per-connection dial timeout (default 5s)
	Now         func() time.Time // clock, for tests
	HasIPv6     func() bool      // whether IPv6 addresses can be dialed (default: a global IPv6 address is assigned)
}

// Resolver is safe for concurrent use.
type Resolver struct {
	servers []string
	dialer  net.Dialer
	ttl     time.Duration
	now     func() time.Time
	hasV6   func() bool
	rr      atomic.Uint32 // DNS server round-robin
	rot     atomic.Uint32 // address rotation when dialing

	mu    sync.RWMutex
	cache map[string]entry
	sf    singleflight.Group
}

type entry struct {
	addrs   []netip.Addr
	expires time.Time
}

// New creates a Resolver for servers, each "host:port".
//
// The Go resolver still reads resolv.conf for its retry policy and calls
// our Dial hook once per attempt; the hook ignores the address it is given
// and substitutes the next configured server, so retries rotate through
// UPSTREAM_DNS.
func New(servers []string, opts Options) (*Resolver, error) {
	if len(servers) == 0 {
		return nil, errors.New("resolver: no DNS servers configured")
	}
	r := &Resolver{
		servers: servers,
		ttl:     opts.TTL,
		now:     opts.Now,
		cache:   make(map[string]entry),
	}
	if r.ttl <= 0 {
		r.ttl = 60 * time.Second
	}
	if r.now == nil {
		r.now = time.Now
	}
	r.hasV6 = opts.HasIPv6
	if r.hasV6 == nil {
		r.hasV6 = hostHasIPv6
	}
	r.dialer.Timeout = opts.DialTimeout
	if r.dialer.Timeout <= 0 {
		r.dialer.Timeout = 5 * time.Second
	}
	return r, nil
}

// netResolver returns a Go resolver whose attempts walk UPSTREAM_DNS from
// start. Each address family gets its own, so both queries fail over
// together instead of one family's retry landing on a dead server again.
func (r *Resolver) netResolver(start int) *net.Resolver {
	var n atomic.Uint32
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			s := r.servers[(start+int(n.Add(1)-1))%len(r.servers)]
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, network, s)
		},
	}
}

// Lookup returns the IPv4 and IPv6 addresses for host. IP literals are
// returned as-is. On a lookup failure an expired cached answer is preferred over an
// error.
func (r *Resolver) Lookup(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap()
		return []netip.Addr{a}, nil
	}

	r.mu.RLock()
	e, ok := r.cache[host]
	r.mu.RUnlock()
	if ok && r.now().Before(e.expires) {
		return e.addrs, nil
	}

	v, err, _ := r.sf.Do(host, func() (any, error) { return r.query(host) })
	if err != nil {
		if ok {
			return e.addrs, nil // stale beats failure
		}
		return nil, err
	}
	return v.([]netip.Addr), nil
}

func (r *Resolver) query(host string) ([]netip.Addr, error) {
	// Detached from any single caller so one client's disconnect cannot
	// fail the lookup shared by everyone waiting on it.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// The trailing dot makes the name absolute, so resolv.conf search
	// domains are never appended.
	// A and AAAA are asked separately so a failure of one family (a server
	// that mishandles AAAA, say) cannot hide the other's answer.
	start := int(r.rr.Add(1) - 1)
	var res [2][]netip.Addr
	var errs [2]error
	var wg sync.WaitGroup
	for i, network := range [2]string{"ip4", "ip6"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i], errs[i] = r.netResolver(start).LookupNetIP(ctx, network, host+".")
		}()
	}
	wg.Wait()
	var addrs []netip.Addr
	for _, ips := range res {
		for _, ip := range ips {
			addrs = append(addrs, ip.Unmap().WithZone(""))
		}
	}
	if len(addrs) == 0 {
		if err := errors.Join(errs[0], errs[1]); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("resolver: no A or AAAA records for %s", host)
	}
	r.mu.Lock()
	r.cache[host] = entry{addrs: addrs, expires: r.now().Add(r.ttl)}
	r.mu.Unlock()
	return addrs, nil
}

type avoidKey struct{}

// WithAvoid returns a context that makes DialContext try every address of a
// host other than the one listed for it in avoid (host to IP) first. The
// avoided address is still the last resort, so a host with a single address
// keeps working. It is how a retry reaches a different server than the
// attempt that just failed.
func WithAvoid(ctx context.Context, avoid map[string]string) context.Context {
	if len(avoid) == 0 {
		return ctx
	}
	return context.WithValue(ctx, avoidKey{}, avoid)
}

// Avoided returns the address WithAvoid asked to avoid for host, or "".
func Avoided(ctx context.Context, host string) string {
	m, _ := ctx.Value(avoidKey{}).(map[string]string)
	return m[host]
}

// DialContext connects to address ("host:port"), trying each resolved address
// in turn. Addresses rotate within their family, and the families alternate
// starting with IPv4, so a host with a broken IPv6 route costs one failed
// attempt rather than all of them. It satisfies http.Transport.DialContext.
func (r *Resolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("resolver: unsupported network %q", network)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := r.Lookup(ctx, host)
	if err != nil {
		return nil, err
	}
	if !r.hasV6() {
		// No IPv6 of our own: those addresses cannot be reached, so do not
		// spend dial attempts on them.
		network = "tcp4"
	}
	addrs = filterNetwork(addrs, network)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("resolver: no %s addresses for %s", network, host)
	}
	order := interleave(addrs, int(r.rot.Add(1)))
	if ip := Avoided(ctx, host); ip != "" {
		order = avoidLast(order, ip)
	}
	var firstErr error
	for _, a := range order {
		c, err := r.dialer.DialContext(ctx, "tcp", net.JoinHostPort(a.String(), port))
		if err == nil {
			return c, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if ctx.Err() != nil {
			break
		}
	}
	return nil, firstErr
}

// filterNetwork keeps the addresses usable on network ("tcp4" and "tcp6"
// restrict the family; "tcp" keeps everything).
func filterNetwork(addrs []netip.Addr, network string) []netip.Addr {
	if network == "tcp" {
		return addrs
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if a.Is4() == (network == "tcp4") {
			out = append(out, a)
		}
	}
	return out
}

// interleave rotates each family by start and alternates them, IPv4 first.
func interleave(addrs []netip.Addr, start int) []netip.Addr {
	var v4, v6 []netip.Addr
	for _, a := range addrs {
		if a.Is4() {
			v4 = append(v4, a)
		} else {
			v6 = append(v6, a)
		}
	}
	rot := func(l []netip.Addr) []netip.Addr {
		out := make([]netip.Addr, len(l))
		for i := range l {
			out[i] = l[(start+i)%len(l)]
		}
		return out
	}
	v4, v6 = rot(v4), rot(v6)
	out := make([]netip.Addr, 0, len(addrs))
	for i := 0; i < len(v4) || i < len(v6); i++ {
		if i < len(v4) {
			out = append(out, v4[i])
		}
		if i < len(v6) {
			out = append(out, v6[i])
		}
	}
	return out
}

// avoidLast moves the address ip to the back, keeping the rest in order.
func avoidLast(order []netip.Addr, ip string) []netip.Addr {
	out := make([]netip.Addr, 0, len(order))
	var skipped []netip.Addr
	for _, a := range order {
		if a.String() == ip {
			skipped = append(skipped, a)
		} else {
			out = append(out, a)
		}
	}
	return append(out, skipped...)
}

// hostHasIPv6 reports whether an up interface carries a global IPv6 address
// (not loopback, link-local or unique-local).
func hostHasIPv6() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() == nil && ipn.IP.IsGlobalUnicast() && !ipn.IP.IsPrivate() {
				return true
			}
		}
	}
	return false
}
