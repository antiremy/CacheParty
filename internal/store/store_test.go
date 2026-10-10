package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"
	"unsafe"
)

func open(t *testing.T, dir string) *FileStore {
	t.Helper()
	s, err := Open(dir, Options{FlushInterval: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func readAll(t *testing.T, s ChunkStore, k Key) []byte {
	t.Helper()
	c, err := s.Get(k)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer c.Close()
	b, err := io.ReadAll(io.NewSectionReader(c.File, c.Offset, c.Size))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestCommitGetRoundTrip(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	k := ChunkKey(ObjectKey("steam", "/depot/1/chunk/abc"), 1<<20, 3)
	if _, err := s.Get(k); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get before commit: %v", err)
	}
	data := bytes.Repeat([]byte("0123456789"), 100000) // > copy buffer
	if err := s.Commit(k, int64(len(data)), bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, k); !bytes.Equal(got, data) {
		t.Error("data mismatch")
	}
	info, err := s.Stat(k)
	if err != nil || info.Size != int64(len(data)) || info.LastAccess == 0 {
		t.Errorf("Stat = %+v, %v", info, err)
	}
	if n, b := s.Usage(); n != 1 || b != int64(len(data)) {
		t.Errorf("Usage = %d, %d", n, b)
	}
}

func TestChunkFileLayout(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)
	if err := s.Commit(k, 3, strings.NewReader("abc")); err != nil {
		t.Fatal(err)
	}
	p := s.path(k)
	h := fmt.Sprintf("%x", k[:])
	want := filepath.Join(dir, h[30:], h[28:30], h)
	if p != want {
		t.Errorf("path = %s, want %s", p, want)
	}
	if b, err := os.ReadFile(p); err != nil || string(b) != "abc" {
		t.Errorf("file = %q, %v", b, err)
	}
}

func TestCommitIsAtomicAndRejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)

	for name, r := range map[string]io.Reader{
		"short":         strings.NewReader("abc"),
		"long":          strings.NewReader("abcdefghij"),
		"read error":    io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(errors.New("boom"))),
		"timeout error": iotest.TimeoutReader(strings.NewReader("abcdef")),
	} {
		if err := s.Commit(k, 6, r); err == nil {
			t.Errorf("%s: Commit succeeded", name)
		}
		if _, err := s.Get(k); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: partial chunk visible: %v", name, err)
		}
	}
	if left, _ := os.ReadDir(filepath.Join(dir, tmpDir)); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
	if n, b := s.Usage(); n != 0 || b != 0 {
		t.Errorf("Usage after failures = %d, %d", n, b)
	}
}

func TestCommitReplacesExisting(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)
	s.Commit(k, 3, strings.NewReader("old"))
	old, _ := s.Get(k) // an open reader keeps its data across the replace
	defer old.Close()
	if err := s.Commit(k, 5, strings.NewReader("newer")); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, s, k); string(got) != "newer" {
		t.Errorf("got %q", got)
	}
	if b, _ := io.ReadAll(old.File); string(b) != "old" {
		t.Errorf("open handle saw %q", b)
	}
	if n, b := s.Usage(); n != 1 || b != 5 {
		t.Errorf("Usage = %d, %d", n, b)
	}
}

