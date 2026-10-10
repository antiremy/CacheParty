package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"cacheparty/internal/domains"
	"cacheparty/internal/metrics"
	"cacheparty/internal/resolver"
	"cacheparty/internal/store"
	"cacheparty/internal/testutil"
	"cacheparty/internal/upstream"
)

func TestMetricsCountMissAndHit(t *testing.T) {
	o := testutil.NewOrigin(t)
	o.Set("/f.bin", &testutil.Obj{Body: testutil.Pattern(2 * testSlice), ContentType: "application/octet-stream"})
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "127.0.0.1"})
	res, err := resolver.New([]string{dns.Addr}, resolver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mt := domains.NewMatcher()
	_ = mt.Add("testcdn", "cdn.test")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	var (
		reqs, bytes           = &metrics.Vec{}, &metrics.Vec{}
		upReqs                = &metrics.Counter{}
		diskRead, diskWritten = &metrics.Counter{}, &metrics.Counter{}
	)
	st, err := store.Open(t.TempDir(), store.Options{FlushInterval: time.Hour, Log: quiet, BytesWritten: diskWritten})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	h := New(Config{
		Hostname: "cachebox", Matcher: mt, Upstream: upstream.New(res, upstream.Options{}), Log: quiet,
		Store: st, SliceSize: testSlice,
		Requests: reqs, BytesSent: bytes, UpstreamRequests: upReqs, CacheRead: diskRead,
	})
	waitFillsAtCleanup(t, h)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	host := "cdn.test:" + o.Listener.Addr().String()[len("127.0.0.1:"):]
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest("GET", srv.URL+"/f.bin", nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if n != 2*testSlice {
			t.Fatalf("request %d: %d bytes", i, n)
		}
	}
	deadline := time.Now().Add(2 * time.Second) // the log hook runs after the response is sent
	for reqs.With("HIT").Value() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if reqs.With("MISS").Value() != 1 || reqs.With("HIT").Value() != 1 {
		t.Fatalf("MISS=%d HIT=%d", reqs.With("MISS").Value(), reqs.With("HIT").Value())
	}
	if got := bytes.With("HIT").Value(); got != 2*testSlice {
		t.Fatalf("HIT bytes = %d", got)
	}
	if got := upReqs.Value(); got != 2 {
		t.Fatalf("upstream requests = %d, want 2 (one per chunk, none for the hit)", got)
	}
	if got := diskWritten.Value(); got != 2*testSlice {
		t.Fatalf("disk written = %d, want %d", got, 2*testSlice)
	}
	// The hit reads both chunks from disk; the miss may read a chunk that
	// read-ahead finished before the client got to it.
	if got := diskRead.Value(); got < 2*testSlice || got > 4*testSlice {
		t.Fatalf("disk read = %d, want between %d and %d", got, 2*testSlice, 4*testSlice)
	}
}

func TestFlowsCountSentAndFetchedBytes(t *testing.T) {
	o := testutil.NewOrigin(t)
	o.Set("/f.bin", &testutil.Obj{Body: testutil.Pattern(2 * testSlice), ContentType: "application/octet-stream"})
	o.Set("/nocache.bin", &testutil.Obj{Body: testutil.Pattern(100)})
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "127.0.0.1"})
	res, err := resolver.New([]string{dns.Addr}, resolver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	mt := domains.NewMatcher()
	_ = mt.Add("testcdn", "cdn.test")
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(t.TempDir(), store.Options{FlushInterval: time.Hour, Log: quiet})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// Counted in the child and, through Parent, in flow.
	flow := &Flow{Sent: &metrics.Counter{}, Fetched: &metrics.Counter{}}
	child := &Flow{Sent: &metrics.Counter{}, Fetched: &metrics.Counter{}, Parent: flow}
	var (
		mu       sync.Mutex
		services []string
		paths    []string
	)
	h := New(Config{
		Hostname: "cachebox", Matcher: mt, Upstream: upstream.New(res, upstream.Options{}), Log: quiet,
		Store: st, SliceSize: testSlice,
		Flows: func(s string, r *http.Request) *Flow {
			mu.Lock()
			defer mu.Unlock()
			services, paths = append(services, s), append(paths, r.URL.Path)
			return child
		},
	})
	waitFillsAtCleanup(t, h)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	host := "cdn.test:" + o.Listener.Addr().String()[len("127.0.0.1:"):]
	get := func(path string) {
		t.Helper()
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Host = host
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// The handler counts sent bytes after each write returns, so the client
	// can finish reading before the last write is counted.
	waitSent := func(want int64) {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		for flow.Sent.Value() < want && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	get("/f.bin") // miss
	get("/f.bin") // hit
	waitSent(4 * testSlice)
	if got := flow.Sent.Value(); got != 4*testSlice {
		t.Fatalf("sent = %d, want %d", got, 4*testSlice)
	}
	if got := flow.Fetched.Value(); got != 2*testSlice {
		t.Fatalf("fetched = %d, want %d (one copy, nothing for the hit)", got, 2*testSlice)
	}

	get("/nocache.bin?nocache=1") // bypass: fetched and sent alike
	waitSent(4*testSlice + 100)
	if s, f := flow.Sent.Value(), flow.Fetched.Value(); s != 4*testSlice+100 || f != 2*testSlice+100 {
		t.Fatalf("after bypass: sent %d fetched %d", s, f)
	}
	if s, f := child.Sent.Value(), child.Fetched.Value(); s != flow.Sent.Value() || f != flow.Fetched.Value() {
		t.Fatalf("child counted sent %d fetched %d, parent %d %d", s, f, flow.Sent.Value(), flow.Fetched.Value())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 3 || paths[0] != "/f.bin" || paths[2] != "/nocache.bin" {
		t.Fatalf("flows requested for %q", paths)
	}
	for _, s := range services {
		if s != "testcdn" {
			t.Fatalf("flow requested for %q, want testcdn", s)
		}
	}
}
