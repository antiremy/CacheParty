package evict

import (
	"bytes"
	"io"
	"log/slog"
	"math/rand"
	"slices"
	"testing"
	"time"

	"cacheparty/internal/store"
)

const slice = 100

type env struct {
	st   *store.FileStore
	now  time.Time
	free int64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(t.TempDir(), store.Options{FlushInterval: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &env{st: st, now: time.Now(), free: 1 << 40}
}

func (e *env) evictor(c Config) *Evictor {
	c.Store, c.SliceSize, c.Free = e.st, slice, func(string) (int64, error) { return e.free, nil }
	if c.Now == nil {
		c.Now = func() time.Time { return e.now }
	}
	c.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(c)
}

// put stores a whole object of n chunks (the last one short if short).
func (e *env) put(t *testing.T, name string, chunks int, short int64) (store.Key, []store.Key) {
	t.Helper()
	obj := store.ObjectKey("id", name)
	size := int64(chunks) * slice
	if short > 0 {
		size = int64(chunks-1)*slice + short
	}
	if err := e.st.PutObject(obj, store.NewObjectMeta(size, "", "")); err != nil {
		t.Fatal(err)
	}
	var keys []store.Key
	for i := 0; i < chunks; i++ {
		n := int64(slice)
		if i == chunks-1 && short > 0 {
			n = short
		}
		k := store.ChunkKey(obj, slice, int64(i))
		if err := e.st.Commit(k, n, bytes.NewReader(make([]byte, n))); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k)
	}
	return obj, keys
}

func (e *env) has(k store.Key) bool { _, err := e.st.Stat(k); return err == nil }

// The store stamps access times with the real clock, so age tests use real,
// short intervals.
func TestExpiresChunksNotAccessedForMaxAge(t *testing.T) {
	e := newEnv(t)
	_, old := e.put(t, "/old", 2, 0)
	time.Sleep(200 * time.Millisecond)
	_, fresh := e.put(t, "/fresh", 1, 0)
	ev := e.evictor(Config{MaxAge: 150 * time.Millisecond, Now: time.Now})
	res := ev.Sweep()
	if res.Expired != 2 || e.has(old[0]) || e.has(old[1]) || !e.has(fresh[0]) {
		t.Fatalf("res = %+v; old %v %v fresh %v", res, e.has(old[0]), e.has(old[1]), e.has(fresh[0]))
	}
}

func TestAccessKeepsChunkAlive(t *testing.T) {
	e := newEnv(t)
	_, keys := e.put(t, "/a", 1, 0)
	time.Sleep(120 * time.Millisecond)
	c, err := e.st.Get(keys[0])
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	time.Sleep(60 * time.Millisecond) // 180 ms since the write, 60 since the read
	ev := e.evictor(Config{MaxAge: 150 * time.Millisecond, Now: time.Now})
	if res := ev.Sweep(); res.Expired != 0 || !e.has(keys[0]) {
		t.Fatalf("recently accessed chunk expired: %+v", res)
	}
}

func TestEvictsLeastRecentlyUsedOverMaxBytes(t *testing.T) {
	e := newEnv(t)
	_, a := e.put(t, "/a", 3, 0) // 300 bytes
	time.Sleep(2 * time.Millisecond)
	_, b := e.put(t, "/b", 3, 0)
	time.Sleep(2 * time.Millisecond)
	c, err := e.st.Get(a[0]) // a[0] becomes the most recently used
	if err != nil {
		t.Fatal(err)
	}
	c.Close()

	// 600 bytes held, limit 400: must free at least 200 (+5% headroom of 400 = 20).
	ev := e.evictor(Config{MaxBytes: 400})
	res := ev.Sweep()
	_, used := e.st.Usage()
	if used > 400-20 {
		t.Fatalf("used = %d after sweep, want <= 380; res %+v", used, res)
	}
	if !e.has(a[0]) {
		t.Error("most recently used chunk was evicted")
	}
	if e.has(a[1]) || e.has(a[2]) {
		t.Error("older chunks of /a should have gone before /b's")
	}
	_ = b
	if res.Evicted == 0 || res.Expired != 0 {
		t.Errorf("res = %+v", res)
	}
}

func TestNothingEvictedUnderLimits(t *testing.T) {
	e := newEnv(t)
	_, keys := e.put(t, "/a", 2, 0)
	ev := e.evictor(Config{MaxBytes: 1000, MinFree: 1 << 20, MaxAge: time.Hour})
	if res := ev.Sweep(); res.Expired+res.Evicted+res.Objects != 0 || !e.has(keys[0]) {
		t.Fatalf("res = %+v", res)
	}
}

func TestEvictsForMinFreeDisk(t *testing.T) {
	e := newEnv(t)
	e.put(t, "/a", 4, 0) // 400 bytes
	e.free = 1000
	ev := e.evictor(Config{MinFree: 1200}) // short by 200 (+60 headroom)
	res := ev.Sweep()
	if res.Freed < 260 {
		t.Fatalf("freed %d, want >= 260", res.Freed)
	}
}

func TestDropsObjectMetadataWhenLastChunkGone(t *testing.T) {
	e := newEnv(t)
	obj, keys := e.put(t, "/a", 2, 50)
	ev := e.evictor(Config{MaxAge: time.Hour})
	e.now = e.now.Add(2 * time.Hour)
	ev.Sweep()
	if _, ok := e.st.Object(obj); !ok {
		t.Fatal("metadata dropped on the first sweep that saw it empty; want a second sweep to confirm")
	}
	if res := ev.Sweep(); res.Objects != 1 {
		t.Fatalf("second sweep: %+v", res)
	}
	if _, ok := e.st.Object(obj); ok {
		t.Error("metadata still present")
	}
	_ = keys
}

func TestKeepsObjectMetadataWhileAnyChunkRemains(t *testing.T) {
	e := newEnv(t)
	obj, keys := e.put(t, "/a", 3, 0)
	e.st.Delete(keys[0])
	e.st.Delete(keys[1])
	ev := e.evictor(Config{MaxAge: time.Hour})
	for i := 0; i < 3; i++ {
		ev.Sweep()
	}
	if _, ok := e.st.Object(obj); !ok {
		t.Fatal("metadata dropped while the last chunk is still cached")
	}
}

func TestMetadataOfObjectBeingFilledSurvivesOneSweep(t *testing.T) {
	e := newEnv(t)
	obj := store.ObjectKey("id", "/filling")
	e.st.PutObject(obj, store.NewObjectMeta(300, "", "")) // fill started, no chunk yet
	ev := e.evictor(Config{MaxAge: time.Hour})
	ev.Sweep()
	k := store.ChunkKey(obj, slice, 0)
	e.st.Commit(k, slice, bytes.NewReader(make([]byte, slice))) // first chunk lands
	ev.Sweep()
	if _, ok := e.st.Object(obj); !ok {
		t.Fatal("metadata of an object whose first chunk just landed was dropped")
	}
}

// countingStore counts full passes over the chunks and can act right after
// each one.
type countingStore struct {
	store.ChunkStore
	iterations int
	after      func()
}

func (c *countingStore) Iterate(fn func(store.Key, store.Info) bool) {
	c.iterations++
	c.ChunkStore.Iterate(fn)
	if c.after != nil {
		c.after()
	}
}

func TestSkipsExpiryPassWhileNothingCanHaveExpired(t *testing.T) {
	e := newEnv(t)
	_, keys := e.put(t, "/a", 2, 0)
	cs := &countingStore{ChunkStore: e.st}
	ev := e.evictor(Config{MaxAge: time.Hour})
	ev.cfg.Store = cs

	ev.Sweep() // learns the oldest access time
	ev.Sweep()
	ev.Sweep()
	if cs.iterations != 1 {
		t.Fatalf("%d passes over the chunks, want 1", cs.iterations)
	}
	e.now = e.now.Add(2 * time.Hour)
	if res := ev.Sweep(); res.Expired != 2 || e.has(keys[0]) || cs.iterations != 2 {
		t.Fatalf("after MaxAge: res %+v, %d passes", res, cs.iterations)
	}
}

func TestFullExpiryPassRunsPeriodically(t *testing.T) {
	e := newEnv(t)
	e.put(t, "/a", 1, 0)
	cs := &countingStore{ChunkStore: e.st}
	ev := e.evictor(Config{MaxAge: time.Hour})
	ev.cfg.Store = cs
	for i := 0; i < objectCheckEvery; i++ {
		ev.Sweep()
	}
	if cs.iterations != 2 {
		t.Fatalf("%d passes in %d sweeps, want 2", cs.iterations, objectCheckEvery)
	}
}

// A chunk read between the snapshot and its eviction is spared, and another
// pass finds the next victim.
func TestEvictionRetriesWhenVictimIsRead(t *testing.T) {
	e := newEnv(t)
	_, a := e.put(t, "/a", 1, 0)
	time.Sleep(2 * time.Millisecond)
	_, b := e.put(t, "/b", 1, 0)
	time.Sleep(2 * time.Millisecond)
	_, c := e.put(t, "/c", 1, 0)
	cs := &countingStore{ChunkStore: e.st}
	cs.after = func() {
		if cs.iterations == 1 {
			ch, err := e.st.Get(a[0]) // the LRU victim is read just in time
			if err != nil {
				t.Fatal(err)
			}
			ch.Close()
		}
	}
	ev := e.evictor(Config{MaxBytes: 250}) // 300 held: one chunk must go
	ev.cfg.Store = cs
	res := ev.Sweep()
	if !e.has(a[0]) || e.has(b[0]) || !e.has(c[0]) {
		t.Errorf("a %v b %v c %v, want only b evicted", e.has(a[0]), e.has(b[0]), e.has(c[0]))
	}
	if res.Evicted != 1 || cs.iterations != 2 {
		t.Errorf("res %+v after %d passes", res, cs.iterations)
	}
}

func TestLRUSetKeepsOldestCoveringNeed(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for trial := 0; trial < 100; trial++ {
		var all []victim
		var total int64
		for i, access := range rng.Perm(200) {
			v := victim{size: 1 + rng.Int63n(100), access: int64(access)}
			v.k[0], v.k[1] = byte(i), byte(i>>8)
			all = append(all, v)
			total += v.size
		}
		need := 1 + rng.Int63n(total)
		s := lruSet{need: need}
		for _, v := range all {
			s.offer(v)
		}
		got := s.sorted()

		// Expected: the shortest least-recently-used prefix covering need.
		slices.SortFunc(all, func(x, y victim) int { return int(x.access - y.access) })
		var sum int64
		n := 0
		for sum < need {
			sum += all[n].size
			n++
		}
		if !slices.Equal(got, all[:n]) {
			t.Fatalf("trial %d: kept %d chunks, want the %d least recently used (need %d)", trial, len(got), n, need)
		}
	}
}

func TestSetLimitsAppliesOnNextSweep(t *testing.T) {
	e := newEnv(t)
	e.put(t, "a", 10, 0)
	ev := e.evictor(Config{})
	if r := ev.Sweep(); r.Evicted != 0 {
		t.Fatalf("no limits: evicted %d", r.Evicted)
	}
	ev.SetLimits(Limits{MaxBytes: 5 * slice})
	ev.Sweep()
	if _, used := e.st.Usage(); used > 5*slice {
		t.Fatalf("after SetLimits: %d bytes cached, limit %d", used, 5*slice)
	}
}

// putApp stores an object of n chunks belonging to app.
func (e *env) putApp(t *testing.T, name, app string, chunks int) (store.Key, []store.Key) {
	t.Helper()
	obj, keys := e.put(t, name, chunks, 0)
	m, _ := e.st.Object(obj)
	m2 := *m
	m2.App = app
	e.st.PutObject(obj, &m2)
	return obj, keys
}

func TestPurgeRemovesOnlyThatAppsChunks(t *testing.T) {
	e := newEnv(t)
	_, a1 := e.putApp(t, "/depot/1/chunk/a", "steam/depot/1", 3)
	_, a2 := e.putApp(t, "/depot/1/chunk/b", "steam/depot/1", 2)
	_, other := e.putApp(t, "/depot/2/chunk/a", "steam/depot/2", 2)
	_, untagged := e.put(t, "/depot/1/chunk/c", 1, 0)
	e.st.Delete(a2[1]) // already partly evicted
	ev := e.evictor(Config{})

	want := PurgeResult{Objects: 2, Chunks: 4, Bytes: 4 * slice}
	if got := ev.Purge("steam/depot/1", true); got != want {
		t.Fatalf("dry run = %+v, want %+v", got, want)
	}
	if n, _ := e.st.Usage(); n != 7 {
		t.Fatalf("dry run removed chunks: %d left", n)
	}

	if got := ev.Purge("steam/depot/1", false); got != want {
		t.Fatalf("purge = %+v, want %+v", got, want)
	}
	for _, k := range append(a1, a2[0]) {
		if e.has(k) {
			t.Error("chunk of the purged app survived")
		}
	}
	for _, k := range append(other, untagged...) {
		if !e.has(k) {
			t.Error("chunk of another app was removed")
		}
	}
	if got := ev.Purge("steam/depot/1", false); got != (PurgeResult{}) {
		t.Errorf("second purge = %+v", got)
	}
	if got := ev.Purge("", false); got != (PurgeResult{}) {
		t.Errorf("purging the empty app = %+v", got)
	}

	// The metadata goes the way of any emptied object: two sweeps later.
	ev.Sweep()
	if dropped := ev.Sweep().Objects; dropped != 2 {
		t.Errorf("sweeps dropped %d objects, want 2", dropped)
	}
}
