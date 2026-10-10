package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/domains"
	"cacheparty/internal/resolver"
	"cacheparty/internal/store"
	"cacheparty/internal/testutil"
	"cacheparty/internal/upstream"
)

const testSlice = 1000

type cenv struct {
	proxy  *httptest.Server
	st     *store.FileStore
	dir    string // the store's root
	origin *testutil.Origin
	log    *syncBuf // access log
	errs   *syncBuf // error log
	host   string   // "cdn.test:<origin port>"
	h      *Handler
}

// newCachedEnv wires a caching proxy to a fake origin reachable only via
// the fake upstream DNS ("cdn.test" -> 127.0.0.1).
func newCachedEnv(t testing.TB) *cenv {
	t.Helper()
	o := testutil.NewOrigin(t)
	e := newCachedEnvAt(t, o.Listener.Addr().String(), nil, testSlice)
	e.origin = o
	return e
}

// newCachedEnvAt is for tests with a hand-written origin at addr. wrap, if
// non-nil, wraps the proxy's listener.
func newCachedEnvAt(t testing.TB, addr string, wrap func(net.Listener) net.Listener, slice int64) *cenv {
	t.Helper()
	dns := testutil.NewFakeDNS(t, map[string]string{"cdn.test": "127.0.0.1"})
	res, err := resolver.New([]string{dns.Addr}, resolver.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := domains.NewMatcher()
	_ = m.Add("testcdn", "cdn.test")
	errs := &syncBuf{}
	elog := slog.New(slog.NewTextHandler(errs, nil))
	dir := t.TempDir()
	st, err := store.Open(dir, store.Options{FlushInterval: time.Hour, Log: elog})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	logs := &syncBuf{}
	h := New(Config{
		Hostname:  "cachebox",
		Version:   "v1.2.3",
		Matcher:   m,
		Upstream:  upstream.New(res, upstream.Options{}),
		Access:    accesslog.NewLogger(logs, "cachelog"),
		Log:       elog,
		Store:     st,
		SliceSize: slice,
	})
	waitFillsAtCleanup(t, h)
	p := httptest.NewUnstartedServer(h)
	if wrap != nil {
		p.Listener = wrap(p.Listener)
	}
	p.Start()
	t.Cleanup(p.Close)
	return &cenv{proxy: p, st: st, dir: dir, log: logs, errs: errs, h: h, host: "cdn.test" + addr[strings.LastIndexByte(addr, ':'):]}
}

// waitFillsAtCleanup makes cleanup wait for h's background fills. Register it
// after the store's cleanup and before the server's, so it runs after the
// server closes and before the store does: a fill must not write into a
// closed store or a removed directory.
func waitFillsAtCleanup(t testing.TB, h *Handler) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := h.Wait(ctx); err != nil {
			t.Errorf("background fills still running at cleanup: %v", err)
		}
	})
}

func (e *cenv) newReq(ctx context.Context, path string, hdr map[string]string) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, "GET", e.proxy.URL+path, nil)
	req.Host = e.host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return req
}

func (e *cenv) get(t testing.TB, path string, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.DefaultClient.Do(e.newReq(context.Background(), path, hdr))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading body of %s: %v", path, err)
	}
	return resp, b
}

