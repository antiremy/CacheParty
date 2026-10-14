package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cacheparty/internal/testutil"
)

type result struct {
	status int
	cache  string
	body   []byte
	err    error
}

// getAll runs n concurrent GETs of path and collects the outcomes.
func (e *cenv) getAll(n int, path string, hdr map[string]string) []result {
	out := make([]result, n)
	var wg sync.WaitGroup
	for i := range out {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(e.newReq(context.Background(), path, hdr))
			if err != nil {
				out[i].err = err
				return
			}
			defer resp.Body.Close()
			out[i].status = resp.StatusCode
			out[i].cache = resp.Header.Get("X-Upstream-Cache-Status")
			out[i].body, out[i].err = io.ReadAll(resp.Body)
		}()
	}
	wg.Wait()
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// The headline: 50 clients ask for the same cold object at once; upstream
// sees each chunk exactly once and everybody gets the right bytes.
func TestFiftyClientsOneUpstreamFetchPerChunk(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(5500) // 6 chunks of 1000, the last short
	gate, started := make(chan struct{}), make(chan string, 64)
	e.origin.Set("/big.bin", &testutil.Obj{Body: body, Gate: gate, Started: started})

	const n = 50
	done := make(chan []result, 1)
	go func() { done <- e.getAll(n, "/big.bin", nil) }()

	<-started // the first chunk's fetch has reached the origin
	// Let all clients pile up behind it before the origin answers.
	time.Sleep(200 * time.Millisecond)
	close(gate)
	results := <-done

	misses := 0
	for i, r := range results {
		if r.err != nil || r.status != 200 || !bytes.Equal(r.body, body) {
			t.Fatalf("client %d: status %d, err %v, %d bytes", i, r.status, r.err, len(r.body))
		}
		if r.cache == "MISS" {
			misses++
		}
	}
	eq(t, "clients that report MISS", misses, 1)
	want := "[bytes=0-999 bytes=1000-1999 bytes=2000-2999 bytes=3000-3999 bytes=4000-4999 bytes=5000-5999]"
	eq(t, "upstream ranges", fmt.Sprint(e.origin.Ranges("/big.bin")), want)
	eq(t, "chunks stored", e.chunks(), int64(6))

	// Warm: all hits, no more upstream traffic.
	for _, r := range e.getAll(n, "/big.bin", nil) {
		if r.err != nil || !bytes.Equal(r.body, body) || r.cache != "HIT" {
			t.Fatalf("warm client: err %v, cache %q", r.err, r.cache)
		}
	}
	eq(t, "upstream requests after warm round", len(e.origin.Requests()), 6)
}

// Clients asking for different ranges of one cold object share the chunks
// they have in common.
func TestOverlappingRangesShareFetches(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(4000)
	gate, started := make(chan struct{}), make(chan string, 16)
	e.origin.Set("/f", &testutil.Obj{Body: body, Gate: gate, Started: started})

	ranges := []string{"bytes=0-1499", "bytes=500-2499", "bytes=1500-3999", "bytes=0-3999", "bytes=999-1000"}
	var wg sync.WaitGroup
	errs := make(chan error, len(ranges))
	for _, rg := range ranges {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.DefaultClient.Do(e.newReq(context.Background(), "/f", map[string]string{"Range": rg}))
			if err != nil {
				errs <- err
				return
			}
			defer resp.Body.Close()
			got, err := io.ReadAll(resp.Body)
			var lo, hi int
			fmt.Sscanf(rg, "bytes=%d-%d", &lo, &hi)
			if err != nil || resp.StatusCode != 206 || !bytes.Equal(got, body[lo:hi+1]) {
				errs <- fmt.Errorf("%s: status %d, err %v, %d bytes", rg, resp.StatusCode, err, len(got))
			}
		}()
	}
	<-started
	time.Sleep(100 * time.Millisecond)
	close(gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	// Four chunks, each fetched once (the order depends on who got there first).
	eq(t, "upstream requests", len(e.origin.Requests()), 4)
	seen := map[string]int{}
	for _, r := range e.origin.Ranges("/f") {
		seen[r]++
	}
	for r, c := range seen {
		if c != 1 {
			t.Errorf("range %s fetched %d times", r, c)
		}
	}
}

