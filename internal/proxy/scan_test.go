package proxy

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"cacheparty/internal/chunk"
	"cacheparty/internal/store"
	"cacheparty/internal/testutil"
)

// getRange fetches rng of path and checks the body against the object.
func (e *cenv) getRange(t *testing.T, path string, body []byte, from, to int) {
	t.Helper()
	resp, got := e.get(t, path, map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", from, to)})
	if resp.StatusCode != 206 || !bytes.Equal(got, body[from:to+1]) {
		t.Fatalf("bytes=%d-%d: status %d, %d bytes", from, to, resp.StatusCode, len(got))
	}
}

// A client reading an object forward in short ranges with gaps between them
// (as Battle.net reads its archives) gets the chunks after its range fetched
// before it asks for them, and the window carries over to its next request.
func TestForwardScanReadsAheadPastRange(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(10 * testSlice)
	e.origin.Set("/f", &testutil.Obj{Body: body})

	e.getRange(t, "/f", body, 0, 99) // starts tracking; nothing to go on yet
	time.Sleep(50 * time.Millisecond)
	eq(t, "upstream ranges after the first range", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999]")

	// A cached range after a gap continues the scan: the next two chunks
	// are fetched although the client did not ask for them.
	e.getRange(t, "/f", body, 300, 399)
	waitFor(t, "chunks 1 and 2 to be cached", func() bool { return e.chunks() == 3 })
	time.Sleep(50 * time.Millisecond) // a fetch beyond the window would have started by now
	eq(t, "upstream ranges after the second range", fmt.Sprint(e.origin.Ranges("/f")),
		"[bytes=0-999 bytes=1000-1999 bytes=2000-2999]")

	e.getRange(t, "/f", body, 2500, 2599)
	waitFor(t, "chunks 3 and 4 to be cached", func() bool { return e.chunks() == 5 })
	time.Sleep(50 * time.Millisecond)
	eq(t, "upstream ranges after the third range", fmt.Sprint(e.origin.Ranges("/f")),
		"[bytes=0-999 bytes=1000-1999 bytes=2000-2999 bytes=3000-3999 bytes=4000-4999]")
}

// Ranges that go backwards are not a forward scan, so nothing is fetched
// past them.
func TestBackwardRangesDoNotReadAhead(t *testing.T) {
	e := newCachedEnv(t)
	body := testutil.Pattern(10 * testSlice)
	e.origin.Set("/f", &testutil.Obj{Body: body})

	e.getRange(t, "/f", body, 5000, 5099)
	e.getRange(t, "/f", body, 2000, 2099)
	e.getRange(t, "/f", body, 0, 99)
	time.Sleep(50 * time.Millisecond)
	eq(t, "upstream ranges", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999 bytes=2000-2999 bytes=5000-5999]")
}

// CACHE_MAX_PREFETCHES=0 turns off reading ahead of forward scans too.
func TestForwardScanHonoursDisabledReadAhead(t *testing.T) {
	e := newCachedEnv(t)
	e.h.SetMaxPrefetches(-1)
	body := testutil.Pattern(10 * testSlice)
	e.origin.Set("/f", &testutil.Obj{Body: body})

	e.getRange(t, "/f", body, 0, 99)
	e.getRange(t, "/f", body, 300, 399)
	e.getRange(t, "/f", body, 1500, 1599)
	time.Sleep(50 * time.Millisecond)
	eq(t, "upstream ranges", fmt.Sprint(e.origin.Ranges("/f")), "[bytes=0-999 bytes=1000-1999]")
}

func TestScanTracker(t *testing.T) {
	var tr scanTracker
	t0 := time.Unix(1000, 0)
	obj := store.ObjectKey("blizzard", "/a")
	r := func(from, to int64) chunk.Range { return chunk.Range{Start: from, End: to} }

	if tr.observe("c1", obj, r(0, 99), t0) != nil {
		t.Fatal("first range continued a scan")
	}
	if tr.observe("c1", obj, r(100, 199), t0) == nil {
		t.Fatal("adjacent range did not continue the scan")
	}
	s := tr.observe("c1", obj, r(200+scanMaxGap, 300+scanMaxGap), t0)
	if s == nil {
		t.Fatal("range scanMaxGap on did not continue the scan")
	}
	s.window.Store(8)
	if tr.observe("c1", obj, r(302+2*scanMaxGap, 400+2*scanMaxGap), t0) != nil {
		t.Fatal("range more than scanMaxGap on continued the scan")
	}
	eq(t, "window after the scan broke", s.window.Load(), int64(readAheadMin))
	if tr.observe("c1", obj, r(0, 99), t0) != nil {
		t.Fatal("backward range continued the scan")
	}
	if tr.observe("c2", obj, r(100, 199), t0) != nil {
		t.Fatal("another client's range continued the scan")
	}
	if tr.observe("c1", obj, r(100, 199), t0.Add(scanIdle)) != nil {
		t.Fatal("range after scanIdle continued the scan")
	}

	// A full tracker makes room by forgetting idle scans, and otherwise
	// tracks nothing new.
	tr = scanTracker{}
	for i := range scanMaxEntries {
		tr.observe(fmt.Sprint(i), obj, r(0, 99), t0)
	}
	tr.observe("late", obj, r(0, 99), t0)
	if tr.observe("late", obj, r(100, 199), t0) != nil {
		t.Fatal("a full tracker tracked another client")
	}
	later := t0.Add(scanIdle)
	tr.observe("late", obj, r(0, 99), later)
	if tr.observe("late", obj, r(100, 199), later) == nil {
		t.Fatal("tracker did not forget idle scans to make room")
	}
	eq(t, "scans tracked", len(tr.m), 1)
}
