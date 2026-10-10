package proxy

import (
	"context"
	"io"
	"net/http"
	"testing"

	"cacheparty/internal/domains"
	"cacheparty/internal/testutil"
)

const benchSlice = 1 << 20

// benchWriter is a ResponseWriter that discards the body. Its ReadFrom
// claims the bytes without reading them, standing in for the kernel's
// sendfile, so the numbers are the cost of our code and not of copying.
// The header map is reused, so net/http's own per-request allocations
// (header map, request, response) are not counted either.
type benchWriter struct {
	h    http.Header
	n    int64
	code int
}

func (w *benchWriter) Header() http.Header         { return w.h }
func (w *benchWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }
func (w *benchWriter) WriteHeader(c int)           { w.code = c }
func (w *benchWriter) Flush()                      {}
func (w *benchWriter) ReadFrom(r io.Reader) (int64, error) {
	lr := r.(*io.LimitedReader)
	n := lr.N
	lr.N = 0
	w.n += n
	return n, nil
}

func (w *benchWriter) reset() {
	clear(w.h)
	w.n, w.code = 0, 0
}

// hitHandler returns a handler over a pre-filled store. It has no upstream
// client, so any miss would crash the benchmark rather than go unnoticed.
func hitHandler(tb testing.TB, size int) (*Handler, *cenv) {
	tb.Helper()
	o := testutil.NewOrigin(tb)
	e := newCachedEnvAt(tb, o.Listener.Addr().String(), nil, benchSlice)
	e.origin = o
	o.Set("/bench.bin", &testutil.Obj{Body: testutil.Pattern(size), ContentType: "application/octet-stream"})
	if resp, _ := e.get(tb, "/bench.bin", nil); resp.StatusCode != 200 {
		tb.Fatalf("fill: status %d", resp.StatusCode)
	}
	m := domains.NewMatcher()
	_ = m.Add("testcdn", "cdn.test")
	return New(Config{Hostname: "cachebox", Matcher: m, Store: e.st, SliceSize: benchSlice}), e
}

func benchRequest(e *cenv, rng string) *http.Request {
	req := e.newReq(context.Background(), "/bench.bin", nil)
	req.RequestURI = "/bench.bin"
	req.RemoteAddr = "10.0.0.1:1234"
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	return req
}

var hitCases = []struct {
	name   string
	rng    string
	want   int64
	chunks int
}{
	{"one-full-chunk", "bytes=1048576-2097151", 1 << 20, 1},
	{"small-range", "bytes=100-4195", 4096, 1},
	{"spans-3-chunks", "bytes=500000-3000000", 3000000 - 500000 + 1, 3},
	{"whole-object", "", 3 << 20, 3},
}

func BenchmarkHitServeHTTP(b *testing.B) {
	h, e := hitHandler(b, 3<<20)
	for _, tc := range hitCases {
		b.Run(tc.name, func(b *testing.B) {
			req := benchRequest(e, tc.rng)
			w := &benchWriter{h: make(http.Header, 16)}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				w.reset()
				h.ServeHTTP(w, req)
				if w.n != tc.want {
					b.Fatalf("sent %d bytes (status %d), want %d", w.n, w.code, tc.want)
				}
			}
		})
	}
}

// BenchmarkHitLoopback is the full path over real sockets: sendfile on the
// server side, a draining client on the other. Allocations shown include
// the HTTP client's and net/http's own.
func BenchmarkHitLoopback(b *testing.B) {
	_, e := hitHandler(b, 3<<20)
	for _, tc := range hitCases {
		b.Run(tc.name, func(b *testing.B) {
			req := benchRequest(e, tc.rng)
			req.RequestURI = ""
			b.SetBytes(tc.want)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				resp, err := http.DefaultClient.Do(req.Clone(context.Background()))
				if err != nil {
					b.Fatal(err)
				}
				n, _ := io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if n != tc.want {
					b.Fatalf("got %d bytes, want %d", n, tc.want)
				}
			}
		})
	}
}

// Guards the hit path against allocation creep. The limit is what the
// path costs today; raise it only on purpose.
func TestHitPathAllocations(t *testing.T) {
	h, e := hitHandler(t, 3<<20)
	for _, tc := range hitCases {
		req := benchRequest(e, tc.rng)
		w := &benchWriter{h: make(http.Header, 16)}
		got := testing.AllocsPerRun(200, func() {
			w.reset()
			h.ServeHTTP(w, req)
		})
		t.Logf("%-16s %.0f allocs/request", tc.name, got)
		// Per request: ~6 for headers and bookkeeping. Per chunk: ~5 for
		// opening its file (os.File, path, fstat); a file-per-chunk layout
		// cannot avoid those. Limits have a little headroom.
		if limit := 12.0 + 6.0*float64(tc.chunks-1); got > limit {
			t.Errorf("%s: %.0f allocs/request, want <= %.0f", tc.name, got, limit)
		}
	}
}
