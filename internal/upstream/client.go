// Package upstream is the HTTP client used to fetch from origin CDNs.
//
// Redirects are followed inside the client, so a miss never needs the
// second localhost proxy hop nginx uses. All connections are dialed through
// the upstream-only resolver.
package upstream

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"

	"cacheparty/internal/resolver"
)

// MaxRedirects is the number of redirect hops followed per request.
const MaxRedirects = 5

// Dialer is satisfied by *resolver.Resolver.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// Options tunes the client; zero values select defaults.
type Options struct {
	ResponseHeaderTimeout time.Duration // default 30s
	IdleConnTimeout       time.Duration // default 90s
	MaxIdleConnsPerHost   int           // default 64
}

// Client wraps an http.Client configured for cache fills.
type Client struct {
	hc    *http.Client
	retry *http.Client // fresh connections only, so a retry can pick another IP
}

// ErrTooManyRedirects is returned when a chain exceeds MaxRedirects.
var ErrTooManyRedirects = errors.New("upstream: too many redirects")

// New builds a Client that dials through d.
func New(d Dialer, opts Options) *Client {
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = 30 * time.Second
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = 90 * time.Second
	}
	if opts.MaxIdleConnsPerHost <= 0 {
		opts.MaxIdleConnsPerHost = 64
	}
	tr := &http.Transport{
		Proxy:                 nil, // never honour HTTP_PROXY: we are the proxy
		DialContext:           d.DialContext,
		DisableCompression:    true, // forward the client's Accept-Encoding untouched
		MaxIdleConns:          1024,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,
	}
	check := func(_ *http.Request, via []*http.Request) error {
		if len(via) > MaxRedirects {
			return fmt.Errorf("%w (%d)", ErrTooManyRedirects, MaxRedirects)
		}
		return nil
	}
	rt := tr.Clone()
	rt.DisableKeepAlives = true // a pooled connection would reuse the failed IP
	return &Client{
		hc:    &http.Client{Transport: tr, CheckRedirect: check},
		retry: &http.Client{Transport: rt, CheckRedirect: check},
	}
}

// Do sends req, following up to MaxRedirects redirects. Redirects are never
// surfaced to the caller; the final response is returned.
//
// A GET or HEAD that fails after connecting (reset, timeout) or is answered
// 404 is tried once more on a new connection to a different address of the
// same host, when it has one. 404 is retried because CDN edges sometimes
// miss an object their siblings have. The retry's outcome wins, unless it
// fails and the first attempt produced a response, which is returned then.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	if _, ok := req.Header["User-Agent"]; !ok {
		// An absent UA must stay absent rather than become Go-http-client.
		req.Header["User-Agent"] = []string{""}
	}
	if !retryable(req) {
		return c.hc.Do(req)
	}
	var tr connTrace
	resp, err := c.hc.Do(req.WithContext(httptrace.WithClientTrace(req.Context(), tr.trace())))
	switch {
	case err != nil:
		if req.Context().Err() != nil || errors.Is(err, ErrTooManyRedirects) {
			return resp, err
		}
	case resp.StatusCode != http.StatusNotFound:
		return resp, nil
	}
	avoid := tr.used()
	if len(avoid) == 0 {
		return resp, err // never connected (DNS or dial failure): nothing to avoid
	}
	resp2, err2 := c.retry.Do(req.Clone(resolver.WithAvoid(req.Context(), avoid)))
	if err2 != nil {
		return resp, err
	}
	if resp != nil {
		resp.Body.Close()
	}
	return resp2, nil
}

func retryable(req *http.Request) bool {
	return (req.Method == http.MethodGet || req.Method == http.MethodHead) &&
		(req.Body == nil || req.Body == http.NoBody)
}

// connTrace remembers which address each host was reached at.
type connTrace struct {
	mu   sync.Mutex
	host string // host of the connection being set up
	ips  map[string]string
}

func (t *connTrace) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(hostPort string) {
			h := hostPort
			if i := strings.LastIndexByte(h, ':'); i >= 0 {
				h = h[:i]
			}
			t.mu.Lock()
			t.host = h
			t.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			addr := info.Conn.RemoteAddr().String()
			if i := strings.LastIndexByte(addr, ':'); i >= 0 {
				addr = addr[:i]
			}
			t.mu.Lock()
			if t.ips == nil {
				t.ips = map[string]string{}
			}
			t.ips[t.host] = addr
			t.mu.Unlock()
		},
	}
}

func (t *connTrace) used() map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ips
}

// HTTPClient exposes the underlying client (used for the domain-list fetch,
// so that fetch also avoids the system resolver).
func (c *Client) HTTPClient() *http.Client { return c.hc }

// CloseIdleConnections drops pooled connections.
func (c *Client) CloseIdleConnections() { c.hc.CloseIdleConnections() }
