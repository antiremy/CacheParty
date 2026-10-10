// Package evict keeps the chunk cache within its limits.
//
// A sweep removes, in this order:
//
//  1. chunks not accessed for MaxAge (nginx's proxy_cache_path inactive=);
//  2. the least recently used chunks, while the cache holds more than
//     MaxBytes or the disk has less than MinFree bytes free;
//  3. metadata of objects that have no chunks left.
//
// Chunks are evicted individually, not by object: a large download that is
// partly evicted refetches only the missing slices.
package evict

import (
	"cmp"
	"context"
	"log/slog"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cacheparty/internal/chunk"
	"cacheparty/internal/store"
)

// Config tunes an Evictor. Zero MaxBytes, MinFree or MaxAge disable that
// limit.
type Config struct {
	Store     store.ChunkStore
	SliceSize int64
	Dir       string // a path on the cache's file system, for the free-space check

	MaxBytes int64         // CACHE_DISK_SIZE
	MinFree  int64         // MIN_FREE_DISK
	MaxAge   time.Duration // CACHE_MAX_AGE
	Interval time.Duration // between sweeps; default 1 minute

	Log *slog.Logger
	// OnSweep, if set, is called with each sweep's result.
	OnSweep func(Result)

	// Free reports free bytes on Dir's file system; defaults to statfs.
	Free func(dir string) (int64, error)
	// Now is the clock; defaults to time.Now.
	Now func() time.Time
}

// Evictor runs sweeps.
type Evictor struct {
	limitsMu sync.Mutex // guards cfg's limits, which SetLimits changes
	cfg      Config
	// Objects that looked empty on the previous sweep. Metadata is only
	// dropped when an object is empty on two sweeps in a row, so a fill that
	// has stored its metadata but not yet its first chunk is never caught.
	emptyBefore map[store.Key]struct{}
	sweeps      int

	// oldest is the earliest last-access time left by the previous full
	// pass. Chunks committed or read since are newer, so while it is inside
	// MaxAge nothing can have expired and the pass is skipped.
	oldest      int64
	oldestKnown bool

	// purged is set by Purge, so the next sweep checks objects for ones it
	// emptied.
	purged atomic.Bool
}

// objectCheckEvery is how often (in sweeps) objects are checked even though
// no chunk was evicted: chunks also vanish through invalidation and Scrub.
// It is also how often a full expiry pass runs regardless of oldest, in case
// the clock stepped back and stamped chunks older than oldest.
const objectCheckEvery = 60

// maxPasses bounds the passes one sweep makes to free space when chunks it
// picked were read before they could be evicted.
const maxPasses = 3

