// Package sni is the :443 passthrough. It reads the TLS ClientHello just far
// enough to learn the server name, connects to that name on port 443 through
// the upstream-only resolver, replays the hello and then relays bytes in both
// directions. Nothing is decrypted or cached: HTTPS downloads go straight to
// the CDN, as with nginx's stream module and ssl_preread.
package sni

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"crypto/tls"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/metrics"
)

// Resolver is satisfied by *resolver.Resolver.
type Resolver interface {
	Lookup(ctx context.Context, host string) ([]netip.Addr, error)
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Config carries the server's dependencies.
type Config struct {
	Resolver Resolver
	Log      *slog.Logger
	Access   io.Writer // stream-access.log; nil disables logging

	// IsLocal reports whether ip belongs to this machine. A name that
	// resolves to one is refused, since dialing it would loop back to us.
	// Defaults to checking the interface addresses.
	IsLocal func(ip netip.Addr) bool

	HelloTimeout time.Duration // reading the ClientHello; default 10s
	DialTimeout  time.Duration // default 10s

	// Optional metrics; nil values are ignored.
	Conns         *metrics.Vec     // connections by result
	BytesSent     *metrics.Counter // to clients
	BytesReceived *metrics.Counter // from clients

	// Domain, if set, returns the counters for one server name's bytes,
	// sent to and received from clients. It is called once a connection to
	// that name is relayed; either counter may be nil.
	Domain func(name string) (sent, received *metrics.Counter)
}

// nginx stream status codes, which the log uses.
const (
	statusOK         = 200
	statusBadRequest = 400
	statusForbidden  = 403
	statusBadGateway = 502
)

// Server relays :443 connections.
type Server struct {
	cfg Config

	mu     sync.Mutex
	ln     net.Listener
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// New returns a Server.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.IsLocal == nil {
		cfg.IsLocal = newLocalAddrs().contains
	}
	if cfg.HelloTimeout <= 0 {
		cfg.HelloTimeout = 10 * time.Second
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	return &Server{cfg: cfg, conns: map[net.Conn]struct{}{}}
}

// Serve accepts connections on ln until it is closed or Shutdown is called.
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		ln.Close()
		return net.ErrClosed
	}
	s.ln = ln
	s.mu.Unlock()
	for {
		c, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				time.Sleep(50 * time.Millisecond)
				continue
			}
			return err
		}
		if !s.track(c, true) {
			c.Close()
			return nil
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.track(c, false)
			s.handle(c)
		}()
	}
}

func (s *Server) track(c net.Conn, add bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !add {
		delete(s.conns, c)
		return true
	}
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// Shutdown stops accepting and waits for open connections to finish. When ctx
// ends first the remaining ones are closed.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	if s.ln != nil {
		s.ln.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
		<-done
		return ctx.Err()
	}
}

func (s *Server) handle(client net.Conn) {
	defer client.Close()
	start := time.Now()
	e := accesslog.StreamEntry{
		RemoteAddr: remoteIP(client), Time: start, Protocol: "TCP",
	}
	c := &counts{sent: s.cfg.BytesSent, received: s.cfg.BytesReceived}
	defer func() {
		e.BytesSent, e.BytesReceived = c.n[0].Load(), c.n[1].Load()
		e.SessionTime = time.Since(start)
		s.finish(&e)
	}()

	name, hello, err := readServerName(client, s.cfg.HelloTimeout)
	c.addReceived(int64(len(hello)))
	e.ServerName = name
	if err != nil || name == "" {
		e.Status = statusBadRequest
		return
	}

	up, status := s.connect(name)
	if status != statusOK {
		e.Status = status
		return
	}
	defer up.Close()
	if s.cfg.Domain != nil {
		// The hello was counted before the name was known.
		c.domSent, c.domReceived = s.cfg.Domain(name)
		c.domReceived.Add(int64(len(hello)))
	}
	if _, err := up.Write(hello); err != nil {
		e.Status = statusBadGateway
		return
	}
	e.Status = statusOK
	relay(client, up, c)
}

// counts tallies one connection's bytes, sent to the client and received
// from it, and passes them on to the totals and the server name's counters
// as they move.
type counts struct {
	n                    [2]atomic.Int64 // sent, received
	sent, received       *metrics.Counter
	domSent, domReceived *metrics.Counter
}

func (c *counts) addSent(n int64) {
	c.n[0].Add(n)
	c.sent.Add(n)
	c.domSent.Add(n)
}

func (c *counts) addReceived(n int64) {
	c.n[1].Add(n)
	c.received.Add(n)
	c.domReceived.Add(n)
}

// connect dials name:443, refusing names that lead back to this machine.
func (s *Server) connect(name string) (net.Conn, int) {
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.DialTimeout)
	defer cancel()
	addrs, err := s.cfg.Resolver.Lookup(ctx, name)
	if err != nil {
		s.cfg.Log.Warn("sni: resolve", "host", name, "err", err)
		return nil, statusBadGateway
	}
	for _, a := range addrs {
		if s.cfg.IsLocal(a) {
			s.cfg.Log.Warn("sni: host resolves to this machine; DNS is looping back", "host", name, "addr", a)
			return nil, statusForbidden
		}
	}
	c, err := s.cfg.Resolver.DialContext(ctx, "tcp", net.JoinHostPort(name, "443"))
	if err != nil {
		s.cfg.Log.Warn("sni: dial", "host", name, "err", err)
		return nil, statusBadGateway
	}
	return c, statusOK
}