func (e *cenv) chunks() int64 { n, _ := e.st.Usage(); return n }

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestColdMissThenWarmHit(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500) // 3 chunks of 1000
	lm := time.Date(2024, 5, 6, 7, 8, 9, 0, time.UTC)
	e.origin.Set("/game.bin", &testutil.Obj{Body: body, ContentType: "application/x-game", LastModified: lm})

	resp, got := e.get(t, "/game.bin", nil)
	eq(t, "cold status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("cold body mismatch")
	}
	eq(t, "cold cache status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
	eq(t, "cold upstream status", resp.Header.Get("X-Upstream-Status"), "206")
	eq(t, "Content-Length", resp.Header.Get("Content-Length"), "2500")
	eq(t, "Content-Type", resp.Header.Get("Content-Type"), "application/x-game")
	eq(t, "Last-Modified", resp.Header.Get("Last-Modified"), lm.Format(http.TimeFormat))
	eq(t, "Accept-Ranges", resp.Header.Get("Accept-Ranges"), "bytes")
	eq(t, "ETag stripped", resp.Header.Get("ETag"), "")
	eq(t, "Clacks", resp.Header.Get("X-Clacks-Overhead") != "", true)
	eq(t, "Processed-By", resp.Header.Get("X-LanCache-Processed-By"), "cachebox")
	if got := e.origin.Ranges("/game.bin"); fmt.Sprint(got) != "[bytes=0-999 bytes=1000-1999 bytes=2000-2999]" {
		t.Errorf("upstream ranges = %v", got)
	}
	if reqs := e.origin.Requests(); reqs[0].AE != "identity" {
		t.Errorf("chunk fetch Accept-Encoding = %q, want identity", reqs[0].AE)
	}
	eq(t, "chunks stored", e.chunks(), int64(3))

	before := len(e.origin.Requests())
	resp, got = e.get(t, "/game.bin", nil)
	eq(t, "warm status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("warm body mismatch")
	}
	eq(t, "warm cache status", resp.Header.Get("X-Upstream-Cache-Status"), "HIT")
	eq(t, "warm upstream status header", resp.Header.Get("X-Upstream-Status"), "")
	eq(t, "warm version", resp.Header.Get("X-CacheParty-Version"), "v1.2.3")
	eq(t, "warm Content-Type", resp.Header.Get("Content-Type"), "application/x-game")
	eq(t, "warm Content-Length", resp.Header.Get("Content-Length"), "2500")
	eq(t, "warm Last-Modified", resp.Header.Get("Last-Modified"), lm.Format(http.TimeFormat))
	eq(t, "warm upstream requests", len(e.origin.Requests()), before)

	// the access log says so too
	deadline := time.Now().Add(2 * time.Second)
	for strings.Count(e.log.String(), "\n") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	lines := strings.Split(strings.TrimSpace(e.log.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"MISS"`) || !strings.Contains(lines[1], `"HIT"`) ||
		!strings.Contains(lines[0], `" 200 2500 `) {
		t.Errorf("access log:\n%s", strings.Join(lines, "\n"))
	}
}

func TestByteRanges(t *testing.T) {
	const size = 2500
	body := testutil.Pattern(size)
	for i, tc := range []struct {
		rng        string
		start, end int // expected inclusive bytes, for 206
		status     int
	}{
		{"", 0, size - 1, 200},
		{"bytes=0-0", 0, 0, 206},
		{"bytes=5-9", 5, 9, 206},
		{"bytes=0-999", 0, 999, 206},
		{"bytes=995-1005", 995, 1005, 206},
		{"bytes=1000-1999", 1000, 1999, 206},
		{"bytes=999-1000", 999, 1000, 206},
		{"bytes=500-2200", 500, 2200, 206},
		{"bytes=0-2499", 0, 2499, 206},
		{"bytes=0-99999", 0, 2499, 206},
		{"bytes=1500-", 1500, 2499, 206},
		{"bytes=2499-", 2499, 2499, 206},
		{"bytes=2499-2499", 2499, 2499, 206},
		{"bytes=2000-2100", 2000, 2100, 206},
		{"bytes=-100", 2400, 2499, 206},
		{"bytes=-1", 2499, 2499, 206},
		{"bytes=-2500", 0, 2499, 206},
		{"bytes=-99999", 0, 2499, 206},
		{"bytes=-1200", 1300, 2499, 206},
		{"bytes=0-1,5-6", 0, size - 1, 200}, // multi-range: full object
		{"bytes=5-2", 0, size - 1, 200},     // invalid: ignored
		{"items=0-5", 0, size - 1, 200},     // unknown unit: ignored
	} {
		t.Run(fmt.Sprintf("%02d/%s", i, tc.rng), func(t *testing.T) {
			e := newCachedEnv(t)
			e.origin.Set("/f", &testutil.Obj{Body: body})
			hdr := map[string]string{}
			if tc.rng != "" {
				hdr["Range"] = tc.rng
			}
			want := body[tc.start : tc.end+1]
			for pass, wantCache := range []string{"MISS", "HIT"} {
				before := len(e.origin.Requests())
				resp, got := e.get(t, "/f", hdr)
				eq(t, "status", resp.StatusCode, tc.status)
				if !bytes.Equal(got, want) {
					t.Fatalf("pass %d: body mismatch: got %d bytes, want %d", pass, len(got), len(want))
				}
				eq(t, "Content-Length", resp.Header.Get("Content-Length"), fmt.Sprint(len(want)))
				if tc.status == 206 {
					eq(t, "Content-Range", resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-%d/%d", tc.start, tc.end, size))
				} else {
					eq(t, "Content-Range", resp.Header.Get("Content-Range"), "")
				}
				if pass == 1 {
					eq(t, "second pass cache status", resp.Header.Get("X-Upstream-Cache-Status"), wantCache)
					eq(t, "second pass upstream requests", len(e.origin.Requests()), before)
				}
			}
		})
	}
}

// A request for part of an uncached object must fetch only the chunks it
// touches (plus chunk 0 when a suffix range needs the size).
func TestRangeFetchesOnlyNeededChunks(t *testing.T) {
	body := testutil.Pattern(5500) // 6 chunks
	for _, tc := range []struct {
		rng  string
		want string
	}{
		{"bytes=2100-2200", "[bytes=2000-2999]"},
		{"bytes=1900-3100", "[bytes=1000-1999 bytes=2000-2999 bytes=3000-3999]"},
		{"bytes=4500-", "[bytes=4000-4999 bytes=5000-5999]"},
		{"bytes=-100", "[bytes=0-999 bytes=5000-5999]"},
	} {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body})
		e.get(t, "/f", map[string]string{"Range": tc.rng})
		if got := fmt.Sprint(e.origin.Ranges("/f")); got != tc.want {
			t.Errorf("%s: upstream ranges %s, want %s", tc.rng, got, tc.want)
		}
	}
}

