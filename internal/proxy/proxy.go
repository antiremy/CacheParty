// Package proxy implements the :80 handler.
//
// GET requests go through the chunk cache (cache.go) when a Store is
// configured; everything else, and any response that cannot be cached
// safely, is passed through and reported as BYPASS, as is any request that
// matches the bypass rules (bypass.go).
package proxy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/domains"
	"cacheparty/internal/metrics"
	"cacheparty/internal/store"
	"cacheparty/internal/upstream"
)

const (
	// Canonical form: Go writes it this way on the wire either way, and a
	// canonical key avoids a per-lookup allocation in Header.Get/Set.
	headerProcessedBy = "X-Lancache-Processed-By"
	headerVersion     = "X-Cacheparty-Version"
	clacksOverhead    = "GNU Terry Pratchett, GNU Zoey -Crabbey- Lough"
	statusClientAbort = 499 // nginx's code for a client that went away
)

// hopByHop headers are never forwarded in either direction.
var hdrClacks = []string{clacksOverhead}

var hopByHop = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// Config carries the handler's dependencies.
type Config struct {
	Hostname string // this machine's name, for loop detection
	Version  string // of the running build, sent as X-CacheParty-Version if set
	Matcher  *domains.Matcher
	Upstream *upstream.Client
	Access   *accesslog.Logger
	Log      *slog.Logger

	// Store enables caching; nil means every request is a pass-through.
	Store store.ChunkStore
	// SliceSize is CACHE_SLICE_SIZE in bytes; required when Store is set.
	SliceSize int64
	// MaxPrefetches caps chunk fetches started ahead of clients, across all
	// clients. 0 selects the default (64); negative disables read-ahead.
	MaxPrefetches int

	// Optional metrics; nil values are ignored.
	Requests         *metrics.Vec     // finished requests by cache status
	BytesSent        *metrics.Vec     // body bytes sent to clients by cache status
	UpstreamRequests *metrics.Counter // chunk fetches sent upstream
	UpstreamErrors   *metrics.Counter // chunk fetches that failed to get a response
	Coalesced        *metrics.Counter // requests that joined an in-progress fetch
	CacheRead        *metrics.Counter // body bytes sent from chunk files on disk
	DroppedChunks    *metrics.Counter // cached chunks dropped because they could not be read

	// Flows, if set, returns the counters for a request's body bytes, given
	// its cache identifier and the client request (for finer breakdowns,
	// such as by game). They are updated while responses are still running,
	// so throughput can be watched during long downloads.
	Flows func(service string, r *http.Request) *Flow
	// App, if set, names what a request downloads within its service (one
	// game, say). It is stored with each object's metadata, so that all
	// of a game's chunks can be found again (see evict.Evictor.Purge).
	App func(service string, r *http.Request) string
	// DashboardPort, if set, is linked from the landing page served to
	// requests addressed to the cache itself (see forCache).
	DashboardPort string
}

// Flow counts body bytes as they move. Either counter may be nil. Bytes are
// also counted in Parent, if set, so one Flow can feed a game and the
// service it belongs to, and in each of Also, which suits breakdowns that
// cut across that chain, such as by client.
type Flow struct {
	Sent    *metrics.Counter // to clients
	Fetched *metrics.Counter // from origins
	Parent  *Flow
	Also    []*Flow
}

func (f *Flow) sent(n int64) {
	for ; f != nil; f = f.Parent {
		f.Sent.Add(n)
		for _, a := range f.Also {
			a.sent(n)
		}
	}
}

func (f *Flow) fetched(n int64) {
	for ; f != nil; f = f.Parent {
		f.Fetched.Add(n)
		for _, a := range f.Also {
			a.fetched(n)
		}
	}
}

// Handler is the caching proxy's http.Handler.
type Handler struct {
	cfg       Config
	bufs      sync.Pool
	chunkBufs sync.Pool // SliceSize buffers for flights

	flightMu sync.Mutex
	flights  map[store.Key]*flight // chunk fetches in progress

	prefetching   atomic.Int32              // running prefetches
	maxPrefetches atomic.Int32              // limit on prefetching; see SetMaxPrefetches
	readAheadMax  int64                     // read-ahead window limit, in chunks
	readingAhead  atomic.Int32              // responses with a read-ahead window
	fills         sync.WaitGroup            // running flights; see Wait
	active        atomic.Int64              // requests being served
	bypassHosts   atomic.Pointer[bypassSet] // BYPASS_DOMAINS; see SetBypassDomains

	processedBy []string // shared header value for requests without one
	version     []string // X-CacheParty-Version value; nil when Version is unset
	sliceLenHdr []string // Content-Length value of a whole chunk
}

