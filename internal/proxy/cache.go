package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cacheparty/internal/chunk"
	"cacheparty/internal/store"
)

// fillIdleTimeout aborts an upstream fetch that has produced no bytes for
// this long. Fetches are detached from the client, so without it a stalled
// origin would pin a goroutine forever.
const fillIdleTimeout = 2 * time.Minute

// Once a response has had to fetch a chunk, the chunks it needs next are
// fetched before the client reaches them, so chunk boundaries do not each
// cost an upstream round trip. The window starts at readAheadMin chunks and
// doubles each time the client reaches a chunk that is still arriving (the
// window is too shallow for the origin's latency), up to readAheadMaxBytes
// or the response's share of the prefetch slots (see readAheadLimit).
// defaultMaxPrefetches caps how many such fetches run at once across all
// clients unless Config.MaxPrefetches says otherwise.
const (
	readAheadMin         = 2
	readAheadMaxBytes    = 32 << 20
	defaultMaxPrefetches = 64
)

// readAheadLimit is the deepest window a response may use right now: the
// window limit, or its even share of the prefetch slots among the responses
// reading ahead, whichever is smaller.
func (h *Handler) readAheadLimit() int64 {
	share := int64(h.maxPrefetches.Load()) / max(int64(h.readingAhead.Load()), 1)
	return max(min(h.readAheadMax, share), 1)
}

// Header values shared by every response; net/http never modifies them.
var (
	hdrAcceptRanges = []string{"bytes"}
	hdrHit          = []string{"HIT"}
	hdrMiss         = []string{"MISS"}
)

// chunkFetchDropHeaders are removed when a client request is turned into a
// fixed-size chunk fetch: we set Range ourselves and keep no validators.
var chunkFetchDropHeaders = []string{
	"Range", "If-Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since",
}

var errChanged = errors.New("object changed upstream")

// object is the request-scoped view of the thing being cached.
type object struct {
	r    *http.Request
	id   string // cache identifier (service)
	key  store.Key
	flow *Flow // nil without Config.Flows
}

// outcome says what a chunk fetch produced.
type outcome uint8

const (
	outChunk  outcome = iota // chunk committed to the store
	outFull                  // upstream ignored Range: resp holds the whole object, body open
	outRelay                 // not cacheable: relay resp to the client as-is
	outUnsat                 // upstream answered 416; total is the object size
	outBypass                // cacheable status but unsafe to cache (e.g. Content-Encoding)
)

// fetched is the result of fetchChunk.
type fetched struct {
	out    outcome
	resp   *http.Response // outFull, outRelay: caller closes the body
	total  int64          // outUnsat
	status int            // upstream status code
	took   time.Duration  // time to response headers
}

// requestPath is the request target without its query string. It is the
// path part of the cache key; it is deliberately not URL-decoded, so
// distinct URLs never share a key.
func requestPath(uri string) string {
	if i := strings.IndexByte(uri, '?'); i >= 0 {
		return uri[:i]
	}
	return uri
}

func (h *Handler) serveCached(w http.ResponseWriter, r *http.Request, rec *record) {
	id := h.cfg.Matcher.Identify(r.Host, r.Header.Get("User-Agent"))
	o := object{r: r, id: id, key: store.ObjectKey(id, requestPath(r.RequestURI))}
	if h.cfg.Flows != nil {
		o.flow = h.cfg.Flows(id, r)
	}
	spec := chunk.Parse(r.Header.Get("Range"))
	if r.Header.Get("If-Range") != "" {
		// We keep no validators that could answer it, so serve the whole
		// object: always correct, merely larger than a client hoped for.
		spec = chunk.Spec{}
	}

	err := h.serveObject(w, rec, &o, spec)
	if errors.Is(err, errChanged) {
		err = h.serveObject(w, rec, &o, spec) // stale entries are gone now
	}
	if err != nil {
		if r.Context().Err() != nil {
			rec.status = statusClientAbort // left while waiting for the fill
			return
		}
		h.cfg.Log.Error("cache fill failed", "host", r.Host, "uri", r.RequestURI, "err", err)
		h.fail(w, rec, http.StatusBadGateway, "upstream error")
	}
}

