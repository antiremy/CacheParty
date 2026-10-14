package proxy

import (
	"sync"
	"sync/atomic"
	"time"

	"cacheparty/internal/chunk"
	"cacheparty/internal/store"
)

// Some clients read an object forward one range request at a time, skipping
// the parts they do not need: Battle.net walks its archives this way, with
// one request in flight across dozens of archives. Read-ahead within a
// response cannot help them, since each range is short, so the scan tracker
// remembers where each client's last range on an object ended. A range that
// starts at or after that point, at most scanMaxGap bytes on, continues a
// forward scan, and its response reads ahead past its own end (see
// serveObject). Entries idle for scanIdle are forgotten, and at most
// scanMaxEntries are kept.
const (
	scanMaxGap     = readAheadMaxBytes
	scanIdle       = 30 * time.Second
	scanMaxEntries = 4096
)

type scanKey struct {
	client string
	obj    store.Key
}

// scan is one client's forward read of one object.
type scan struct {
	next   int64        // offset just past the last range served; under scanTracker.mu
	seen   time.Time    // when that range was requested; under scanTracker.mu
	window atomic.Int64 // read-ahead window in chunks, carried between requests
}

type scanTracker struct {
	mu sync.Mutex
	m  map[scanKey]*scan
}

// observe records that client asked for rng of object obj and returns the
// scan it continues, or nil if it does not continue one.
func (t *scanTracker) observe(client string, obj store.Key, rng chunk.Range, now time.Time) *scan {
	k := scanKey{client, obj}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[k]
	if s == nil {
		if t.m == nil {
			t.m = map[scanKey]*scan{}
		}
		if len(t.m) >= scanMaxEntries {
			t.expire(now)
			if len(t.m) >= scanMaxEntries {
				return nil // too many clients scanning at once to track another
			}
		}
		s = &scan{}
		s.window.Store(readAheadMin)
		t.m[k] = s
		s.next, s.seen = rng.End+1, now
		return nil
	}
	cont := now.Sub(s.seen) < scanIdle && rng.Start >= s.next && rng.Start-s.next <= scanMaxGap
	if !cont {
		s.window.Store(readAheadMin)
	}
	s.next, s.seen = rng.End+1, now
	if cont {
		return s
	}
	return nil
}

// expire forgets scans idle for scanIdle. t.mu must be held.
func (t *scanTracker) expire(now time.Time) {
	for k, s := range t.m {
		if now.Sub(s.seen) >= scanIdle {
			delete(t.m, k)
		}
	}
}
