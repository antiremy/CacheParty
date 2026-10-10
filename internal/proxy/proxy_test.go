package proxy

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/domains"
	"cacheparty/internal/resolver"
	"cacheparty/internal/testutil"
	"cacheparty/internal/upstream"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type env struct {
	proxy   *httptest.Server
	log     *syncBuf
	cdnHost string // "cdn.test:<port of the fake CDN>"
}

// newEnv wires a proxy to a fake CDN that is only reachable via the fake
// upstream DNS server ("cdn.test" -> 127.0.0.1).
func newEnv(t *testing.T, cdn http.Handler) *env {
	t.Helper()
	origin := httptest.NewServer(cdn)
	t.Cleanup(origin.Close)
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "127.0.0.1"})
	res, err := resolver.New([]string{dns.Addr}, resolver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := domains.NewMatcher()
	_ = m.Add("testcdn", "cdn.test")
	logs := &syncBuf{}
	h := New(Config{
		Hostname: "cachebox",
		Version:  "v1.2.3",
		Matcher:  m,
		Upstream: upstream.New(res, upstream.Options{}),
		Access:   accesslog.NewLogger(logs, "cachelog"),
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	p := httptest.NewServer(h)
	t.Cleanup(p.Close)
	port := origin.Listener.Addr().String()[strings.LastIndexByte(origin.Listener.Addr().String(), ':'):]
	return &env{proxy: p, log: logs, cdnHost: "cdn.test" + port}
}

func (e *env) do(t *testing.T, method, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.proxy.URL+path, nil)
	req.Host = e.cdnHost
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestPassThroughBasics(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Content-Type", "application/x-test")
		w.Header().Set("X-Clacks-Overhead", "bogus")
		w.Header().Set("X-CacheParty-Version", "bogus")
		fmt.Fprintf(w, "path=%s q=%s host=%s xff=%s real=%s pb=%s",
			r.URL.Path, r.URL.RawQuery, r.Host, r.Header.Get("X-Forwarded-For"),
			r.Header.Get("X-Real-IP"), r.Header.Get("X-LanCache-Processed-By"))
	}))
	resp, body := e.do(t, "GET", "/a/b?x=1", map[string]string{
		"X-Forwarded-For": "10.0.0.9", "X-LanCache-Processed-By": "otherbox",
	})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := "path=/a/b q=x=1 host=" + e.cdnHost + " xff=10.0.0.9, 127.0.0.1 real=127.0.0.1 pb=otherbox"
	if body != want {
		t.Errorf("upstream saw:\n%s\nwant:\n%s", body, want)
	}
	for k, v := range map[string]string{
		"Etag":                    "",
		"X-Clacks-Overhead":       "GNU Terry Pratchett, GNU Zoey -Crabbey- Lough",
		"X-Lancache-Processed-By": "cachebox,otherbox",
		"X-Cacheparty-Version":    "v1.2.3",
		"X-Upstream-Cache-Status": "BYPASS",
		"X-Upstream-Status":       "200",
		"Content-Type":            "application/x-test",
	} {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q", k, got, v)
		}
	}
	if resp.Header.Get("X-Upstream-Response-Time") == "" {
		t.Error("missing X-Upstream-Response-Time")
	}
}

func TestPassesStatusAndRange(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, "f", time.Time{}, strings.NewReader("0123456789"))
	}))
	resp, body := e.do(t, "GET", "/f", map[string]string{"Range": "bytes=2-4"})
	if resp.StatusCode != 206 || body != "234" || resp.Header.Get("Content-Range") != "bytes 2-4/10" {
		t.Errorf("range: %d %q %q", resp.StatusCode, body, resp.Header.Get("Content-Range"))
	}
	resp, _ = e.do(t, "GET", "/missing", nil)
	if resp.StatusCode != 404 || resp.Header.Get("X-Upstream-Status") != "404" {
		t.Errorf("404 not passed through: %d", resp.StatusCode)
	}
	resp, body = e.do(t, "HEAD", "/f", nil)
	if resp.StatusCode != 200 || body != "" || resp.Header.Get("Content-Length") != "10" {
		t.Errorf("HEAD: %d %q cl=%s", resp.StatusCode, body, resp.Header.Get("Content-Length"))
	}
}

func TestFollowsRedirectsInternally(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/final", http.StatusFound)
			return
		}
		fmt.Fprint(w, "final")
	}))
	resp, body := e.do(t, "GET", "/start", nil)
	if resp.StatusCode != 200 || body != "final" {
		t.Errorf("got %d %q", resp.StatusCode, body)
	}
}

func TestLoopDetection(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach upstream") }))
	resp, _ := e.do(t, "GET", "/x", map[string]string{"X-LanCache-Processed-By": "cachebox"})
	if resp.StatusCode != 508 {
		t.Errorf("status %d, want 508", resp.StatusCode)
	}
}

func TestHeartbeat(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("must not reach upstream") }))
	resp, _ := e.do(t, "GET", "/lancache-heartbeat", nil)
	if resp.StatusCode != 204 ||
		resp.Header.Get("X-LanCache-Processed-By") != "cachebox" ||
		resp.Header.Get("X-CacheParty-Version") != "v1.2.3" ||
		resp.Header.Get("Access-Control-Expose-Headers") != "*" ||
		resp.Header.Get("Access-Control-Allow-Origin") != "*" ||
		resp.Header.Get("X-Clacks-Overhead") == "" {
		t.Errorf("bad heartbeat: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestBadGatewayWhenUpstreamUnresolvable(t *testing.T) {
	e := newEnv(t, http.NotFoundHandler())
	req, _ := http.NewRequest("GET", e.proxy.URL+"/x", nil)
	req.Host = "nonexistent.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status %d, want 502", resp.StatusCode)
	}
}

func TestAccessLogLine(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "hello") }))
	e.do(t, "GET", "/p?q=1", map[string]string{"User-Agent": "tester", "Range": "bytes=0-4"})
	var line string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if line = e.log.String(); line != "" {
			break
		}
	}
	// Dynamic fields: time. Everything else must match exactly.
	pre := `[testcdn] 127.0.0.1 / - - - [`
	post := `] "GET /p?q=1 HTTP/1.1" 200 5 "-" "tester" "BYPASS" "cdn.test" "bytes=0-4"` + "\n"
	if !strings.HasPrefix(line, pre) || !strings.HasSuffix(line, post) {
		t.Errorf("unexpected log line:\n%s", line)
	}
}

func TestHeartbeatIsNotLogged(t *testing.T) {
	e := newEnv(t, http.NotFoundHandler())
	e.do(t, "GET", "/lancache-heartbeat", nil)
	time.Sleep(50 * time.Millisecond)
	if s := e.log.String(); s != "" {
		t.Errorf("heartbeat logged: %s", s)
	}
}

func TestTruncatedUpstreamBodyAbortsClient(t *testing.T) {
	e := newEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.Write([]byte("short"))
		// returning early makes the server drop the connection
	}))
	req, _ := http.NewRequest("GET", e.proxy.URL+"/x", nil)
	req.Host = e.cdnHost
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return // aborted before headers is also acceptable
	}
	defer resp.Body.Close()
	if _, err := io.ReadAll(resp.Body); err == nil {
		t.Error("client should see an error for a truncated body")
	}
}
