package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"

	"cacheparty/internal/chunk"
	"cacheparty/internal/store"
)

// outCached is a flight outcome (see fetchOutcome in cache.go): another
// flight committed the chunk just before this one started, so nothing was
// fetched and the chunk should simply be opened from the store.
const outCached outcome = 100

// flight is one in-progress upstream fetch of one chunk, shared by every
// client that needs that chunk. The fetch runs in its own goroutine, detached
// from all clients. Bytes are copied into buf as they arrive from upstream, and
// every client streams from buf while the chunk is still being committed to
// disk, so a cold chunk costs one upstream request and nobody waits for the
// whole chunk before seeing its first byte.
type flight struct {
	// ready is closed once the outcome is known. Until then only ready may
	// be touched; afterwards out, f, meta and (for failures) err are
	// immutable.
	ready chan struct{}
	out   outcome
	f     fetched
	meta  *store.ObjectMeta
	err   error

	began bool // begin ran; touched only by the fetching goroutine

	// prefetch is set for a fetch started ahead of a client (see
	// prefetch); it is immutable. The first client to join such a flight
	// takes the leader's place, since nobody else asked for it.
	prefetch bool
	claimed  atomic.Bool

	// refs counts the fetching goroutine and every client reading from the
	// flight. The last to release it returns buf to pool.
	refs atomic.Int32
	pool *sync.Pool
	bufp *[]byte

	mu   sync.Mutex
	buf  []byte // len == chunk size once out == outChunk; written only beyond n
	n    int    // bytes of buf that are valid
	fin  bool   // no more bytes will arrive
	ferr error  // why, if the chunk did not complete
	wake chan struct{}
	done chan struct{} // closed with fin
}

// newFlight returns a flight holding one reference, the fetching
// goroutine's. Its chunk buffer comes from pool.
func newFlight(pool *sync.Pool) *flight {
	fl := &flight{ready: make(chan struct{}), wake: make(chan struct{}), done: make(chan struct{}), pool: pool}
	fl.refs.Store(1)
	return fl
}

// begin publishes a chunk about to be committed. After it returns, clients
// may stream from the flight.
func (fl *flight) begin(f fetched, meta *store.ObjectMeta, size int64) {
	fl.began = true
	f.out = outChunk
	fl.out, fl.f, fl.meta = outChunk, f, meta
	fl.bufp = fl.pool.Get().(*[]byte)
	fl.buf = (*fl.bufp)[:size] // pooled buffers hold a whole slice
	close(fl.ready)
}

// release drops a reference. The last one recycles the chunk buffer, so a
// reader must not touch buf after releasing.
func (fl *flight) release() {
	if fl.refs.Add(-1) == 0 && fl.bufp != nil {
		fl.buf = nil
		fl.pool.Put(fl.bufp)
		fl.bufp = nil
	}
}

// finish records the end of the fetch.
func (fl *flight) finish(f fetched, err error) {
	if !fl.began {
		fl.out, fl.f, fl.err = f.out, f, err
		close(fl.ready)
	}
	fl.mu.Lock()
	fl.fin, fl.ferr = true, err
	close(fl.wake)
	fl.mu.Unlock()
	close(fl.done)
}

// complete reports whether the fetch has ended, without waiting.
func (fl *flight) complete() bool {
	select {
	case <-fl.done:
		return true
	default:
		return false
	}
}

// feed is an io.Reader that copies what it reads into the flight's buffer.
type feed struct {
	r  io.Reader
	fl *flight
}

func (t feed) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		fl := t.fl
		fl.mu.Lock()
		copy(fl.buf[fl.n:], p[:n]) // Commit never reads past the chunk size
		fl.n += n
		close(fl.wake)
		fl.wake = make(chan struct{})
		fl.mu.Unlock()
	}
	return n, err
}

// stream writes bytes [from, to) of the chunk to w as they become available.
// A cancelled ctx (the client left) returns ctx's error; a fetch that ends
// before to returns its error.
func (fl *flight) stream(ctx context.Context, w http.ResponseWriter, from, to int64) (int64, error) {
	fl2, _ := w.(http.Flusher)
	pos := from
	for pos < to {
		fl.mu.Lock()
		for int64(fl.n) <= pos && !fl.fin {
			wake := fl.wake
			fl.mu.Unlock()
			select {
			case <-wake:
			case <-ctx.Done():
				return pos - from, ctx.Err()
			}
			fl.mu.Lock()
		}
		n, ferr := int64(fl.n), fl.ferr
		fl.mu.Unlock()
		if n <= pos {
			if ferr == nil {
				ferr = io.ErrUnexpectedEOF
			}
			return pos - from, ferr
		}
		hi := min(n, to)
		if hi == int64(len(fl.buf)) {
			// The last byte of the chunk goes out only once the commit has
			// landed, so a client that finishes can rely on the chunk being
			// cached. The commit follows the last upstream byte immediately.
			select {
			case <-fl.done:
			case <-ctx.Done():
				return pos - from, ctx.Err()
			}
		}
		if _, err := w.Write(fl.buf[pos:hi]); err != nil {
			return pos - from, err
		}
		if fl2 != nil {
			fl2.Flush() // this is what makes the stream live
		}
		pos = hi
	}
	return pos - from, nil
}

