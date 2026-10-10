package sni

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"cacheparty/internal/metrics"
)

// fakeResolver resolves every name to addrs and dials every name at target.
type fakeResolver struct {
	addrs  []netip.Addr
	target string
	mu     sync.Mutex
	dialed []string
}

func (f *fakeResolver) Lookup(context.Context, string) ([]netip.Addr, error) { return f.addrs, nil }
func (f *fakeResolver) DialContext(ctx context.Context, _, address string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, address)
	f.mu.Unlock()
	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.target)
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	addr  string
	fr    *fakeResolver
	log   *syncBuf
	conns *metrics.Vec
	sent  *metrics.Counter
	doms  sync.Map // server name -> *[2]metrics.Counter
	pool  *x509.CertPool
	seen  chan string // SNI values seen by the origin
	srv   *Server
}

func setup(t *testing.T) *env {
	t.Helper()
	e := &env{log: &syncBuf{}, conns: &metrics.Vec{}, sent: &metrics.Counter{}, seen: make(chan string, 8)}

	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello from origin")
	}))
	origin.TLS = &tls.Config{GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		e.seen <- h.ServerName
		return nil, nil
	}}
	origin.StartTLS()
	t.Cleanup(origin.Close)
	e.pool = x509.NewCertPool()
	e.pool.AddCert(origin.Certificate())

	e.fr = &fakeResolver{addrs: []netip.Addr{netip.MustParseAddr("203.0.113.9")}, target: origin.Listener.Addr().String()}
	e.srv = New(Config{
		Resolver: e.fr, Access: e.log, IsLocal: func(netip.Addr) bool { return false },
		Conns: e.conns, BytesSent: e.sent,
		Domain: func(name string) (sent, received *metrics.Counter) {
			c, _ := e.doms.LoadOrStore(name, new([2]metrics.Counter))
			return &c.(*[2]metrics.Counter)[0], &c.(*[2]metrics.Counter)[1]
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e.addr = ln.Addr().String()
	go e.srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e.srv.Shutdown(ctx)
	})
	return e
}

