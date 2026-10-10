package proxy

import (
	"bufio"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/domains"
	"cacheparty/internal/metrics"
)

// newLandingServer serves a Handler with no Upstream, so a request it tried
// to proxy would fail the test.
func newLandingServer(t *testing.T) (*httptest.Server, *syncBuf, *metrics.Vec) {
	t.Helper()
	logs := &syncBuf{}
	reqs := metrics.NewRegistry().CounterVec("requests_total", "", "status")
	h := New(Config{
		Hostname:      "cachebox",
		Matcher:       domains.NewMatcher(),
		Access:        accesslog.NewLogger(logs, "cachelog"),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		Requests:      reqs,
		DashboardPort: "8080",
	})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s, logs, reqs
}

func TestLandingPageForIPHost(t *testing.T) {
	s, logs, reqs := newLandingServer(t)
	resp, err := http.Get(s.URL + "/") // Host is 127.0.0.1:<port>
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("status %d, type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{"cachebox", `href="http://127.0.0.1:8080/"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page lacks %q:\n%s", want, body)
		}
	}
	time.Sleep(50 * time.Millisecond) // logging runs after the response
	if l := logs.String(); l != "" {
		t.Errorf("landing page logged: %s", l)
	}
	if n := reqs.With("-").Value(); n != 0 {
		t.Errorf("landing page counted as %d requests", n)
	}
}

func TestLandingPageWithoutHost(t *testing.T) {
	s, logs, _ := newLandingServer(t)
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET / HTTP/1.0\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `href="http://127.0.0.1:8080/"`) {
		t.Errorf("status %d, body:\n%s", resp.StatusCode, body)
	}
	time.Sleep(50 * time.Millisecond) // logging runs after the response
	if l := logs.String(); l != "" {
		t.Errorf("landing page logged: %s", l)
	}
}

func TestOtherPathsOnIPHostAreNotFound(t *testing.T) {
	s, logs, _ := newLandingServer(t)
	resp, err := http.Get(s.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("status %d, want 404", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond) // logging runs after the response
	if l := logs.String(); l != "" {
		t.Errorf("logged: %s", l)
	}
}

func TestForCache(t *testing.T) {
	for host, want := range map[string]bool{
		"":                          true,
		"192.168.1.10":              true,
		"192.168.1.10:80":           true,
		"[fd00::1]":                 true,
		"[fd00::1]:80":              true,
		"cdn.test":                  false,
		"lancache.steamcontent.com": false,
	} {
		if got := forCache(&http.Request{Host: host}); got != want {
			t.Errorf("forCache(%q) = %v, want %v", host, got, want)
		}
	}
}