func TestDeleteAndSelfHealing(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer func() { s.Close() }() // s is reopened below
	k1 := ChunkKey(ObjectKey("a", "/1"), 10, 0)
	k2 := ChunkKey(ObjectKey("a", "/2"), 10, 0)
	k3 := ChunkKey(ObjectKey("a", "/3"), 10, 0)
	for _, k := range []Key{k1, k2, k3} {
		if err := s.Commit(k, 4, strings.NewReader("data")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete(k1); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(k1); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	if _, err := os.Stat(s.path(k1)); !os.IsNotExist(err) {
		t.Error("file not removed")
	}
	if _, err := s.Get(k1); !errors.Is(err, ErrNotFound) {
		t.Error("deleted chunk still found")
	}

	// File vanished behind our back: Get reports a miss and forgets the entry.
	os.Remove(s.path(k2))
	if _, err := s.Get(k2); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing file: %v", err)
	}
	if _, err := s.Stat(k2); !errors.Is(err, ErrNotFound) {
		t.Error("entry for missing file not forgotten")
	}

	// File truncated by a crash before writeback: on the next start it is
	// never served, and removed.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(s.path(k3), []byte("da"), 0o644)
	s = open(t, dir)
	if _, err := s.Get(k3); !errors.Is(err, ErrNotFound) {
		t.Errorf("short file: %v", err)
	}
	if _, err := os.Stat(s.path(k3)); !os.IsNotExist(err) {
		t.Error("short file not removed")
	}
	if n, b := s.Usage(); n != 0 || b != 0 {
		t.Errorf("Usage = %d, %d", n, b)
	}
}

// A file this process wrote is trusted; one loaded from the index is
// checked by the first Get and trusted after that.
func TestGetVerifiesSizeOncePerEntry(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	k := ChunkKey(ObjectKey("a", "/1"), 10, 0)
	if err := s.Commit(k, 4, strings.NewReader("data")); err != nil {
		t.Fatal(err)
	}
	if e, _ := s.lookup(k); !e.verified {
		t.Error("committed entry not verified")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir)
	defer s.Close()
	if e, _ := s.lookup(k); e.verified {
		t.Error("loaded entry verified before any Get")
	}
	c, err := s.Get(k)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if e, _ := s.lookup(k); !e.verified {
		t.Error("entry not verified after Get")
	}
}

func TestIterateAllowsDelete(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	want := map[Key]bool{}
	for i := 0; i < 300; i++ { // spreads over many shards
		k := ChunkKey(ObjectKey("a", fmt.Sprint("/", i)), 10, 0)
		want[k] = true
		s.Commit(k, 2, strings.NewReader("xy"))
	}
	seen := 0
	s.Iterate(func(k Key, i Info) bool {
		if !want[k] || i.Size != 2 {
			t.Errorf("unexpected %x %+v", k, i)
		}
		seen++
		s.Delete(k)
		return true
	})
	if seen != 300 {
		t.Errorf("saw %d", seen)
	}
	if n, _ := s.Usage(); n != 0 {
		t.Errorf("%d chunks left", n)
	}
	// early stop
	for i := 0; i < 10; i++ {
		s.Commit(ChunkKey(ObjectKey("a", fmt.Sprint("/", i)), 10, 0), 2, strings.NewReader("xy"))
	}
	calls := 0
	s.Iterate(func(Key, Info) bool { calls++; return false })
	if calls != 1 {
		t.Errorf("Iterate ignored stop: %d calls", calls)
	}
}

func TestPersistenceAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	obj := ObjectKey("steam", "/x")
	k := ChunkKey(obj, 1<<20, 0)
	gone := ChunkKey(obj, 1<<20, 1)
	s.Commit(k, 5, strings.NewReader("hello"))
	s.Commit(gone, 5, strings.NewReader("bye!!"))
	s.PutObject(obj, NewObjectMeta(1234, "application/x-test", "Mon, 02 Jan 2006 15:04:05 GMT"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s.Delete(gone)
	gobj := ObjectKey("steam", "/gone")
	s.PutObject(gobj, NewObjectMeta(1, "", ""))
	s.Flush()
	s.DeleteObject(gobj)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir)
	defer s.Close()
	if got := readAll(t, s, k); string(got) != "hello" {
		t.Errorf("got %q", got)
	}
	if _, err := s.Get(gone); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted chunk resurrected: %v", err)
	}
	m, ok := s.Object(obj)
	if !ok || m.Size != 1234 || m.ContentType != "application/x-test" || m.LastModified != "Mon, 02 Jan 2006 15:04:05 GMT" {
		t.Errorf("object = %+v, %v", m, ok)
	}
	if got := m.ContentTypeHeader(); len(got) != 1 || got[0] != "application/x-test" {
		t.Errorf("ContentTypeHeader = %v", got)
	}
	if _, ok := s.Object(gobj); ok {
		t.Error("deleted object resurrected")
	}
	if n, b := s.Usage(); n != 1 || b != 5 {
		t.Errorf("Usage = %d, %d", n, b)
	}
}