func (e *env) get(t *testing.T, sniName string) string {
	t.Helper()
	// The origin's certificate is for example.com / 127.0.0.1, so verify
	// against a name it has but send the SNI under test.
	tr := &http.Transport{
		DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, e.addr)
		},
		TLSClientConfig: &tls.Config{RootCAs: e.pool, ServerName: sniName, InsecureSkipVerify: true},
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr}).Get("https://" + sniName + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func waitLog(t *testing.T, e *env, want string) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s := e.log.String(); strings.Contains(s, want) {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("log never contained %q; have %q", want, e.log.String())
	return ""
}

func TestPassthrough(t *testing.T) {
	e := setup(t)
	if got := e.get(t, "Cdn.Example.com"); got != "hello from origin" {
		t.Fatalf("body = %q", got)
	}
	if sni := <-e.seen; sni != "Cdn.Example.com" {
		t.Fatalf("origin saw SNI %q: the hello must be forwarded untouched", sni)
	}
	e.fr.mu.Lock()
	dialed := e.fr.dialed
	e.fr.mu.Unlock()
	if len(dialed) != 1 || dialed[0] != "cdn.example.com:443" {
		t.Fatalf("dialed %v", dialed)
	}
	line := waitLog(t, e, `"cdn.example.com"`)
	if !strings.Contains(line, " TCP 200 ") {
		t.Fatalf("log line: %q", line)
	}
	if e.conns.With("ok").Value() != 1 || e.sent.Value() == 0 {
		t.Fatalf("metrics: ok=%d sent=%d", e.conns.With("ok").Value(), e.sent.Value())
	}
	c, ok := e.doms.Load("cdn.example.com")
	if !ok {
		t.Fatal("no counters for cdn.example.com")
	}
	d := c.(*[2]metrics.Counter)
	if d[0].Value() != e.sent.Value() || d[1].Value() == 0 {
		t.Fatalf("domain sent=%d received=%d, total sent=%d", d[0].Value(), d[1].Value(), e.sent.Value())
	}
}

func TestCopyCountingReportsAsItGoes(t *testing.T) {
	src := bytes.Repeat([]byte("x"), 2*countEvery+5)
	var dst bytes.Buffer
	var adds []int64
	if err := copyCounting(&dst, bytes.NewReader(src), func(n int64) { adds = append(adds, n) }); err != nil {
		t.Fatal(err)
	}
	if dst.Len() != len(src) {
		t.Fatalf("copied %d of %d", dst.Len(), len(src))
	}
	if want := []int64{countEvery, countEvery, 5}; fmt.Sprint(adds) != fmt.Sprint(want) {
		t.Fatalf("adds = %v, want %v", adds, want)
	}
}

func TestNoSNIIsRejected(t *testing.T) {
	e := setup(t)
	c, err := net.Dial("tcp", e.addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")) // not TLS at all
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if n, _ := io.Copy(io.Discard, c); n != 0 {
		t.Fatalf("server answered a non-TLS client with %d bytes", n)
	}
	waitLog(t, e, ` TCP 400 `)
	if len(e.fr.dialed) != 0 {
		t.Fatalf("dialed %v for a request with no SNI", e.fr.dialed)
	}
}

func TestIPLiteralSNIIsRejected(t *testing.T) {
	if got := normalize("192.0.2.1"); got != "" {
		t.Fatalf("IP literal accepted: %q", got)
	}
	if got := normalize("a b.com"); got != "" {
		t.Fatalf("junk accepted: %q", got)
	}
	if got := normalize("Foo.COM."); got != "foo.com" {
		t.Fatalf("normalize = %q", got)
	}
}

func TestLoopIsRefused(t *testing.T) {
	e := setup(t)
	e.srv.cfg.IsLocal = func(netip.Addr) bool { return true }
	c, err := tls.Dial("tcp", e.addr, &tls.Config{ServerName: "loop.example.com", InsecureSkipVerify: true})
	if err == nil {
		c.Close()
		t.Fatal("handshake succeeded through a looping name")
	}
	waitLog(t, e, ` TCP 403 `)
	if len(e.fr.dialed) != 0 {
		t.Fatalf("dialed %v for a looping name", e.fr.dialed)
	}
	if e.conns.With("loop").Value() != 1 {
		t.Fatal("loop not counted")
	}
	if _, ok := e.doms.Load("loop.example.com"); ok {
		t.Fatal("a refused name got domain counters")
	}
}

func TestLocalAddrsCachesInterfaceList(t *testing.T) {
	now := time.Unix(0, 0)
	reads := 0
	var fail bool
	ip := net.ParseIP("192.0.2.7")
	l := &localAddrs{
		now: func() time.Time { return now },
		list: func() ([]net.Addr, error) {
			reads++
			if fail {
				return nil, errors.New("netlink unavailable")
			}
			return []net.Addr{&net.IPNet{IP: ip, Mask: net.CIDRMask(24, 32)}}, nil
		},
	}
	local := netip.MustParseAddr("192.0.2.7")
	other := netip.MustParseAddr("198.51.100.1")

	if !l.contains(local) || l.contains(other) {
		t.Fatal("wrong answer from first read")
	}
	if !l.contains(netip.MustParseAddr("::ffff:192.0.2.7")) {
		t.Error("IPv4-mapped form of a local address not recognised")
	}
	if !l.contains(netip.MustParseAddr("127.0.0.1")) || !l.contains(netip.MustParseAddr("::")) {
		t.Error("loopback or unspecified not local")
	}
	if reads != 1 {
		t.Errorf("reads = %d within the TTL, want 1", reads)
	}

	now = now.Add(localAddrTTL + time.Second)
	fail = true
	if !l.contains(local) {
		t.Error("failed refresh forgot the last good list")
	}
	if reads != 2 {
		t.Errorf("reads = %d after the TTL, want 2", reads)
	}
}
