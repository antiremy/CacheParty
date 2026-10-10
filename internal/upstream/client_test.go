package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"cacheparty/internal/resolver"
	"cacheparty/internal/testutil"
)

func newClient(t *testing.T, names map[string]string) *Client {
	t.Helper()
	dns := testutil.NewFakeDNS(t, names)
	r, err := resolver.New([]string{dns.Addr}, resolver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return New(r, Options{})
}

func get(t *testing.T, c *Client, url string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func port(srv *httptest.Server) string {
	return srv.Listener.Addr().String()[strings.LastIndexByte(srv.Listener.Addr().String(), ':'):]
}

func TestDialsThroughUpstreamResolverAndSendsHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s ua=%q", r.Host, r.Header["User-Agent"])
	}))
	defer srv.Close()
	c := newClient(t, map[string]string{"cdn.test": "127.0.0.1"})

	_, body := get(t, c, "http://cdn.test"+port(srv)+"/x", nil)
	if body != `host=cdn.test`+port(srv)+` ua=[]` {
		t.Fatalf("body = %s", body)
	}
	_, body = get(t, c, "http://cdn.test"+port(srv)+"/x", map[string]string{"User-Agent": "Steam"})
	if !strings.Contains(body, `ua=["Steam"]`) {
		t.Fatalf("body = %s", body)
	}
}

func TestFollowsRedirectsAndKeepsRange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/a", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/b", http.StatusFound) })
	mux.HandleFunc("/b", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/c", http.StatusTemporaryRedirect) })
	mux.HandleFunc("/c", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "range="+r.Header.Get("Range")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newClient(t, map[string]string{"cdn.test": "127.0.0.1"})

	resp, body := get(t, c, "http://cdn.test"+port(srv)+"/a", map[string]string{"Range": "bytes=0-9"})
	if resp.StatusCode != 200 || body != "range=bytes=0-9" {
		t.Fatalf("status=%d body=%q", resp.StatusCode, body)
	}
}

func TestRedirectLimit(t *testing.T) {
	var hops int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops++
		http.Redirect(w, r, "/loop", http.StatusFound)
	}))
	defer srv.Close()
	c := newClient(t, map[string]string{"cdn.test": "127.0.0.1"})
	req, _ := http.NewRequest("GET", "http://cdn.test"+port(srv)+"/loop", nil)
	resp, err := c.Do(req)
	if resp != nil {
		resp.Body.Close()
	}
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("err = %v", err)
	}
	if hops != MaxRedirects+1 {
		t.Fatalf("hops = %d, want %d", hops, MaxRedirects+1)
	}
}

func TestRetriesNotFoundOnDifferentAddress(t *testing.T) {
	var hits [2]atomic.Int32
	mk := func(i int, status int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			w.WriteHeader(status)
			fmt.Fprintf(w, "server %d", i)
		}))
	}
	a, b := mk(0, 404), mk(1, 200)
	defer a.Close()
	defer b.Close()

	d := &avoidDialer{first: a.Listener.Addr().String(), second: b.Listener.Addr().String()}
	c := New(d, Options{})
	resp, body := get(t, c, "http://cdn.test/x", nil)
	if resp.StatusCode != 200 || body != "server 1" {
		t.Fatalf("status=%d body=%q, want the retry's 200 from server 1", resp.StatusCode, body)
	}
	if hits[0].Load() != 1 || hits[1].Load() != 1 {
		t.Errorf("hits = %d, %d; want one each", hits[0].Load(), hits[1].Load())
	}
}

func TestNotFoundEverywhereReturnsNotFound(t *testing.T) {
	mk := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	}
	a, b := mk(), mk()
	defer a.Close()
	defer b.Close()
	c := New(&avoidDialer{first: a.Listener.Addr().String(), second: b.Listener.Addr().String()}, Options{})
	resp, _ := get(t, c, "http://cdn.test/x", nil)
	if resp.StatusCode != 404 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func TestRetriesResetConnectionOnDifferentAddress(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler) // closes the connection without a response
	}))
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	defer bad.Close()
	defer good.Close()
	c := New(&avoidDialer{first: bad.Listener.Addr().String(), second: good.Listener.Addr().String()}, Options{})
	if _, body := get(t, c, "http://cdn.test/x", nil); body != "ok" {
		t.Fatalf("body = %q", body)
	}
}

func TestNoRetryForPOSTOrOtherStatuses(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	d := &avoidDialer{first: srv.Listener.Addr().String(), second: srv.Listener.Addr().String()}
	c := New(d, Options{})
	if resp, _ := get(t, c, "http://cdn.test/x", nil); resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if n.Load() != 1 {
		t.Errorf("500 was retried (%d requests)", n.Load())
	}
	req, _ := http.NewRequest("POST", "http://cdn.test/x", strings.NewReader("x"))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if n.Load() != 2 {
		t.Errorf("POST was retried (%d requests)", n.Load())
	}
}

// avoidDialer stands in for the resolver: the first server is "the first
// address of cdn.test" and the second "another one". Like the real resolver
// it honours resolver.WithAvoid.
type avoidDialer struct {
	first, second string
}

func (d *avoidDialer) DialContext(ctx context.Context, network, _ string) (net.Conn, error) {
	if resolver.Avoided(ctx, "cdn.test") != "" {
		return net.Dial(network, d.second)
	}
	return net.Dial(network, d.first)
}