// source is where a client reads one chunk from: an open cached file, or a
// flight in progress (holding a reference to it). The zero value means
// "nothing".
type source struct {
	ch store.Chunk
	fl *flight
}

func (s source) empty() bool { return s.ch.File == nil && s.fl == nil }

func (s source) close() {
	if s.ch.File != nil {
		s.ch.Close()
	}
	if s.fl != nil {
		s.fl.release()
	}
}

// join returns the flight fetching chunk key, starting one if there is none,
// with a reference the caller must release. leader reports whether the
// caller started it.
func (h *Handler) join(o *object, key store.Key, idx int64) (fl *flight, leader bool) {
	h.flightMu.Lock()
	defer h.flightMu.Unlock()
	// A registered flight's fetcher still holds its reference, so the count
	// cannot have reached zero.
	if fl = h.flights[key]; fl != nil {
		fl.refs.Add(1)
		if fl.prefetch && fl.claimed.CompareAndSwap(false, true) {
			return fl, true
		}
		h.cfg.Coalesced.Inc()
		return fl, false
	}
	fl = h.startFlight(o, key, idx, false)
	fl.refs.Add(1)
	return fl, true
}

// prefetch starts fetching chunk idx ahead of the client that will need it,
// unless it is cached, already being fetched, or too many prefetches are
// running. It never waits.
func (h *Handler) prefetch(o *object, idx int64, meta *store.ObjectMeta) {
	key := store.ChunkKey(o.key, h.cfg.SliceSize, idx)
	if info, err := h.cfg.Store.Stat(key); err == nil && info.Size == h.chunkLen(idx, meta.Size) {
		return
	}
	if h.prefetching.Add(1) > h.maxPrefetches.Load() {
		h.prefetching.Add(-1)
		return
	}
	h.flightMu.Lock()
	defer h.flightMu.Unlock()
	if h.flights[key] != nil {
		h.prefetching.Add(-1)
		return
	}
	h.startFlight(o, key, idx, true)
}

// startFlight registers and starts a fetch of chunk key. h.flightMu must be
// held.
func (h *Handler) startFlight(o *object, key store.Key, idx int64, prefetch bool) *flight {
	fl := newFlight(&h.chunkBufs)
	fl.prefetch = prefetch
	h.flights[key] = fl
	// The goroutine outlives this request, so it gets a private copy that
	// is also detached from the client's cancellation.
	oc := &object{r: o.r.Clone(context.WithoutCancel(o.r.Context())), id: o.id, key: o.key, flow: o.flow}
	h.fills.Add(1)
	go func() {
		defer h.fills.Done()
		h.runFlight(fl, oc, key, idx)
	}()
	return fl
}

// Wait blocks until every chunk fetch running in the background has
// finished, or ctx ends. Fetches outlive the requests that start them (a
// client may get its last byte before a chunk it only needed for the size is
// committed, and prefetches run ahead of clients), so call Wait after the
// server has stopped and before closing the Store.
func (h *Handler) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { h.fills.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Handler) runFlight(fl *flight, o *object, key store.Key, idx int64) {
	if fl.prefetch {
		defer h.prefetching.Add(-1)
	}
	var (
		f   fetched
		err error
	)
	defer func() {
		if r := recover(); r != nil {
			h.cfg.Log.Error("chunk fetch panicked", "panic", r)
			f, err = fetched{}, errors.New("chunk fetch panicked")
		}
		// Unregister before announcing the end: a request that misses the
		// store after this point finds the committed chunk in it.
		h.flightMu.Lock()
		delete(h.flights, key)
		h.flightMu.Unlock()
		fl.finish(f, err)
		fl.release()
	}()

	// The previous flight for this chunk may have committed between the
	// caller's store lookup and our registration.
	if meta, ok := h.cfg.Store.Object(o.key); ok {
		if info, serr := h.cfg.Store.Stat(key); serr == nil && info.Size == h.chunkLen(idx, meta.Size) {
			f.out = outCached
			return
		}
	}
	f, err = h.fetchChunk(o.r.Context(), o, idx, fl)
	if fl.prefetch && err == nil && f.out != outChunk {
		// No client owns this response; whoever needs the chunk fetches
		// it for itself (see acquire).
		closeFetched(&f)
	}
}

// chunkLen is the size of chunk idx of an object of the given total size.
func (h *Handler) chunkLen(idx, total int64) int64 {
	return chunk.End(idx, h.cfg.SliceSize, total) - chunk.Start(idx, h.cfg.SliceSize) + 1
}