// Many chunks of different sizes in one flush: every index record must carry
// its own size (bbolt holds on to the value slices until commit).
func TestPersistenceOfManyDistinctChunks(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	want := map[Key][]byte{}
	for i := 1; i <= 50; i++ {
		k := ChunkKey(ObjectKey("a", fmt.Sprint("/", i)), 10, 0)
		data := bytes.Repeat([]byte{byte(i)}, i*7)
		want[k] = data
		if err := s.Commit(k, int64(len(data)), bytes.NewReader(data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, dir)
	defer s.Close()
	var total int64
	for k, data := range want {
		info, err := s.Stat(k)
		if err != nil || info.Size != int64(len(data)) {
			t.Fatalf("after reopen Stat = %+v, %v; want size %d", info, err, len(data))
		}
		if got := readAll(t, s, k); !bytes.Equal(got, data) {
			t.Fatal("data mismatch after reopen")
		}
		total += int64(len(data))
	}
	if n, b := s.Usage(); n != 50 || b != total {
		t.Errorf("Usage = %d, %d; want 50, %d", n, b, total)
	}
}

func TestEmptyMetaHeadersAreNil(t *testing.T) {
	m := NewObjectMeta(1, "", "")
	if m.ContentTypeHeader() != nil || m.LastModifiedHeader() != nil {
		t.Error("expected nil header values")
	}
}

// Startup must trust the index and not walk the tree: a file that is on disk
// but not in the index is simply not cached.
func TestStartupDoesNotScanTree(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)
	s.Commit(k, 3, strings.NewReader("abc"))
	s.Close() // flushes
	// drop an unindexed file into the tree
	stray := ChunkKey(ObjectKey("a", "/stray"), 10, 0)
	s2 := open(t, dir)
	os.MkdirAll(filepath.Dir(s2.path(stray)), 0o755)
	os.WriteFile(s2.path(stray), []byte("zzz"), 0o644)
	s2.Close()
	s3 := open(t, dir)
	defer s3.Close()
	if _, err := s3.Get(stray); !errors.Is(err, ErrNotFound) {
		t.Errorf("unindexed file served: %v", err)
	}
	if n, _ := s3.Usage(); n != 1 {
		t.Errorf("chunks = %d", n)
	}
}

func TestOpenClearsInterruptedTempFiles(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, tmpDir), 0o755)
	os.WriteFile(filepath.Join(dir, tmpDir, "leftover"), []byte("x"), 0o644)
	s := open(t, dir)
	defer s.Close()
	if left, _ := os.ReadDir(filepath.Join(dir, tmpDir)); len(left) != 0 {
		t.Errorf("leftover temp files: %v", left)
	}
}

// Hits must not touch bbolt; the access time is flushed in a batch.
// The index maps must stay pointer-free, or every GC cycle scans every
// cached chunk.
func TestIndexHoldsNoPointers(t *testing.T) {
	var noPointers func(reflect.Type) bool
	noPointers = func(typ reflect.Type) bool {
		switch typ.Kind() {
		case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
			return true
		case reflect.Array:
			return noPointers(typ.Elem())
		case reflect.Struct:
			for i := 0; i < typ.NumField(); i++ {
				if !noPointers(typ.Field(i).Type) {
					return false
				}
			}
			return true
		}
		return false
	}
	m := reflect.TypeOf(shard{}.m)
	if !noPointers(m.Key()) || !noPointers(m.Elem()) {
		t.Errorf("%v holds pointers", m)
	}
	if size := reflect.TypeOf(entry{}).Size(); size > 32 {
		t.Errorf("entry is %d bytes, want <= 32", size)
	}
}

func TestLastAccessIsBatched(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)
	s.Commit(k, 3, strings.NewReader("abc"))
	s.Flush()
	before := dbWrites(s)
	time.Sleep(2 * time.Millisecond)
	t0 := time.Now().UnixNano()
	for i := 0; i < 1000; i++ {
		c, err := s.Get(k)
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if after := dbWrites(s); after != before {
		t.Errorf("hits touched bbolt: tx stats %d -> %d", before, after)
	}
	s.pendMu.Lock()
	queued := len(s.dirty)
	s.pendMu.Unlock()
	if queued != 1 {
		t.Errorf("%d queued index updates for 1000 hits of one chunk, want 1", queued)
	}
	s.Close()

	s = open(t, dir)
	defer s.Close()
	info, _ := s.Stat(k)
	if info.LastAccess < t0 {
		t.Errorf("last access not persisted: %d < %d", info.LastAccess, t0)
	}
}

func TestConcurrentCommitGetDelete(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				k := ChunkKey(ObjectKey("a", fmt.Sprint("/", i%20)), 10, 0)
				data := fmt.Sprintf("value-%04d", i%20) // same bytes for a key from every writer
				switch (i + g) % 3 {
				case 0:
					if err := s.Commit(k, int64(len(data)), strings.NewReader(data)); err != nil {
						t.Error(err)
					}
				case 1:
					if c, err := s.Get(k); err == nil {
						b, _ := io.ReadAll(c.File)
						c.Close()
						if string(b) != data {
							t.Errorf("read %q, want %q", b, data)
						}
					}
				default:
					s.Delete(k)
				}
			}
		}(g)
	}
	wg.Wait()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// counters agree with the index
	var n, b int64
	s.Iterate(func(_ Key, i Info) bool { n++; b += i.Size; return true })
	if gn, gb := s.Usage(); gn != n || gb != b {
		t.Errorf("Usage = %d,%d; Iterate = %d,%d", gn, gb, n, b)
	}
}