func (s *Server) finish(e *accesslog.StreamEntry) {
	result := "ok"
	switch e.Status {
	case statusBadRequest:
		result = "bad_request"
	case statusForbidden:
		result = "loop"
	case statusBadGateway:
		result = "upstream_error"
	}
	s.cfg.Conns.With(result).Inc()
	if s.cfg.Access != nil {
		_, _ = s.cfg.Access.Write(accesslog.AppendStream(nil, e))
	}
}

// relay copies both ways until each side is done. When one direction ends,
// the other side is told with a half-close so the peer sees a clean EOF; if
// a direction fails, both connections are torn down.
func relay(client, up net.Conn, c *counts) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		finishHalf(up, client, copyCounting(up, client, c.addReceived))
	}()
	finishHalf(client, up, copyCounting(client, up, c.addSent))
	wg.Wait()
}

// countEvery is how many bytes copyCounting moves between reports, so a long
// download shows up in the counters while it runs rather than at its end.
const countEvery = 256 << 10

// copyCounting copies src to dst until EOF, reporting bytes to add every
// countEvery. Copying through an io.LimitedReader keeps the kernel's
// zero-copy splice between TCP connections, which wrapping either side in a
// counting reader or writer would lose.
func copyCounting(dst io.Writer, src io.Reader, add func(int64)) error {
	for {
		n, err := io.Copy(dst, &io.LimitedReader{R: src, N: countEvery})
		add(n)
		if err != nil || n < countEvery {
			return err
		}
	}
}

func finishHalf(dst, src net.Conn, err error) {
	if err != nil {
		dst.Close()
		src.Close()
		return
	}
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	} else {
		dst.Close()
	}
}

var errStop = errors.New("sni: hello read")

// readServerName reads the client's TLS ClientHello and returns the SNI plus
// every byte consumed, to be replayed upstream. The stdlib TLS server does
// the parsing: its GetConfigForClient callback runs as soon as the hello is
// decoded, and returning an error from it ends the handshake without
// answering. The conn handed to it swallows writes so no alert reaches the
// client.
func readServerName(c net.Conn, timeout time.Duration) (name string, hello []byte, err error) {
	_ = c.SetReadDeadline(time.Now().Add(timeout))
	defer c.SetReadDeadline(time.Time{})

	rc := &recorder{Conn: c}
	srv := tls.Server(rc, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name = h.ServerName
			return nil, errStop
		},
	})
	err = srv.Handshake()
	if errors.Is(err, errStop) {
		err = nil
	}
	name = normalize(name)
	return name, rc.buf.Bytes(), err
}

type recorder struct {
	net.Conn
	buf bytes.Buffer
}

func (r *recorder) Read(p []byte) (int, error) {
	n, err := r.Conn.Read(p)
	r.buf.Write(p[:n])
	return n, err
}

func (r *recorder) Write(p []byte) (int, error) { return len(p), nil }

// normalize lower-cases name and returns "" for anything that is not a plain
// DNS name, so an IP literal or junk never reaches the resolver.
func normalize(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name == "" || len(name) > 253 {
		return ""
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return ""
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return ""
		}
	}
	return name
}

func remoteIP(c net.Conn) string {
	if host, _, err := net.SplitHostPort(c.RemoteAddr().String()); err == nil {
		return host
	}
	return c.RemoteAddr().String()
}

// localAddrTTL is how long the interface address list is reused before it
// is read again. Addresses rarely change, and the check runs for every
// connection.
const localAddrTTL = 30 * time.Second

// localAddrs remembers this machine's interface addresses for localAddrTTL.
type localAddrs struct {
	list func() ([]net.Addr, error) // net.InterfaceAddrs
	now  func() time.Time

	mu    sync.Mutex
	set   map[netip.Addr]struct{}
	until time.Time
}

func newLocalAddrs() *localAddrs {
	return &localAddrs{list: net.InterfaceAddrs, now: time.Now}
}

// contains reports whether ip belongs to this machine.
func (l *localAddrs) contains(ip netip.Addr) bool {
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	ip = ip.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now := l.now(); now.After(l.until) {
		set, err := l.read()
		if err != nil && l.set != nil {
			set = l.set // keep the last good list rather than forget it
		}
		l.set, l.until = set, now.Add(localAddrTTL)
	}
	_, ok := l.set[ip]
	return ok
}

func (l *localAddrs) read() (map[netip.Addr]struct{}, error) {
	addrs, err := l.list()
	if err != nil {
		return nil, err
	}
	set := make(map[netip.Addr]struct{}, len(addrs))
	for _, a := range addrs {
		if p, ok := a.(*net.IPNet); ok {
			if got, ok := netip.AddrFromSlice(p.IP); ok {
				set[got.Unmap()] = struct{}{}
			}
		}
	}
	return set, nil
}