// serveObject answers the request from the cache, filling missing chunks.
// It returns an error only if nothing has been written to the client yet.
// Failures after the headers are sent abort the connection instead.
func (h *Handler) serveObject(w http.ResponseWriter, rec *record, o *object, spec chunk.Spec) error {
	slice, st := h.cfg.SliceSize, h.cfg.Store

	meta, known := st.Object(o.key)
	if known && meta.App == "" && h.cfg.App != nil {
		h.recordApp(o, meta)
	}
	var (
		rng    chunk.Range
		status chunk.Status
		idx    int64
	)
	if known {
		if rng, status = spec.Resolve(meta.Size); status == chunk.StatusUnsatisfiable {
			h.writeUnsatisfiable(w, rec, meta.Size, hdrHit, nil)
			return nil
		}
		idx, _ = chunk.Span(rng, slice)
	} else {
		// Size unknown: fetch the chunk the client needs first (or chunk 0
		// for a suffix range) to learn it.
		idx = chunk.Index(spec.Hint(), slice)
	}

	src, f, err := h.acquire(o, idx, meta)
	if err != nil {
		return err
	}
	if f != nil && f.out != outChunk {
		return h.finishOther(w, rec, o, spec, f)
	}
	if !known {
		if src.fl != nil {
			meta, known = src.fl.meta, true
		} else {
			meta, known = st.Object(o.key)
		}
		if !known {
			src.close()
			return errors.New("object metadata missing after fill")
		}
		if rng, status = spec.Resolve(meta.Size); status == chunk.StatusUnsatisfiable {
			src.close()
			h.writeUnsatisfiable(w, rec, meta.Size, hdrMiss, f)
			return nil
		}
	}
	first, last := chunk.Span(rng, slice)
	if first != idx {
		src.close() // only learned the size; the chunk stays cached
		src = source{}
	}

	missed := f != nil
	rec.cacheStatus = "HIT"
	if missed {
		rec.cacheStatus = "MISS"
	}
	h.writeCachedHeaders(w, meta, rng, status, f)
	rec.status = http.StatusOK
	if status == chunk.StatusPartial {
		rec.status = http.StatusPartialContent
	}
	w.WriteHeader(rec.status)
	// net/http copies the first 512 body bytes through user space (for
	// content sniffing) unless the header has already gone out. Flushing
	// it lets even small hits go straight to sendfile.
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	var lr io.LimitedReader
	ahead := false // set once a chunk comes from upstream
	window := int64(readAheadMin)
	for i := first; i <= last; i++ {
		if src.empty() {
			if i > first && o.r.Context().Err() != nil {
				return nil // client gone; do not start more fetches for it
			}
			if src, missed, err = h.acquireChunk(o, i, meta); err != nil {
				h.abort(o, "chunk fill failed after response started", err)
			}
			if missed {
				rec.cacheStatus = "MISS"
			}
		}
		if src.fl != nil {
			if !ahead {
				ahead = true
				h.readingAhead.Add(1)
				defer h.readingAhead.Add(-1)
			} else if !src.fl.complete() {
				window = min(window*2, h.readAheadMax)
			}
		}
		if ahead && o.r.Context().Err() == nil {
			// The share is checked on every chunk, so a deep window shrinks
			// when other downloads start and grows back when they finish.
			for j := i + 1; j <= min(i+min(window, h.readAheadLimit()), last); j++ {
				h.prefetch(o, j, meta)
			}
		}
		from, to := chunk.Within(i, slice, rng)
		var n int64
		n, err = h.send(w, &lr, o, src, from, to)
		if err != nil && src.fl == nil && o.r.Context().Err() == nil && unreadable(src.ch, from+n) {
			// The cached file failed, not the client: send the rest of
			// the chunk from a fresh fetch, so the client never notices.
			src.close()
			h.dropChunk(o, i, fmt.Errorf("sending cached chunk: %w", err))
			var m int64
			if src, _, err = h.acquireChunk(o, i, meta); err == nil {
				rec.cacheStatus = "MISS"
				m, err = h.send(w, &lr, o, src, from+n, to)
			}
			n += m
		}
		src.close()
		src = source{}
		rec.bytes += n
		o.flow.sent(n)
		if err != nil {
			if o.r.Context().Err() != nil {
				return nil // client went away
			}
			h.abort(o, "sending chunk", err)
		}
	}
	return nil
}