// New returns a Handler.
func New(cfg Config) *Handler {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	h := &Handler{
		cfg:          cfg,
		flights:      map[store.Key]*flight{},
		readAheadMax: readAheadMin,
		processedBy:  []string{cfg.Hostname},
		sliceLenHdr:  []string{strconv.FormatInt(cfg.SliceSize, 10)},
	}
	if cfg.Version != "" {
		h.version = []string{cfg.Version}
	}
	h.SetMaxPrefetches(cfg.MaxPrefetches)
	if cfg.SliceSize > 0 {
		h.readAheadMax = max(readAheadMin, readAheadMaxBytes/cfg.SliceSize)
	}
	h.bufs.New = func() any { b := make([]byte, 256<<10); return &b }
	h.chunkBufs.New = func() any { b := make([]byte, cfg.SliceSize); return &b }
	return h
}

// SetMaxPrefetches changes Config.MaxPrefetches while serving. Prefetches
// already running above a lowered limit are left to finish.
func (h *Handler) SetMaxPrefetches(n int) {
	if n == 0 {
		n = defaultMaxPrefetches
	}
	h.maxPrefetches.Store(int32(max(n, 0)))
}

// SetBypassDomains sets the domains whose requests skip the cache (see
// parseBypassDomains for the format). Invalid entries are logged and ignored.
func (h *Handler) SetBypassDomains(list string) {
	set, bad := parseBypassDomains(list)
	for _, e := range bad {
		h.cfg.Log.Warn("ignoring invalid BYPASS_DOMAINS entry", "entry", e)
	}
	h.bypassHosts.Store(set)
}

// Active returns the number of requests being served.
func (h *Handler) Active() int64 { return h.active.Load() }

// record is what the access log needs to know about a finished request.
type record struct {
	status      int
	bytes       int64
	cacheStatus string
	skipLog     bool
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rec := record{}
	h.active.Add(1)
	// Deferred so aborted responses (panic(http.ErrAbortHandler)) are logged.
	defer func() {
		h.active.Add(-1)
		if !rec.skipLog {
			h.logAccess(r, &rec, start)
		}
	}()
	h.serve(w, r, &rec)
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request, rec *record) {
	hdr := w.Header()
	hdr["X-Clacks-Overhead"] = hdrClacks
	if h.version != nil {
		hdr[headerVersion] = h.version
	}

	incoming := r.Header.Get(headerProcessedBy)
	if incoming == h.cfg.Hostname {
		// This request already passed through us: DNS is looping it back.
		h.fail(w, rec, http.StatusLoopDetected, "loop detected")
		return
	}
	if incoming == "" {
		hdr[headerProcessedBy] = h.processedBy
	} else {
		hdr.Set(headerProcessedBy, h.cfg.Hostname+","+incoming)
	}

	if r.URL.Path == "/lancache-heartbeat" && r.Method == http.MethodGet {
		hdr.Set("Access-Control-Expose-Headers", "*")
		hdr.Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusNoContent)
		rec.status, rec.skipLog = http.StatusNoContent, true
		return
	}

	switch {
	case r.Method == http.MethodConnect:
		h.fail(w, rec, http.StatusMethodNotAllowed, "CONNECT not supported")
	case !strings.HasPrefix(r.RequestURI, "/"):
		h.fail(w, rec, http.StatusBadRequest, "unsupported request target")
	case forCache(r):
		h.serveLanding(w, r, rec)
	case r.Method == http.MethodGet && h.cfg.Store != nil && !bypass(r) && !h.bypassHosts.Load().match(r.Host):
		h.serveCached(w, r, rec)
	default:
		h.passThrough(w, r, rec)
	}
}

func (h *Handler) fail(w http.ResponseWriter, rec *record, code int, msg string) {
	http.Error(w, msg, code)
	rec.status = code
}

func (h *Handler) passThrough(w http.ResponseWriter, r *http.Request, rec *record) {
	rec.cacheStatus = "BYPASS"
	out, err := h.upstreamRequest(r.Context(), r)
	if err != nil {
		h.fail(w, rec, http.StatusBadRequest, "bad request")
		return
	}

	t0 := time.Now()
	resp, err := h.cfg.Upstream.Do(out)
	if err != nil {
		if r.Context().Err() != nil {
			rec.status = statusClientAbort
			return
		}
		h.cfg.Log.Error("upstream request failed", "host", r.Host, "uri", r.RequestURI, "err", err)
		h.fail(w, rec, http.StatusBadGateway, "upstream error")
		return
	}
	flow := h.flow(r)
	resp.Body = countingBody{resp.Body, flow}
	h.relay(w, r, rec, resp, time.Since(t0), "BYPASS", flow)
}

// relay sends an upstream response to the client as-is and closes its body.
func (h *Handler) relay(w http.ResponseWriter, r *http.Request, rec *record, resp *http.Response, took time.Duration, cacheStatus string, flow *Flow) {
	defer resp.Body.Close()
	rec.cacheStatus = cacheStatus
	h.writeHeaders(w, resp, took, cacheStatus)
	w.WriteHeader(resp.StatusCode)
	rec.status = resp.StatusCode
	if r.Method == http.MethodHead {
		return
	}
	n, err := h.copyBody(w, resp.Body, flow)
	rec.bytes = n
	if err != nil && r.Context().Err() == nil {
		h.cfg.Log.Error("upstream body failed", "host", r.Host, "uri", r.RequestURI, "err", err)
		// Cut the connection so the client sees a truncated download
		// instead of a body that merely looks complete.
		panic(http.ErrAbortHandler)
	}
}