func TestUnsatisfiableRange(t *testing.T) {
	body := testutil.Pattern(2500)
	t.Run("warm", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body})
		e.get(t, "/f", nil)
		for _, rng := range []string{"bytes=2500-", "bytes=9000-9999", "bytes=-0"} {
			before := len(e.origin.Requests())
			resp, got := e.get(t, "/f", map[string]string{"Range": rng})
			eq(t, rng+" status", resp.StatusCode, 416)
			eq(t, rng+" Content-Range", resp.Header.Get("Content-Range"), "bytes */2500")
			eq(t, rng+" body", len(got), 0)
			eq(t, rng+" upstream requests", len(e.origin.Requests()), before)
		}
	})
	t.Run("cold, upstream says 416", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body})
		resp, got := e.get(t, "/f", map[string]string{"Range": "bytes=5000-"})
		eq(t, "status", resp.StatusCode, 416)
		eq(t, "Content-Range", resp.Header.Get("Content-Range"), "bytes */2500")
		eq(t, "body", len(got), 0)
		eq(t, "chunks stored", e.chunks(), int64(0))
	})
	t.Run("cold, size learned from a real chunk", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body})
		resp, _ := e.get(t, "/f", map[string]string{"Range": "bytes=2500-"})
		eq(t, "status", resp.StatusCode, 416)
		eq(t, "Content-Range", resp.Header.Get("Content-Range"), "bytes */2500")
		resp, _ = e.get(t, "/f", map[string]string{"Range": "bytes=-0"})
		eq(t, "suffix 0 status", resp.StatusCode, 416)
	})
	t.Run("empty object, origin answers 416 */0", func(t *testing.T) {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Range", "bytes */0")
			w.WriteHeader(416)
		}))
		t.Cleanup(origin.Close)
		e := newCachedEnvAt(t, origin.Listener.Addr().String(), nil, testSlice)
		resp, got := e.get(t, "/empty", nil)
		eq(t, "status", resp.StatusCode, 200)
		eq(t, "body", len(got), 0)
		resp, _ = e.get(t, "/empty", map[string]string{"Range": "bytes=0-9"})
		eq(t, "ranged status", resp.StatusCode, 416)
		eq(t, "Content-Range", resp.Header.Get("Content-Range"), "bytes */0")
		eq(t, "chunks stored", e.chunks(), int64(0))
	})
	t.Run("empty object, origin ignores Range", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/empty", &testutil.Obj{Body: []byte{}})
		resp, got := e.get(t, "/empty", nil)
		eq(t, "status", resp.StatusCode, 200)
		eq(t, "body", len(got), 0)
	})
}