func TestKeys(t *testing.T) {
	if ObjectKey("a", "/b") == ObjectKey("a/", "b") || ObjectKey("a", "/b") == ObjectKey("a\x00/b", "") {
		t.Error("object keys collide across the identifier/path boundary")
	}
	o := ObjectKey("steam", "/x")
	if ChunkKey(o, 10, 1) == ChunkKey(o, 10, 2) || ChunkKey(o, 10, 1) == ChunkKey(o, 20, 1) {
		t.Error("chunk keys must differ by index and by slice size")
	}
	if n := testingAllocs(func() { _ = ChunkKey(ObjectKey("steam", "/depot/1/chunk/abc"), 1<<20, 7) }); n != 0 {
		t.Errorf("key derivation allocates %v", n)
	}
}

// dbWrites counts bbolt write transactions and page writes so far.
func dbWrites(s *FileStore) int64 {
	st := s.db.Stats()
	return int64(st.TxStats.GetWrite()) + int64(st.TxN)
}

func testingAllocs(f func()) float64 { return testing.AllocsPerRun(100, f) }

func BenchmarkGetClose(b *testing.B) {
	s, err := Open(b.TempDir(), Options{FlushInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	k := ChunkKey(ObjectKey("a", "/b"), 10, 0)
	s.Commit(k, 4, strings.NewReader("data"))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c, err := s.Get(k)
		if err != nil {
			b.Fatal(err)
		}
		c.Close()
	}
}

func BenchmarkCommit1MiB(b *testing.B) {
	s, err := Open(b.TempDir(), Options{FlushInterval: time.Hour})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	data := make([]byte, 1<<20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		k := ChunkKey(ObjectKey("a", fmt.Sprint("/", i%64)), 1<<20, 0)
		if err := s.Commit(k, int64(len(data)), bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFlush writes 20k updated entries into an index of 500k, the
// shape of a busy flush interval on a large cache.
func BenchmarkFlush(b *testing.B) {
	s, err := Open(b.TempDir(), Options{FlushInterval: time.Hour, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	const total, dirty = 500_000, 20_000
	keys := make([]Key, total)
	for i := range keys {
		keys[i] = ChunkKey(ObjectKey("a", "/b"), 1, int64(i))
		s.publish(keys[i], 1)
	}
	if err := s.Flush(); err != nil {
		b.Fatal(err)
	}
	pages := pagesWritten(s)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		for j := 0; j < dirty; j++ {
			s.publish(keys[(i*dirty+j*7919)%total], 1)
		}
		b.StartTimer()
		if err := s.Flush(); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(pagesWritten(s)-pages)/float64(b.N), "pages/op")
}

func pagesWritten(s *FileStore) int64 {
	st := s.db.Stats()
	return st.TxStats.GetWrite()
}

func TestObjectAppPersistsAndIsShared(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	a, b, c := ObjectKey("steam", "/a"), ObjectKey("steam", "/b"), ObjectKey("steam", "/c")
	for _, k := range []Key{a, b} {
		m := NewObjectMeta(10, "", "")
		m.App = "steam/depot/" + string([]byte("990081")) // distinct strings
		s.PutObject(k, m)
	}
	s.PutObject(c, NewObjectMeta(10, "", ""))
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s = open(t, dir)
	defer s.Close()
	ma, _ := s.Object(a)
	mb, _ := s.Object(b)
	mc, _ := s.Object(c)
	if ma.App != "steam/depot/990081" || mb.App != ma.App || mc.App != "" {
		t.Fatalf("apps = %q, %q, %q", ma.App, mb.App, mc.App)
	}
	if unsafe.StringData(ma.App) != unsafe.StringData(mb.App) {
		t.Error("loaded App values are not interned")
	}
}

// Records written before App existed, and records an older version reads.
func TestObjectRecordCompatibility(t *testing.T) {
	old := NewObjectMeta(7, "text/plain", "yesterday")
	rec := encodeObject(old)
	if len(rec) != objectRecordMinSize+len("text/plain")+len("yesterday") {
		t.Errorf("a record without App changed layout: %d bytes", len(rec))
	}
	m, ok := decodeObject(rec, nil)
	if !ok || m.Size != 7 || m.ContentType != "text/plain" || m.LastModified != "yesterday" || m.App != "" {
		t.Fatalf("old record: %+v, %v", m, ok)
	}

	withApp := NewObjectMeta(7, "text/plain", "yesterday")
	withApp.App = "epicgames/app/Fortnite"
	rec = encodeObject(withApp)
	if m, ok := decodeObject(rec, nil); !ok || m.App != withApp.App || m.LastModified != "yesterday" {
		t.Fatalf("new record: %+v, %v", m, ok)
	}
	if _, ok := decodeObject(rec[:len(rec)-1], nil); ok {
		t.Error("truncated App accepted")
	}
}