// flow returns the byte counters for r's cache identifier, or nil.
func (h *Handler) flow(r *http.Request) *Flow {
	if h.cfg.Flows == nil {
		return nil
	}
	return h.cfg.Flows(h.cfg.Matcher.Identify(r.Host, r.Header.Get("User-Agent")), r)
}

// countingBody reports upstream body bytes to a Flow as they are read.
type countingBody struct {
	io.ReadCloser
	flow *Flow
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.flow.fetched(int64(n))
	return n, err
}

// upstreamRequest builds the request to http://<Host><RequestURI>.
func (h *Handler) upstreamRequest(ctx context.Context, r *http.Request) (*http.Request, error) {
	out, err := http.NewRequestWithContext(ctx, r.Method, "http://"+r.Host+r.RequestURI, r.Body)
	if err != nil {
		return nil, err
	}
	out.Host = r.Host
	out.ContentLength = r.ContentLength
	if r.ContentLength == 0 {
		out.Body = http.NoBody
	}
	out.Header = r.Header.Clone()
	stripHopByHop(out.Header)

	ip := remoteIP(r)
	out.Header.Set("X-Real-IP", ip)
	if prior := r.Header.Values("X-Forwarded-For"); len(prior) > 0 {
		out.Header.Set("X-Forwarded-For", strings.Join(prior, ", ")+", "+ip)
	} else {
		out.Header.Set("X-Forwarded-For", ip)
	}
	return out, nil
}

func (h *Handler) writeHeaders(w http.ResponseWriter, resp *http.Response, took time.Duration, cacheStatus string) {
	dst := w.Header()
	for k, vv := range resp.Header {
		dst[k] = vv
	}
	stripHopByHop(dst)
	dst.Del("Etag") // Battle.net fix: never expose upstream ETags
	if _, ok := dst["Content-Type"]; !ok {
		dst["Content-Type"] = nil // stop net/http sniffing a type into the response
	}
	// Set after the copy so upstream values cannot override ours.
	dst.Set(headerProcessedBy, w.Header().Get(headerProcessedBy))
	dst.Set("X-Clacks-Overhead", clacksOverhead)
	if h.version != nil {
		dst[headerVersion] = h.version
	} else {
		dst.Del(headerVersion)
	}
	dst.Set("X-Upstream-Cache-Status", cacheStatus)
	dst.Set("X-Upstream-Status", strconv.Itoa(resp.StatusCode))
	var tb [24]byte
	dst.Set("X-Upstream-Response-Time", string(strconv.AppendFloat(tb[:0], took.Seconds(), 'f', 3, 64)))
}

// copyBody streams src to w through a pooled buffer. The error is from the
// upstream read side only; client write errors just end the transfer.
func (h *Handler) copyBody(w io.Writer, src io.Reader, flow *Flow) (int64, error) {
	bp := h.bufs.Get().(*[]byte)
	defer h.bufs.Put(bp)
	buf := *bp
	var total int64
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			total += int64(wn)
			flow.sent(int64(wn))
			if werr != nil {
				return total, nil
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return total, nil
			}
			return total, rerr
		}
	}
}

func (h *Handler) logAccess(r *http.Request, rec *record, start time.Time) {
	if errors.Is(r.Context().Err(), context.Canceled) && rec.status == 0 {
		rec.status = statusClientAbort
	}
	status := rec.cacheStatus
	if status == "" {
		status = "-"
	}
	h.cfg.Requests.With(status).Inc()
	h.cfg.BytesSent.With(status).Add(rec.bytes)
	if h.cfg.Access == nil {
		return
	}
	ua := r.Header.Get("User-Agent")
	h.cfg.Access.Log(&accesslog.Entry{
		CacheID:       h.cfg.Matcher.Identify(r.Host, ua),
		RemoteAddr:    remoteIP(r),
		XForwardedFor: strings.Join(r.Header.Values("X-Forwarded-For"), ", "),
		Time:          start,
		Method:        r.Method,
		URI:           r.RequestURI,
		Proto:         r.Proto,
		Status:        rec.status,
		BodyBytes:     rec.bytes,
		Referer:       r.Header.Get("Referer"),
		UserAgent:     ua,
		CacheStatus:   rec.cacheStatus,
		Host:          strings.ToLower(domains.StripPort(r.Host)),
		Range:         r.Header.Get("Range"),
	})
}

func remoteIP(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func stripHopByHop(h http.Header) {
	// Headers named by Connection are hop-by-hop too.
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}