func TestUpstreamIgnoresRange(t *testing.T) {
	body := testutil.Pattern(2500)

	t.Run("no range: whole object cached as it streams", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body, IgnoreRange: true})
		resp, got := e.get(t, "/f", nil)
		eq(t, "status", resp.StatusCode, 200)
		if !bytes.Equal(got, body) {
			t.Fatal("body mismatch")
		}
		eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
		eq(t, "upstream requests", len(e.origin.Requests()), 1)
		eq(t, "chunks stored", e.chunks(), int64(3))
		resp, got = e.get(t, "/f", nil)
		if !bytes.Equal(got, body) || resp.Header.Get("X-Upstream-Cache-Status") != "HIT" {
			t.Error("warm request not a correct hit")
		}
		eq(t, "upstream requests after hit", len(e.origin.Requests()), 1)
	})

	t.Run("range request: served as 206, reads only as far as needed", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body, IgnoreRange: true})
		resp, got := e.get(t, "/f", map[string]string{"Range": "bytes=1500-1600"})
		eq(t, "status", resp.StatusCode, 206)
		eq(t, "Content-Range", resp.Header.Get("Content-Range"), "bytes 1500-1600/2500")
		if !bytes.Equal(got, body[1500:1601]) {
			t.Fatal("body mismatch")
		}
		eq(t, "chunks stored", e.chunks(), int64(2)) // 0 and 1; chunk 2 never read
		// the rest still works and is correct
		_, got = e.get(t, "/f", map[string]string{"Range": "bytes=2000-2499"})
		if !bytes.Equal(got, body[2000:]) {
			t.Fatal("second body mismatch")
		}
		eq(t, "chunks stored", e.chunks(), int64(3))
		_, got = e.get(t, "/f", nil)
		if !bytes.Equal(got, body) {
			t.Fatal("final body mismatch")
		}
	})

	t.Run("suffix range", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body, IgnoreRange: true})
		resp, got := e.get(t, "/f", map[string]string{"Range": "bytes=-300"})
		eq(t, "status", resp.StatusCode, 206)
		if !bytes.Equal(got, body[2200:]) {
			t.Fatal("body mismatch")
		}
	})

	t.Run("unknown length: relayed, not cached", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/f", &testutil.Obj{Body: body, IgnoreRange: true, NoLength: true})
		resp, got := e.get(t, "/f", nil)
		eq(t, "status", resp.StatusCode, 200)
		if !bytes.Equal(got, body) {
			t.Fatal("body mismatch")
		}
		eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
		eq(t, "chunks stored", e.chunks(), int64(0))
	})
}