// acquireChunk is acquire for a response already under way, where only a
// cacheable chunk will do. missed reports whether this call fetched it.
func (h *Handler) acquireChunk(o *object, idx int64, meta *store.ObjectMeta) (src source, missed bool, err error) {
	src, f, err := h.acquire(o, idx, meta)
	if err == nil && f != nil && f.out != outChunk {
		closeFetched(f)
		err = fmt.Errorf("unexpected upstream response (%d) for chunk %d", f.status, idx)
	}
	return src, f != nil, err
}

// send writes bytes [from, to) of a chunk to w from wherever src reads.
func (h *Handler) send(w http.ResponseWriter, lr *io.LimitedReader, o *object, src source, from, to int64) (int64, error) {
	if src.fl != nil {
		return src.fl.stream(o.r.Context(), w, from, to)
	}
	n, err := sendChunk(w, lr, src.ch, from, to)
	h.cfg.CacheRead.Add(n)
	return n, err
}

// app is the App to record with o's metadata ("" without Config.App).
func (h *Handler) app(o *object) string {
	if h.cfg.App == nil {
		return ""
	}
	return h.cfg.App(o.id, o.r)
}

// recordApp adds o's App to metadata stored before objects carried one, so
// a game cached by an older version becomes purgeable as it is played.
func (h *Handler) recordApp(o *object, meta *store.ObjectMeta) {
	if app := h.app(o); app != "" {
		m := *meta // stored metadata is immutable
		m.App = app
		h.cfg.Store.PutObject(o.key, &m)
	}
}

// abort ends a response that has already started so the client cannot
// mistake it for a complete download.
func (h *Handler) abort(o *object, msg string, err error) {
	h.cfg.Log.Error(msg, "host", o.r.Host, "uri", o.r.RequestURI, "err", err)
	panic(http.ErrAbortHandler)
}

