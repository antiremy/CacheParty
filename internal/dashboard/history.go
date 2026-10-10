package dashboard

import (
	"sync"
	"time"
)

// History is kept at two resolutions: a second per point for the last 15
// minutes, and a minute per point for the last day.
const (
	finePoints   = 15 * 60
	coarsePoints = 24 * 60
	coarseEvery  = 60 // fine points per coarse point
	rateWindow   = 5  // seconds averaged for per-service rates
)

// point is the bytes moved during one interval.
type point struct {
	Sent, Fetched, SNI  int64
	DiskRead, DiskWrite int64
}

func (p *point) add(q point) {
	p.Sent += q.Sent
	p.Fetched += q.Fetched
	p.SNI += q.SNI
	p.DiskRead += q.DiskRead
	p.DiskWrite += q.DiskWrite
}

func (p point) sub(q point) point {
	return point{p.Sent - q.Sent, p.Fetched - q.Fetched, p.SNI - q.SNI, p.DiskRead - q.DiskRead, p.DiskWrite - q.DiskWrite}
}

// ring is a fixed-size buffer of points, oldest first when read.
type ring struct {
	buf  []point
	next int
	full bool
}

func newRing(n int) *ring { return &ring{buf: make([]point, n)} }

func (r *ring) push(p point) {
	r.buf[r.next] = p
	r.next = (r.next + 1) % len(r.buf)
	r.full = r.full || r.next == 0
}

func (r *ring) points() []point {
	if !r.full {
		return append([]point(nil), r.buf[:r.next]...)
	}
	return append(append([]point(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}

// history samples cumulative byte counts once a second and keeps the
// differences.
type history struct {
	mu      sync.Mutex
	fine    *ring
	coarse  *ring
	pending point // fine points not yet folded into a coarse one
	npend   int
	last    point // cumulative totals at the previous sample
	at      time.Time

	// Per-service and per-game totals at each of the last rateWindow+1
	// samples, and when each last moved.
	svc  [rateWindow + 1]map[flowKey][2]int64
	nsvc int
	seen map[flowKey]time.Time
}

func newHistory() *history {
	return &history{fine: newRing(finePoints), coarse: newRing(coarsePoints), seen: map[flowKey]time.Time{}}
}

// sample records totals (cumulative since start) taken at now.
func (h *history) sample(now time.Time, totals point, services map[flowKey][2]int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.at.IsZero() {
		d := totals.sub(h.last)
		h.fine.push(d)
		h.pending.add(d)
		if h.npend++; h.npend == coarseEvery {
			h.coarse.push(h.pending)
			h.pending, h.npend = point{}, 0
		}
	}
	h.last, h.at = totals, now
	for k, v := range services {
		// The first sample sees every restored total as new; it moved
		// nothing since start, so keep the saved last-active time.
		if h.nsvc > 0 && v != h.svc[0][k] {
			h.seen[k] = now
		}
	}
	copy(h.svc[1:], h.svc[:rateWindow])
	h.svc[0] = services
	h.nsvc = min(h.nsvc+1, len(h.svc))
}

// series returns the points at the given resolution, oldest first, and the
// time the newest one ends.
func (h *history) series(coarse bool) ([]point, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if coarse {
		return h.coarse.points(), h.at.Add(-time.Duration(h.npend) * time.Second)
	}
	return h.fine.points(), h.at
}

// serviceRates returns each service's and game's bytes per second, sent and
// fetched, over the last few samples.
func (h *history) serviceRates() map[flowKey][2]float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[flowKey][2]float64{}
	if h.nsvc < 2 {
		return out
	}
	now, then := h.svc[0], h.svc[h.nsvc-1]
	secs := float64(h.nsvc - 1)
	for s, v := range now {
		old := then[s] // zero for a service that appeared since
		out[s] = [2]float64{float64(v[0]-old[0]) / secs, float64(v[1]-old[1]) / secs}
	}
	return out
}

// lastActive returns when the given stream last moved bytes, or the zero
// time if it has not been seen to.
func (h *history) lastActive(k flowKey) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.seen[k]
}