// A hand-written origin that sends part of a chunk, then stalls.
func stallingOrigin(t *testing.T, body []byte, first int, release chan struct{}) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(206)
		w.Write(body[:first])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write(body[first:])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// Bytes reach every client while the chunk is still arriving from upstream:
// nobody waits for the whole chunk before getting a first byte.
func TestClientsStreamWhileChunkIsStillArriving(t *testing.T) {
	body := testutil.Pattern(1000) // one chunk
	release := make(chan struct{})
	srv := stallingOrigin(t, body, 300, release)
	e := newCachedEnvAt(t, srv.Listener.Addr().String(), nil, testSlice)

	const n = 5
	type firstRead struct {
		got []byte
		err error
	}
	first := make(chan firstRead, n)
	rest := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			resp, err := http.DefaultClient.Do(e.newReq(context.Background(), "/f", nil))
			if err != nil {
				first <- firstRead{err: err}
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 300)
			_, err = io.ReadFull(resp.Body, buf)
			first <- firstRead{buf, err}
			tail, err := io.ReadAll(resp.Body)
			rest <- result{body: append(buf, tail...), err: err}
		}()
	}
	for i := 0; i < n; i++ {
		select {
		case fr := <-first:
			if fr.err != nil || !bytes.Equal(fr.got, body[:300]) {
				t.Fatalf("early bytes: err %v", fr.err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no bytes before the upstream finished the chunk")
		}
	}
	eq(t, "chunks stored while upstream is stalled", e.chunks(), int64(0))
	close(release)
	for i := 0; i < n; i++ {
		r := <-rest
		if r.err != nil || !bytes.Equal(r.body, body) {
			t.Fatalf("client %d: err %v, %d bytes", i, r.err, len(r.body))
		}
	}
	waitFor(t, "chunk to be stored", func() bool { return e.chunks() == 1 })
}

// A cold download fetches the next readAheadMin chunks while the current one
// is still arriving, and never fetches any chunk twice.
func TestColdDownloadFetchesAhead(t *testing.T) {
	body := testutil.Pattern(5500) // 6 chunks
	release := make(chan struct{})
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if r.Header.Get("Range") != "bytes=0-999" {
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-999/%d", len(body)))
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(206)
		w.Write(body[:300])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write(body[300:1000])
	}))
	t.Cleanup(srv.Close)
	e := newCachedEnvAt(t, srv.Listener.Addr().String(), nil, testSlice)
	unstall := sync.OnceFunc(func() { close(release) })
	// Registered last so it runs first: closing either server waits on the
	// stalled handler.
	t.Cleanup(unstall)
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		s := slices.Clone(ranges)
		slices.Sort(s)
		return fmt.Sprint(s)
	}

	done := make(chan result, 1)
	go func() { done <- e.getAll(1, "/f", nil)[0] }()
	// Chunk 0 is stalled mid-body; chunks 1 and 2 are fetched meanwhile.
	waitFor(t, "chunks 1 and 2 to be cached", func() bool { return e.chunks() == 2 })
	eq(t, "upstream ranges while chunk 0 stalls", seen(), "[bytes=0-999 bytes=1000-1999 bytes=2000-2999]")

	unstall()
	r := <-done
	if r.err != nil || r.status != 200 || !bytes.Equal(r.body, body) {
		t.Fatalf("status %d, err %v, %d bytes", r.status, r.err, len(r.body))
	}
	eq(t, "cache status", r.cache, "MISS")
	waitFor(t, "every chunk to be cached", func() bool { return e.chunks() == 6 })
	eq(t, "upstream ranges", seen(),
		"[bytes=0-999 bytes=1000-1999 bytes=2000-2999 bytes=3000-3999 bytes=4000-4999 bytes=5000-5999]")
}