func TestRedirectKeepsOriginalKey(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500)
	e.origin.Set("/start", &testutil.Obj{Redirect: "/elsewhere/final.bin"})
	e.origin.Set("/elsewhere/final.bin", &testutil.Obj{Body: body})

	resp, got := e.get(t, "/start", nil)
	eq(t, "status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
	eq(t, "cold /start requests", e.origin.Count("/start"), 3)
	eq(t, "cold final requests", e.origin.Count("/elsewhere/final.bin"), 3)
	if got := e.origin.Ranges("/elsewhere/final.bin"); fmt.Sprint(got) != "[bytes=0-999 bytes=1000-1999 bytes=2000-2999]" {
		t.Errorf("Range not preserved across the redirect: %v", got)
	}

	before := len(e.origin.Requests())
	resp, got = e.get(t, "/start", nil)
	if !bytes.Equal(got, body) || resp.Header.Get("X-Upstream-Cache-Status") != "HIT" {
		t.Error("warm request not a correct hit under the original path")
	}
	eq(t, "upstream requests on hit", len(e.origin.Requests()), before)

	// the redirect target is a different cache key and was never cached as such
	resp, _ = e.get(t, "/elsewhere/final.bin", nil)
	eq(t, "target cache status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
}

func TestNonCacheableStatusesAreNotStored(t *testing.T) {
	for _, status := range []int{404, 403, 500, 502, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			e := newCachedEnv(t)
			e.origin.Set("/f", &testutil.Obj{Status: status})
			for pass := 1; pass <= 2; pass++ {
				resp, got := e.get(t, "/f", nil)
				eq(t, "status", resp.StatusCode, status)
				eq(t, "X-Upstream-Status", resp.Header.Get("X-Upstream-Status"), fmt.Sprint(status))
				eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
				eq(t, "body", string(got), fmt.Sprintf("status %d", status))
				eq(t, "chunks stored", e.chunks(), int64(0))
				// A 404 is retried once on another address (same one here).
				perRequest := 1
				if status == 404 {
					perRequest = 2
				}
				eq(t, "upstream requests", e.origin.Count("/f"), pass*perRequest)
			}
		})
	}
	t.Run("a later success is cached", func(t *testing.T) {
		e := newCachedEnv(t)
		body := testutil.Pattern(1500)
		e.origin.Set("/f", &testutil.Obj{Status: 503})
		e.get(t, "/f", nil)
		e.origin.Set("/f", &testutil.Obj{Body: body})
		_, got := e.get(t, "/f", nil)
		if !bytes.Equal(got, body) {
			t.Fatal("body mismatch")
		}
		eq(t, "chunks stored", e.chunks(), int64(2))
	})
}

func TestClientDisconnectMidFillStillCaches(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500)
	gate, started := make(chan struct{}), make(chan string, 8)
	e.origin.Set("/f", &testutil.Obj{Body: body, Gate: gate, Started: started})

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(e.newReq(ctx, "/f", nil))
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		errc <- err
	}()
	<-started // the proxy is now waiting on the origin for chunk 0
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("client should have seen its own cancellation")
	}
	time.Sleep(50 * time.Millisecond) // let the proxy notice the disconnect
	close(gate)                       // origin finally answers

	deadline := time.Now().Add(5 * time.Second)
	for e.chunks() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	eq(t, "chunk 0 cached after the client left", e.chunks(), int64(1))
	time.Sleep(100 * time.Millisecond)
	eq(t, "no further fetches for a client that left", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999]")

	// A new client gets chunk 0 from the cache and fetches only the rest.
	_, got := e.get(t, "/f", nil)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
	eq(t, "upstream ranges", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999 bytes=1000-1999 bytes=2000-2999]")
}

func TestQueryStringIsNotPartOfTheKey(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(1200)
	e.origin.Set("/f", &testutil.Obj{Body: body})
	e.get(t, "/f?token=one", nil)
	before := len(e.origin.Requests())
	resp, got := e.get(t, "/f?token=two", nil)
	if !bytes.Equal(got, body) || resp.Header.Get("X-Upstream-Cache-Status") != "HIT" {
		t.Error("different query string missed the cache")
	}
	eq(t, "upstream requests", len(e.origin.Requests()), before)
	if q := e.origin.Requests()[0].Query; q != "token=one" {
		t.Errorf("upstream saw query %q; the query must still be forwarded", q)
	}
	// a different path is a different object
	e.origin.Set("/g", &testutil.Obj{Body: body})
	resp, _ = e.get(t, "/g?token=one", nil)
	eq(t, "other path", resp.Header.Get("X-Upstream-Cache-Status"), "MISS")
}