// sendChunk copies bytes [from, to) of a chunk to w. With a plain file and
// a ResponseWriter whose header is already written, io.Copy hands the file
// straight to the socket (sendfile); nothing is read into user space.
//
// lr is scratch space owned by the caller, so a response spanning many
// chunks allocates one LimitedReader instead of one per chunk.
func sendChunk(w io.Writer, lr *io.LimitedReader, ch store.Chunk, from, to int64) (int64, error) {
	if off := ch.Offset + from; off != 0 {
		if _, err := ch.File.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
	}
	want := to - from
	lr.R, lr.N = ch.File, want
	n, err := io.Copy(w, lr)
	if err == nil && n != want {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

// dropChunk removes cached chunk idx of o because reading it failed with
// err, so the next acquire fetches it again. Readers that already have the
// file open keep reading it, as with eviction.
func (h *Handler) dropChunk(o *object, idx int64, err error) {
	h.cfg.Log.Warn("cached chunk unreadable; dropping it and fetching it again",
		"host", o.r.Host, "uri", o.r.RequestURI, "chunk", idx, "err", err)
	h.cfg.DroppedChunks.Inc()
	if err := h.cfg.Store.Delete(store.ChunkKey(o.key, h.cfg.SliceSize, idx)); err != nil {
		// A commit renames over the file anyway.
		h.cfg.Log.Warn("delete unreadable chunk", "err", err)
	}
}

// fileAtFault reports whether a failure to open a cached chunk means the
// file is bad. Running out of descriptors or memory says nothing about the
// file, and dropping chunks for it would empty a busy cache.
func fileAtFault(err error) bool {
	return !errors.Is(err, syscall.EMFILE) && !errors.Is(err, syscall.ENFILE) && !errors.Is(err, syscall.ENOMEM)
}

// unreadable reports whether a chunk that stopped sending at offset off
// (from the start of the chunk) did so because of its file rather than the
// client: the file must be shorter than the index says, or failing reads.
func unreadable(ch store.Chunk, off int64) bool {
	var b [1]byte
	_, err := ch.File.ReadAt(b[:], ch.Offset+off)
	return err != nil
}

// acquire returns where to read a chunk from, fetching it from upstream
// first if it is not cached. If meta is nil the object size is not known, so
// the chunk is always fetched (that is how the size is learned). Concurrent
// callers for the same chunk share one fetch (see flight); only the caller
// that started it gets a non-nil *fetched, so only it is reported as a MISS.
// When the upstream response is not a cacheable chunk, the returned *fetched
// says what to do and the source is empty.
func (h *Handler) acquire(o *object, idx int64, meta *store.ObjectMeta) (source, *fetched, error) {
	slice, st := h.cfg.SliceSize, h.cfg.Store
	key := store.ChunkKey(o.key, slice, idx)

	if meta != nil {
		want := h.chunkLen(idx, meta.Size)
		ch, err := st.Get(key)
		switch {
		case err == nil && ch.Size == want:
			return source{ch: ch}, nil, nil
		case err == nil:
			// Cached under different assumptions (cannot happen with
			// slice-keyed chunks unless the file was tampered with).
			ch.Close()
			h.dropChunk(o, idx, fmt.Errorf("cached chunk is %d bytes, want %d", ch.Size, want))
		case errors.Is(err, store.ErrNotFound):
		case fileAtFault(err):
			h.dropChunk(o, idx, err)
		default:
			return source{}, nil, err
		}
	}

	fl, leader := h.join(o, key, idx)
	select {
	case <-fl.ready:
	case <-o.r.Context().Done():
		fl.release()
		return source{}, nil, o.r.Context().Err()
	}
	if fl.err == nil && fl.out == outChunk {
		var f *fetched
		if leader {
			f = &fl.f
		}
		return source{fl: fl}, f, nil // the source keeps the reference
	}
	// Only the chunk buffer is recycled; the outcome stays readable.
	fl.release()
	if fl.err != nil {
		return source{}, nil, fl.err
	}
	switch fl.out {
	case outCached:
		ch, err := st.Get(key)
		return source{ch: ch}, nil, err
	}
	// Not a chunk. The response (if it has a body) belongs to the client
	// that started the fetch; the others fetch for themselves. A prefetch
	// has already closed its response, so everyone does.
	if leader && !fl.prefetch {
		return source{}, &fl.f, nil
	}
	f, err := h.fetchChunk(context.WithoutCancel(o.r.Context()), o, idx, nil)
	if err != nil {
		return source{}, nil, err
	}
	if f.out != outChunk {
		return source{}, &f, nil
	}
	ch, err := st.Get(key)
	if err != nil {
		return source{}, nil, fmt.Errorf("chunk vanished after commit: %w", err)
	}
	return source{ch: ch}, &f, nil
}

// fetchChunk is the single place a chunk is fetched from upstream and
// committed. ctx must not be tied to the client request. fl, if not nil, is
// the flight to publish the chunk's bytes to while it is committed.
func (h *Handler) fetchChunk(ctx context.Context, o *object, idx int64, fl *flight) (fetched, error) {
	slice, st := h.cfg.SliceSize, h.cfg.Store
	start := chunk.Start(idx, slice)
	end := start + slice - 1

	ctx, cancel := context.WithCancel(ctx)
	idle := time.AfterFunc(fillIdleTimeout, cancel)
	req, err := h.chunkRequest(ctx, o.r, start, end)
	if err != nil {
		idle.Stop()
		cancel()
		return fetched{}, err
	}
	t0 := time.Now()
	h.cfg.UpstreamRequests.Inc()
	resp, err := h.cfg.Upstream.Do(req)
	if err != nil {
		h.cfg.UpstreamErrors.Inc()
		idle.Stop()
		cancel()
		return fetched{}, err
	}
	resp.Body = &idleBody{ReadCloser: resp.Body, timer: idle, cancel: cancel, flow: o.flow}
	f := fetched{resp: resp, status: resp.StatusCode, took: time.Since(t0)}

	switch {
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		defer resp.Body.Close()
		if _, _, total, ok := chunk.ParseContentRange(resp.Header.Get("Content-Range")); ok && total >= 0 {
			f.out, f.total = outUnsat, total
			return f, nil
		}
		// Relaying it would answer a request the client never made.
		return f, errors.New("upstream sent 416 without a usable Content-Range")
	case resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent:
		f.out = outRelay // 404, 5xx, ...: never cached
		return f, nil
	case encoded(resp):
		// A compressed body's byte ranges are not the object's.
		resp.Body.Close()
		f.out = outBypass
		return f, nil
	case resp.StatusCode == http.StatusOK:
		if resp.ContentLength <= 0 {
			f.out = outRelay // unknown or zero length: nothing to chunk
			return f, nil
		}
		f.out = outFull
		return f, nil
	}

	// 206: validate it is exactly the chunk asked for before storing it.
	defer resp.Body.Close()
	cs, ce, total, ok := chunk.ParseContentRange(resp.Header.Get("Content-Range"))
	if !ok || total <= 0 || cs != start || ce != min(end, total-1) ||
		(resp.ContentLength >= 0 && resp.ContentLength != ce-cs+1) {
		return f, fmt.Errorf("upstream sent unusable 206 (Content-Range %q, wanted bytes %d-%d)",
			resp.Header.Get("Content-Range"), start, end)
	}
	meta := store.NewObjectMeta(total, resp.Header.Get("Content-Type"), resp.Header.Get("Last-Modified"))
	old, had := st.Object(o.key)
	if had && !sameObject(old, meta) {
		h.invalidate(o.key, old)
		return f, errChanged
	}
	if !had {
		meta.App = h.app(o)
		// Before the commit, so clients streaming from fl can rely on it.
		if err := st.PutObject(o.key, meta); err != nil {
			return f, err
		}
	}
	var body io.Reader = resp.Body
	if fl != nil {
		fl.begin(f, meta, ce-cs+1)
		body = feed{resp.Body, fl}
	}
	if err := st.Commit(store.ChunkKey(o.key, slice, idx), ce-cs+1, body); err != nil {
		if !had {
			st.DeleteObject(o.key) // a failed fill leaves nothing behind
		}
		return f, err
	}
	f.out = outChunk
	return f, nil
}

// chunkRequest turns the client's request into the upstream request for one
// chunk: same URL and headers, but with our Range, and no conditionals.
func (h *Handler) chunkRequest(ctx context.Context, r *http.Request, start, end int64) (*http.Request, error) {
	out, err := h.upstreamRequest(ctx, r)
	if err != nil {
		return nil, err
	}
	out.Body, out.ContentLength = http.NoBody, 0
	for _, k := range chunkFetchDropHeaders {
		out.Header.Del(k)
	}
	var b [48]byte
	rng := strconv.AppendInt(append(b[:0], "bytes="...), start, 10)
	rng = strconv.AppendInt(append(rng, '-'), end, 10)
	out.Header.Set("Range", string(rng))
	// Byte ranges of a compressed body are not the object's byte ranges.
	out.Header.Set("Accept-Encoding", "identity")
	return out, nil
}

func encoded(resp *http.Response) bool {
	ce := resp.Header.Get("Content-Encoding")
	return ce != "" && !strings.EqualFold(ce, "identity")
}

// sameObject reports whether two descriptions can be the same object.
// Last-Modified is compared only when both sides have one.
func sameObject(a, b *store.ObjectMeta) bool {
	if a.Size != b.Size {
		return false
	}
	return a.LastModified == "" || b.LastModified == "" || a.LastModified == b.LastModified
}

// invalidate forgets everything cached for an object whose upstream copy
// changed, so chunks of two versions are never mixed.
func (h *Handler) invalidate(key store.Key, old *store.ObjectMeta) {
	st := h.cfg.Store
	h.cfg.Log.Info("upstream object changed; dropping cached chunks", "chunks", chunk.Count(old.Size, h.cfg.SliceSize))
	for i := int64(0); i < chunk.Count(old.Size, h.cfg.SliceSize); i++ {
		if err := st.Delete(store.ChunkKey(key, h.cfg.SliceSize, i)); err != nil {
			h.cfg.Log.Warn("delete stale chunk", "err", err)
		}
	}
	st.DeleteObject(key)
}

// finishOther completes a request whose first fetch was not a plain chunk.
func (h *Handler) finishOther(w http.ResponseWriter, rec *record, o *object, spec chunk.Spec, f *fetched) error {
	switch f.out {
	case outRelay:
		h.relay(w, o.r, rec, f.resp, f.took, "MISS", o.flow)
		return nil
	case outBypass:
		h.passThrough(w, o.r, rec)
		return nil
	case outUnsat:
		rng, status := spec.Resolve(f.total)
		switch status {
		case chunk.StatusUnsatisfiable:
			h.writeUnsatisfiable(w, rec, f.total, hdrMiss, f)
		case chunk.StatusFull: // only an empty object resolves to a full response
			rec.cacheStatus, rec.status = "MISS", http.StatusOK
			hdr := w.Header()
			hdr["Content-Type"] = nil
			hdr["Content-Length"] = []string{"0"}
			h.writeUpstreamInfo(hdr, hdrMiss, f)
			w.WriteHeader(http.StatusOK)
		default:
			return fmt.Errorf("upstream said 416 for range %v of a %d-byte object", rng, f.total)
		}
		return nil
	default: // outFull
		return h.serveFull(w, rec, o, spec, f)
	}
}

func closeFetched(f *fetched) {
	if f.resp != nil {
		f.resp.Body.Close()
	}
}

// serveFull handles an upstream that ignored our Range header and is
// sending the whole object. The body streams to the client while it is cut
// into chunks and cached. Reading stops after the last chunk the client
// needs, so a range request near the start does not drag the whole file.
func (h *Handler) serveFull(w http.ResponseWriter, rec *record, o *object, spec chunk.Spec, f *fetched) error {
	slice, st := h.cfg.SliceSize, h.cfg.Store
	resp := f.resp
	defer resp.Body.Close()

	size := resp.ContentLength
	meta := store.NewObjectMeta(size, resp.Header.Get("Content-Type"), resp.Header.Get("Last-Modified"))
	meta.App = h.app(o)
	if old, ok := st.Object(o.key); ok && !sameObject(old, meta) {
		h.invalidate(o.key, old)
	}
	rng, status := spec.Resolve(size)
	if status == chunk.StatusUnsatisfiable {
		h.writeUnsatisfiable(w, rec, size, hdrMiss, f)
		return nil
	}
	if err := st.PutObject(o.key, meta); err != nil {
		return err
	}
	_, last := chunk.Span(rng, slice)

	rec.cacheStatus = "MISS"
	h.writeCachedHeaders(w, meta, rng, status, f)
	rec.status = http.StatusOK
	if status == chunk.StatusPartial {
		rec.status = http.StatusPartialContent
	}
	w.WriteHeader(rec.status)

	cw := &clientWriter{w: w, rng: rng, flow: o.flow}
	for i := int64(0); i <= last; i++ {
		n := chunk.End(i, slice, size) - chunk.Start(i, slice) + 1
		body := io.TeeReader(io.LimitReader(resp.Body, n), cw)
		if err := st.Commit(store.ChunkKey(o.key, slice, i), n, body); err != nil {
			rec.bytes = cw.sent
			if cw.sent < rng.Len() && o.r.Context().Err() == nil {
				h.abort(o, "streaming full upstream response", err)
			}
			h.cfg.Log.Warn("caching full upstream response", "host", o.r.Host, "uri", o.r.RequestURI, "err", err)
			return nil
		}
	}
	rec.bytes = cw.sent
	return nil
}

// clientWriter forwards the part of a sequential byte stream that falls in
// rng to w. A failed client write never fails the stream: the cache fill
// must carry on for a client that has gone away.
type clientWriter struct {
	w    io.Writer
	rng  chunk.Range
	pos  int64 // absolute offset of the next byte of the stream
	sent int64
	dead bool
	flow *Flow
}

func (c *clientWriter) Write(p []byte) (int, error) {
	lo, hi := max(c.pos, c.rng.Start), min(c.pos+int64(len(p))-1, c.rng.End)
	if !c.dead && hi >= lo {
		n, err := c.w.Write(p[lo-c.pos : hi-c.pos+1])
		c.sent += int64(n)
		c.flow.sent(int64(n))
		if err != nil {
			c.dead = true
		}
	}
	c.pos += int64(len(p))
	return len(p), nil
}

// writeCachedHeaders sets the headers for a response built from the cache.
// Only Content-Type and Last-Modified are remembered from upstream.
func (h *Handler) writeCachedHeaders(w http.ResponseWriter, meta *store.ObjectMeta, rng chunk.Range, status chunk.Status, f *fetched) {
	hdr := w.Header()
	hdr["Content-Type"] = meta.ContentTypeHeader() // nil still stops net/http sniffing one
	if lm := meta.LastModifiedHeader(); lm != nil {
		hdr["Last-Modified"] = lm
	}
	hdr["Accept-Ranges"] = hdrAcceptRanges
	var nb [64]byte
	if rng.Len() == h.cfg.SliceSize {
		hdr["Content-Length"] = h.sliceLenHdr
	} else {
		hdr.Set("Content-Length", string(strconv.AppendInt(nb[:0], rng.Len(), 10)))
	}
	if status == chunk.StatusPartial {
		hdr.Set("Content-Range", string(chunk.AppendContentRange(nb[:0], rng, meta.Size)))
	}
	cs := hdrHit
	if f != nil {
		cs = hdrMiss
	}
	h.writeUpstreamInfo(hdr, cs, f)
}

// writeUpstreamInfo sets the X-Upstream-* headers. f is nil for a hit.
func (h *Handler) writeUpstreamInfo(hdr http.Header, cacheStatus []string, f *fetched) {
	hdr["X-Upstream-Cache-Status"] = cacheStatus
	if f == nil {
		return
	}
	var nb [24]byte
	hdr.Set("X-Upstream-Status", strconv.Itoa(f.status))
	hdr.Set("X-Upstream-Response-Time", string(strconv.AppendFloat(nb[:0], f.took.Seconds(), 'f', 3, 64)))
}

func (h *Handler) writeUnsatisfiable(w http.ResponseWriter, rec *record, size int64, cacheStatus []string, f *fetched) {
	hdr := w.Header()
	hdr["Content-Type"] = nil
	hdr["Accept-Ranges"] = hdrAcceptRanges
	hdr["Content-Length"] = []string{"0"}
	var nb [40]byte
	hdr.Set("Content-Range", string(chunk.AppendUnsatisfiable(nb[:0], size)))
	h.writeUpstreamInfo(hdr, cacheStatus, f)
	w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	rec.status, rec.cacheStatus = http.StatusRequestedRangeNotSatisfiable, cacheStatus[0]
}

// idleBody resets a watchdog on every read; if no bytes arrive for
// fillIdleTimeout the fetch's context is cancelled.
type idleBody struct {
	io.ReadCloser
	timer  *time.Timer
	cancel context.CancelFunc
	flow   *Flow
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(fillIdleTimeout)
		b.flow.fetched(int64(n))
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	err := b.ReadCloser.Close()
	b.cancel()
	return err
}