// New returns an Evictor.
func New(cfg Config) *Evictor {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Free == nil {
		cfg.Free = freeBytes
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Evictor{cfg: cfg, emptyBefore: map[store.Key]struct{}{}}
}

// Limits are the bounds an Evictor enforces; zero disables one.
type Limits struct {
	MaxBytes int64
	MinFree  int64
	MaxAge   time.Duration
}

// SetLimits replaces the limits. The next sweep enforces them; it may run
// concurrently with a sweep.
func (e *Evictor) SetLimits(l Limits) {
	e.limitsMu.Lock()
	e.cfg.MaxBytes, e.cfg.MinFree, e.cfg.MaxAge = l.MaxBytes, l.MinFree, l.MaxAge
	e.limitsMu.Unlock()
}

// Run sweeps every Interval until ctx ends.
func (e *Evictor) Run(ctx context.Context) {
	t := time.NewTicker(e.cfg.Interval)
	defer t.Stop()
	for {
		e.Sweep()
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// Result describes one sweep.
type Result struct {
	Expired, Evicted int   // chunks removed by age, and by size/free space
	Freed            int64 // bytes of chunks removed
	Objects          int   // object metadata records dropped
}

type victim struct {
	k      store.Key
	size   int64
	access int64
}

// Sweep runs one pass. It is not safe to call concurrently with itself.
func (e *Evictor) Sweep() Result {
	res := e.sweep()
	if e.cfg.OnSweep != nil {
		e.cfg.OnSweep(res)
	}
	return res
}

func (e *Evictor) sweep() Result {
	e.limitsMu.Lock()
	cfg := e.cfg
	e.limitsMu.Unlock()
	var (
		res Result
		now = cfg.Now()
	)
	_, used := cfg.Store.Usage()
	need := bytesNeeded(&cfg, used)
	e.sweeps++
	cutoff := now.Add(-cfg.MaxAge).UnixNano()
	expire := cfg.MaxAge > 0 && (!e.oldestKnown || e.oldest < cutoff || e.sweeps%objectCheckEvery == 0)
	if expire || need > 0 {
		e.scan(expire, cutoff, need, &res)
	}

	res.Objects = e.maybeDropObjects(res.Expired+res.Evicted > 0)
	if res.Expired+res.Evicted+res.Objects > 0 {
		chunks, bytes := cfg.Store.Usage()
		cfg.Log.Info("cache sweep", "expired", res.Expired, "evicted", res.Evicted,
			"freed_bytes", res.Freed, "objects_dropped", res.Objects,
			"cached_chunks", chunks, "cached_bytes", bytes)
	}
	return res
}

// scan removes expired chunks (if expire) and then least recently used ones
// until need bytes have been freed.
func (e *Evictor) scan(expire bool, cutoff, need int64, res *Result) {
	st := e.cfg.Store
	for pass := 0; pass < maxPasses; pass++ {
		// Iterate works on a snapshot, so deleting inside it is safe. Only
		// the oldest chunks that cover need are kept as candidates, not one
		// record per cached chunk.
		lru := lruSet{need: need}
		oldest, any := int64(math.MaxInt64), false
		st.Iterate(func(k store.Key, info store.Info) bool {
			if expire && info.LastAccess < cutoff {
				if st.Delete(k) == nil {
					res.Expired++
					res.Freed += info.Size
					need -= info.Size
				}
				return true
			}
			oldest, any = min(oldest, info.LastAccess), true
			if lru.need > 0 {
				lru.offer(victim{k, info.Size, info.LastAccess})
			}
			return true
		})
		e.oldest, e.oldestKnown = oldest, any
		expire = false // a later pass has nothing left to expire
		if need <= 0 {
			return
		}

		skipped := false
		for _, v := range lru.sorted() {
			if need <= 0 {
				return
			}
			// Skip what was read since the snapshot.
			if info, err := st.Stat(v.k); err != nil || info.LastAccess != v.access {
				skipped = true
				continue
			}
			if st.Delete(v.k) == nil {
				res.Evicted++
				res.Freed += v.size
				need -= v.size
			}
		}
		if !skipped {
			return // candidates ran out, so the cache ran out
		}
	}
}

// lruSet keeps the least recently used chunks offered to it whose sizes add
// up to at least need, using memory in proportion to need rather than to
// the cache.
//
// The heap is hand-rolled because container/heap boxes every element it
// pushes, one allocation per chunk offered.
type lruSet struct {
	h     []victim // max-heap on access: most recently used on top
	bytes int64
	need  int64
}

func (s *lruSet) offer(v victim) {
	if s.bytes >= s.need && v.access >= s.h[0].access {
		return // newer than everything kept, which already covers need
	}
	s.h = append(s.h, v)
	s.up(len(s.h) - 1)
	s.bytes += v.size
	for s.bytes-s.h[0].size >= s.need {
		s.bytes -= s.h[0].size
		last := len(s.h) - 1
		s.h[0] = s.h[last]
		s.h = s.h[:last]
		s.down(0)
	}
}

func (s *lruSet) up(i int) {
	for i > 0 {
		p := (i - 1) / 2
		if s.h[p].access >= s.h[i].access {
			return
		}
		s.h[p], s.h[i] = s.h[i], s.h[p]
		i = p
	}
}

func (s *lruSet) down(i int) {
	for {
		c := 2*i + 1
		if c >= len(s.h) {
			return
		}
		if r := c + 1; r < len(s.h) && s.h[r].access > s.h[c].access {
			c = r
		}
		if s.h[i].access >= s.h[c].access {
			return
		}
		s.h[i], s.h[c] = s.h[c], s.h[i]
		i = c
	}
}

// sorted returns the kept chunks, least recently used first.
func (s *lruSet) sorted() []victim {
	slices.SortFunc(s.h, func(a, b victim) int { return cmp.Compare(a.access, b.access) })
	return s.h
}

// headroomDiv: when a limit is hit, free this fraction beyond it, so the next
// sweep does not immediately have to evict again.
const headroomDiv = 20

// bytesNeeded is how many bytes must be freed to satisfy MaxBytes and MinFree.
func bytesNeeded(cfg *Config, used int64) int64 {
	var need int64
	if cfg.MaxBytes > 0 && used > cfg.MaxBytes {
		need = used - (cfg.MaxBytes - cfg.MaxBytes/headroomDiv)
	}
	if cfg.MinFree > 0 {
		free, err := cfg.Free(cfg.Dir)
		if err != nil {
			cfg.Log.Warn("check free disk space", "err", err)
		} else if free < cfg.MinFree {
			need = max(need, cfg.MinFree+cfg.MinFree/headroomDiv-free)
		}
	}
	return need
}

// maybeDropObjects checks objects when chunks were just removed (by this
// sweep or a Purge), when a previous sweep left candidates to confirm, and
// now and then regardless.
func (e *Evictor) maybeDropObjects(removed bool) int {
	if e.purged.Swap(false) {
		removed = true
	}
	if !removed && len(e.emptyBefore) == 0 && e.sweeps%objectCheckEvery != 0 {
		return 0
	}
	return e.dropEmptyObjects()
}

// dropEmptyObjects removes metadata of objects with no chunks.
func (e *Evictor) dropEmptyObjects() int {
	st, slice := e.cfg.Store, e.cfg.SliceSize
	empty := map[store.Key]struct{}{}
	dropped := 0
	st.IterateObjects(func(k store.Key, m *store.ObjectMeta) bool {
		n := chunk.Count(m.Size, slice)
		if n == 0 {
			return true // an empty object has no chunks to evict
		}
		// Last chunk first: LRU evicts the start of a download before its
		// end, so a partly evicted object usually still has its tail and
		// is found non-empty after a probe or two.
		for i := n - 1; i >= 0; i-- {
			if _, err := st.Stat(store.ChunkKey(k, slice, i)); err == nil {
				return true
			}
		}
		if _, seen := e.emptyBefore[k]; seen {
			if st.DeleteObject(k) == nil {
				dropped++
			}
		} else {
			empty[k] = struct{}{}
		}
		return true
	})
	e.emptyBefore = empty
	return dropped
}

// PurgeResult describes what Purge removed, or for a dry run would remove.
type PurgeResult struct {
	Objects int   // objects of the app that had chunks cached
	Chunks  int   // chunks removed
	Bytes   int64 // bytes they held
}

// Purge removes every cached chunk of the objects whose App is app, the way
// a sweep evicts: a client reading one of them keeps its open file, and
// one that needs a removed chunk next fetches it again. Chunks of a slice
// size other than the current one are not found; they age out as usual.
//
// Object metadata is left for the sweep to drop once it has seen the
// object empty twice, as for any object whose chunks are gone, so a fill
// that is just starting keeps the metadata it relies on. A chunk being
// fetched while Purge runs can survive it.
//
// It may run concurrently with a sweep.
func (e *Evictor) Purge(app string, dryRun bool) PurgeResult {
	var res PurgeResult
	if app == "" {
		return res // objects with no recorded app are not one game's
	}
	st, slice := e.cfg.Store, e.cfg.SliceSize
	st.IterateObjects(func(k store.Key, m *store.ObjectMeta) bool {
		if m.App != app {
			return true
		}
		found := false
		for i := range chunk.Count(m.Size, slice) {
			ck := store.ChunkKey(k, slice, i)
			info, err := st.Stat(ck)
			if err != nil || !dryRun && st.Delete(ck) != nil {
				continue
			}
			found = true
			res.Chunks++
			res.Bytes += info.Size
		}
		if found {
			res.Objects++
		}
		return true
	})
	if !dryRun && res.Chunks > 0 {
		e.purged.Store(true)
		chunks, bytes := st.Usage()
		e.cfg.Log.Info("cache purge", "app", app, "objects", res.Objects, "chunks", res.Chunks,
			"freed_bytes", res.Bytes, "cached_chunks", chunks, "cached_bytes", bytes)
	}
	return res
}

func freeBytes(dir string) (int64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return 0, err
	}
	return int64(fs.Bavail) * int64(fs.Bsize), nil
}