// Reaching a chunk that is still arriving doubles the read-ahead window.
func TestReadAheadGrowsWhenClientCatchesUp(t *testing.T) {
	body := testutil.Pattern(8000) // 8 chunks
	release := make(chan struct{})
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if r.Header.Get("Range") != "bytes=1000-1999" {
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
			return
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 1000-1999/%d", len(body)))
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(206)
		w.Write(body[1000:1300])
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Write(body[1300:2000])
	}))
	t.Cleanup(srv.Close)
	e := newCachedEnvAt(t, srv.Listener.Addr().String(), nil, testSlice)
	unstall := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unstall)
	seen := func() string {
		mu.Lock()
		defer mu.Unlock()
		s := slices.Clone(ranges)
		slices.Sort(s)
		return fmt.Sprint(s)
	}

	done := make(chan result, 1)
	go func() { done <- e.getAll(1, "/f", nil)[0] }()
	// Chunk 0 starts a window of 2 (chunks 1, 2). The client then reaches
	// chunk 1 while it stalls, so the window doubles to 4 (chunks 2-5).
	waitFor(t, "chunks 0 and 2-5 to be cached", func() bool { return e.chunks() == 5 })
	time.Sleep(50 * time.Millisecond) // a fetch beyond the window would have started by now
	eq(t, "upstream ranges while chunk 1 stalls", seen(),
		"[bytes=0-999 bytes=1000-1999 bytes=2000-2999 bytes=3000-3999 bytes=4000-4999 bytes=5000-5999]")

	unstall()
	r := <-done
	if r.err != nil || r.status != 200 || !bytes.Equal(r.body, body) {
		t.Fatalf("status %d, err %v, %d bytes", r.status, r.err, len(r.body))
	}
	waitFor(t, "every chunk to be cached", func() bool { return e.chunks() == 8 })
	eq(t, "upstream ranges", seen(),
		"[bytes=0-999 bytes=1000-1999 bytes=2000-2999 bytes=3000-3999 bytes=4000-4999 bytes=5000-5999 bytes=6000-6999 bytes=7000-7999]")
}

// A response may use only its share of the prefetch slots.
func TestReadAheadLimitSharesPrefetchSlots(t *testing.T) {
	h := New(Config{SliceSize: 1 << 20})
	eq(t, "limit with no responses reading ahead", h.readAheadLimit(), int64(32))
	h.readingAhead.Store(1)
	eq(t, "limit for one response", h.readAheadLimit(), int64(32))
	h.readingAhead.Store(4)
	eq(t, "limit for four responses", h.readAheadLimit(), int64(16))
	h.readingAhead.Store(100)
	eq(t, "limit for more responses than slots", h.readAheadLimit(), int64(1))
}

func TestReadAheadLimits(t *testing.T) {
	h := New(Config{SliceSize: 1 << 20})
	eq(t, "default prefetch slots", h.maxPrefetches.Load(), int32(defaultMaxPrefetches))
	eq(t, "window limit for 1 MiB chunks", h.readAheadMax, int64(32))
	h = New(Config{SliceSize: 64 << 20, MaxPrefetches: -1})
	eq(t, "prefetch slots when disabled", h.maxPrefetches.Load(), int32(0))
	eq(t, "window limit for chunks above the byte limit", h.readAheadMax, int64(readAheadMin))
}