func TestUpstreamChangedIsNeverMixed(t *testing.T) {
	oldBody, newBody := testutil.Pattern(2500), testutil.Pattern(2600)
	for i := range newBody {
		newBody[i] ^= 0x5a
	}
	obj := store.ObjectKey("testcdn", "/f")
	fill := func(e *cenv, path string) {
		e.origin.Set(path, &testutil.Obj{Body: oldBody})
		if _, got := e.get(t, path, nil); !bytes.Equal(got, oldBody) {
			t.Fatal("first body mismatch")
		}
	}

	t.Run("detected on the first chunk: transparently refetched", func(t *testing.T) {
		e := newCachedEnv(t)
		fill(e, "/f")
		e.origin.Set("/f", &testutil.Obj{Body: newBody})
		e.st.Delete(store.ChunkKey(obj, testSlice, 0))
		resp, got := e.get(t, "/f", nil)
		eq(t, "status", resp.StatusCode, 200)
		if !bytes.Equal(got, newBody) {
			t.Fatal("served bytes that mix two versions of the object")
		}
		if _, got = e.get(t, "/f", nil); !bytes.Equal(got, newBody) {
			t.Fatal("cached copy is not the new version")
		}
	})

	t.Run("detected after the response started: aborted, never mixed", func(t *testing.T) {
		e := newCachedEnv(t)
		fill(e, "/f")
		e.origin.Set("/f", &testutil.Obj{Body: newBody})
		e.st.Delete(store.ChunkKey(obj, testSlice, 1))
		resp, err := http.DefaultClient.Do(e.newReq(context.Background(), "/f", nil))
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err == nil {
			t.Error("client should see a truncated download")
		}
		if !bytes.HasPrefix(oldBody, got) {
			t.Error("bytes before the abort are not a prefix of the old version")
		}
		// the next request starts clean and gets the new version
		if _, got = e.get(t, "/f", nil); !bytes.Equal(got, newBody) {
			t.Fatal("retry did not return the new version")
		}
	})

	t.Run("same size, different Last-Modified", func(t *testing.T) {
		e := newCachedEnv(t)
		e.origin.Set("/g", &testutil.Obj{Body: oldBody, LastModified: time.Unix(1_700_000_000, 0)})
		e.get(t, "/g", nil)
		other := newBody[:2500]
		e.origin.Set("/g", &testutil.Obj{Body: other, LastModified: time.Unix(1_800_000_000, 0)})
		e.st.Delete(store.ChunkKey(store.ObjectKey("testcdn", "/g"), testSlice, 0))
		if _, got := e.get(t, "/g", nil); !bytes.Equal(got, other) {
			t.Fatal("same-size change not detected")
		}
	})
}

func TestContentEncodingIsNeverCached(t *testing.T) {
	e := newCachedEnv(t)
	e.origin.Set("/f", &testutil.Obj{Body: testutil.Pattern(2500), Encoding: "gzip"})
	resp, got := e.get(t, "/f", map[string]string{"Accept-Encoding": "gzip"})
	eq(t, "status", resp.StatusCode, 200)
	eq(t, "Content-Encoding", resp.Header.Get("Content-Encoding"), "gzip")
	eq(t, "body", len(got), 2500)
	eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status"), "BYPASS")
	eq(t, "chunks stored", e.chunks(), int64(0))
}

func TestIfRangeServesWholeObject(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2500)
	e.origin.Set("/f", &testutil.Obj{Body: body})
	resp, got := e.get(t, "/f", map[string]string{"Range": "bytes=10-20", "If-Range": `"x"`})
	eq(t, "status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
}

func TestNonGETIsNotCached(t *testing.T) {
	e := newCachedEnv(t)
	e.origin.Set("/f", &testutil.Obj{Body: testutil.Pattern(2500)})
	req := e.newReq(context.Background(), "/f", nil)
	req.Method = "HEAD"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	eq(t, "status", resp.StatusCode, 200)
	eq(t, "cache status", resp.Header.Get("X-Upstream-Cache-Status"), "BYPASS")
	eq(t, "chunks stored", e.chunks(), int64(0))
}

func TestBadUpstreamPartialResponseIsNotCached(t *testing.T) {
	// Answers every ranged request with the wrong bytes' Content-Range.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-9/100")
		w.WriteHeader(206)
		w.Write([]byte("0123456789"))
	}))
	t.Cleanup(origin.Close)
	e := newCachedEnvAt(t, origin.Listener.Addr().String(), nil, testSlice)
	resp, _ := e.get(t, "/f", map[string]string{"Range": "bytes=1000-1005"})
	eq(t, "status", resp.StatusCode, 502)
	eq(t, "chunks stored", e.chunks(), int64(0))
}