// Chunk buffers go back to the pool once the fetch and its readers are done,
// so a stream of misses does not allocate one per chunk.
func TestChunkBuffersAreRecycled(t *testing.T) {
	if raceEnabled {
		t.Skip("sync.Pool drops items at random under the race detector")
	}
	defer debug.SetGCPercent(debug.SetGCPercent(-1)) // a GC may empty the pool
	e := newCachedEnv(t)
	const chunks = 100
	e.origin.Set("/f", &testutil.Obj{Body: testutil.Pattern(chunks * testSlice)})
	var allocated atomic.Int64
	e.h.chunkBufs.New = func() any {
		allocated.Add(1)
		b := make([]byte, testSlice)
		return &b
	}
	for i := chunks - 1; i >= 0; i-- {
		// One chunk per request, read backwards, so nothing is fetched
		// ahead.
		rng := fmt.Sprintf("bytes=%d-%d", i*testSlice, (i+1)*testSlice-1)
		if resp, _ := e.get(t, "/f", map[string]string{"Range": rng}); resp.StatusCode != 206 {
			t.Fatalf("%s: status %d", rng, resp.StatusCode)
		}
		if err := e.h.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Each P keeps a buffer of its own, so a few may be allocated; a leak
	// would allocate one per chunk.
	if n := allocated.Load(); n > int64(2*runtime.GOMAXPROCS(0)) || n >= chunks {
		t.Errorf("%d chunk buffers allocated for %d sequential misses", n, chunks)
	}
}

// A range request never fetches ahead past the end of the range.
func TestFetchAheadStopsAtRangeEnd(t *testing.T) {
	e := newCachedEnv(t)
	e.origin.Set("/f", &testutil.Obj{Body: testutil.Pattern(6000)})
	_, got := e.get(t, "/f", map[string]string{"Range": "bytes=0-1499"})
	eq(t, "body length", len(got), 1500)
	time.Sleep(50 * time.Millisecond) // a stray prefetch would have started by now
	eq(t, "upstream ranges", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999 bytes=1000-1999]")
}

// The client that started a fetch may leave; the others still get the chunk,
// and upstream is still asked only once.
func TestLeaderLeavingDoesNotHurtFollowers(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(2000)
	gate, started := make(chan struct{}), make(chan string, 8)
	e.origin.Set("/f", &testutil.Obj{Body: body, Gate: gate, Started: started})

	ctx, cancel := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(e.newReq(ctx, "/f", nil))
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		leaderErr <- err
	}()
	<-started

	follower := make(chan result, 1)
	go func() { follower <- e.getAll(1, "/f", nil)[0] }()
	time.Sleep(100 * time.Millisecond) // the follower is now waiting on the same fetch
	cancel()
	if err := <-leaderErr; err == nil {
		t.Fatal("leader should have seen its cancellation")
	}
	close(gate)

	r := <-follower
	if r.err != nil || r.status != 200 || !bytes.Equal(r.body, body) {
		t.Fatalf("follower: status %d, err %v, %d bytes", r.status, r.err, len(r.body))
	}
	eq(t, "upstream requests", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999 bytes=1000-1999]")
}

// An upstream error is shared by everybody waiting on that fetch, and is not
// cached: the next request tries again.
func TestUncacheableResponseReachesEveryClient(t *testing.T) {
	e := newCachedEnv(t)
	gate, started := make(chan struct{}), make(chan string, 64)
	e.origin.Set("/gone", &testutil.Obj{Status: 404, Gate: gate, Started: started})

	done := make(chan []result, 1)
	go func() { done <- e.getAll(20, "/gone", nil) }()
	<-started
	time.Sleep(100 * time.Millisecond)
	close(gate)
	for i, r := range <-done {
		if r.err != nil || r.status != 404 || string(r.body) != "status 404" {
			t.Fatalf("client %d: status %d, err %v, body %q", i, r.status, r.err, r.body)
		}
	}
	eq(t, "chunks stored", e.chunks(), int64(0))
	n := len(e.origin.Requests())

	e.origin.Set("/gone", &testutil.Obj{Body: testutil.Pattern(10)})
	resp, got := e.get(t, "/gone", nil)
	eq(t, "status after the object appears", resp.StatusCode, 200)
	eq(t, "body length", len(got), 10)
	eq(t, "retried upstream", len(e.origin.Requests()), n+1)
}

// A upstream that fails mid-chunk fails every client waiting on it, and a
// later request starts over.
func TestFailedFetchFailsAllWaiters(t *testing.T) {
	body := testutil.Pattern(1000)
	var mu sync.Mutex
	calls := 0
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		first := calls == 1
		mu.Unlock()
		w.Header().Set("Content-Range", "bytes 0-999/1000")
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(206)
		if first {
			w.Write(body[:400])
			w.(http.Flusher).Flush()
			<-release
			return // hang up short
		}
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	e := newCachedEnvAt(t, srv.Listener.Addr().String(), nil, testSlice)

	done := make(chan []result, 1)
	go func() { done <- e.getAll(10, "/f", nil) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	for i, r := range <-done {
		if r.err == nil && len(r.body) == 1000 {
			t.Errorf("client %d got a full body from a truncated fetch", i)
		}
	}
	eq(t, "chunks stored", e.chunks(), int64(0))
	_, got := e.get(t, "/f", nil)
	if !bytes.Equal(got, body) {
		t.Fatal("retry after failure returned the wrong bytes")
	}
}