func TestTruncatedChunkFromUpstreamIsNotCached(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", "bytes 0-999/2500")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(206)
		w.Write(make([]byte, 400)) // then hang up
	}))
	t.Cleanup(origin.Close)
	e := newCachedEnvAt(t, origin.Listener.Addr().String(), nil, testSlice)
	// The chunk is streamed to the client as it arrives, so the 200 is out
	// before the truncation is seen; the client must see a failed download,
	// not a short body that looks complete.
	resp, err := http.DefaultClient.Do(e.newReq(context.Background(), "/f", nil))
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(resp.Body)
	resp.Body.Close()
	if err == nil {
		t.Error("client read a truncated chunk without an error")
	}
	eq(t, "chunks stored", e.chunks(), int64(0))
	if _, ok := e.st.Object(store.ObjectKey("testcdn", "/f")); ok {
		t.Error("object metadata stored for a failed fill")
	}
}

// --- zero-copy ---

// sfListener wraps accepted connections to see what the HTTP server hands
// to (*net.TCPConn).ReadFrom, which is how Go reaches sendfile(2): the
// source must be an *os.File, possibly inside an *io.LimitedReader.
type sfListener struct {
	net.Listener
	fromFile atomic.Int64 // bytes read from a (limited) *os.File
	other    atomic.Int64 // bytes read from anything else
	inFlight atomic.Int64 // ReadFrom calls that haven't counted their bytes yet
}

type sfConn struct {
	*net.TCPConn
	l *sfListener
}

func (l *sfListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &sfConn{TCPConn: c.(*net.TCPConn), l: l}, nil
}

func (c *sfConn) ReadFrom(r io.Reader) (int64, error) {
	isFile := false
	switch src := r.(type) {
	case *os.File:
		isFile = true
	case *io.LimitedReader:
		_, isFile = src.R.(*os.File)
	}
	c.l.inFlight.Add(1)
	defer c.l.inFlight.Add(-1)
	n, err := c.TCPConn.ReadFrom(r)
	if isFile {
		c.l.fromFile.Add(n)
	} else {
		c.l.other.Add(n)
	}
	return n, err
}

// settle waits until every ReadFrom already started has added its bytes;
// the server can still be inside ReadFrom after the client has read the
// last byte of a response.
func (l *sfListener) settle(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(2 * time.Second); l.inFlight.Load() != 0; {
		if time.Now().After(deadline) {
			t.Fatal("ReadFrom still in flight")
		}
		time.Sleep(time.Millisecond)
	}
}

func (l *sfListener) reset(t *testing.T) {
	t.Helper()
	l.settle(t)
	l.fromFile.Store(0)
	l.other.Store(0)
}

func TestHitsAreServedFromFilesNotUserSpace(t *testing.T) {
	o := testutil.NewOrigin(t)
	var sl *sfListener
	e := newCachedEnvAt(t, o.Listener.Addr().String(), func(l net.Listener) net.Listener {
		sl = &sfListener{Listener: l}
		return sl
	}, testSlice)
	e.origin = o
	body := testutil.Pattern(2500)
	o.Set("/f", &testutil.Obj{Body: body})
	e.get(t, "/f", nil) // fill

	for _, rng := range []string{"", "bytes=10-1500", "bytes=-700"} {
		hdr := map[string]string{}
		want := body
		switch rng {
		case "bytes=10-1500":
			hdr["Range"], want = rng, body[10:1501]
		case "bytes=-700":
			hdr["Range"], want = rng, body[1800:]
		}
		sl.reset(t)
		_, got := e.get(t, "/f", hdr)
		if !bytes.Equal(got, want) {
			t.Fatalf("%q: body mismatch", rng)
		}
		sl.settle(t)
		if sl.fromFile.Load() != int64(len(want)) || sl.other.Load() != 0 {
			t.Errorf("%q: %d of %d body bytes went through a file-backed ReadFrom (%d through other readers)",
				rng, sl.fromFile.Load(), len(want), sl.other.Load())
		}
	}
}
