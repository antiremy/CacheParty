package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestScrubRemovesOrphansAndForgetsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	defer s.Close()

	keep := ChunkKey(ObjectKey("id", "/keep"), 100, 0)
	gone := ChunkKey(ObjectKey("id", "/gone"), 100, 0)
	for _, k := range []Key{keep, gone} {
		if err := s.Commit(k, 10, bytes.NewReader(make([]byte, 10))); err != nil {
			t.Fatal(err)
		}
	}
	// An orphan: a correctly named, correctly placed file the index lacks.
	orphanKey := ChunkKey(ObjectKey("id", "/orphan"), 100, 0)
	orphan := s.path(orphanKey)
	os.MkdirAll(filepath.Dir(orphan), 0o755)
	os.WriteFile(orphan, make([]byte, 25), 0o644)
	// A stranger in the tree must be left alone.
	stranger := filepath.Join(filepath.Dir(orphan), "README")
	os.WriteFile(stranger, []byte("x"), 0o644)
	// And an indexed chunk whose file was deleted behind our back.
	os.Remove(s.path(gone))

	// Make everything look old, so the age guard does not hide it.
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(orphan, old, old)
	time.Sleep(5 * time.Millisecond)

	res, err := s.Scrub(context.Background(), ScrubOptions{MinAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Orphans != 1 || res.Freed != 25 || res.Missing != 1 {
		t.Fatalf("res = %+v", res)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("orphan file still on disk")
	}
	if _, err := os.Stat(stranger); err != nil {
		t.Error("unrelated file was removed")
	}
	if _, err := s.Stat(gone); err == nil {
		t.Error("chunk with no file is still indexed")
	}
	if _, err := s.Stat(keep); err != nil {
		t.Error("healthy chunk was forgotten")
	}
	if n, b := s.Usage(); n != 1 || b != 10 {
		t.Errorf("usage = %d, %d", n, b)
	}
}

func TestScrubLeavesYoungFiles(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	k := ChunkKey(ObjectKey("id", "/young"), 100, 0)
	p := s.path(k)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, make([]byte, 5), 0o644) // just renamed, not yet indexed
	res, err := s.Scrub(context.Background(), ScrubOptions{MinAge: time.Hour})
	if err != nil || res.Orphans != 0 {
		t.Fatalf("res = %+v, err = %v", res, err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Error("young file removed")
	}
}

func TestScrubStopsWhenContextEnds(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	k := ChunkKey(ObjectKey("id", "/a"), 100, 0)
	s.Commit(k, 1, bytes.NewReader([]byte{1}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Scrub(ctx, ScrubOptions{MinAge: time.Hour, Pause: time.Second}); err == nil {
		t.Fatal("want an error from a cancelled context")
	}
	if _, err := s.Stat(k); err != nil {
		t.Error("an interrupted scrub must not forget anything")
	}
}

func TestIterateObjects(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	a, b := ObjectKey("id", "/a"), ObjectKey("id", "/b")
	s.PutObject(a, NewObjectMeta(1, "", ""))
	s.PutObject(b, NewObjectMeta(2, "", ""))
	seen := map[Key]int64{}
	s.IterateObjects(func(k Key, m *ObjectMeta) bool {
		seen[k] = m.Size
		s.DeleteObject(k) // allowed during iteration
		return true
	})
	if len(seen) != 2 || seen[a] != 1 || seen[b] != 2 {
		t.Fatalf("seen = %v", seen)
	}
	if _, ok := s.Object(a); ok {
		t.Error("object not deleted")
	}
}

// brokenStore holds one chunk of each kind Scrub repairs: an orphan file, an
// indexed chunk with no file, and one whose file is too short.
func brokenStore(t *testing.T) (s *FileStore, orphan string, gone, short, keep Key) {
	t.Helper()
	s = open(t, t.TempDir())
	t.Cleanup(func() { s.Close() })
	gone = ChunkKey(ObjectKey("id", "/gone"), 100, 0)
	short = ChunkKey(ObjectKey("id", "/short"), 100, 0)
	keep = ChunkKey(ObjectKey("id", "/keep"), 100, 0)
	for _, k := range []Key{gone, short, keep} {
		if err := s.Commit(k, 10, bytes.NewReader(make([]byte, 10))); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(s.path(gone))
	os.Truncate(s.path(short), 4)
	orphan = s.path(ChunkKey(ObjectKey("id", "/orphan"), 100, 0))
	os.MkdirAll(filepath.Dir(orphan), 0o755)
	os.WriteFile(orphan, make([]byte, 25), 0o644)
	old := time.Now().Add(-3 * time.Hour)
	os.Chtimes(orphan, old, old)
	time.Sleep(5 * time.Millisecond)
	return s, orphan, gone, short, keep
}

func TestScrubRemovesFilesOfTheWrongSize(t *testing.T) {
	s, _, _, short, keep := brokenStore(t)
	res, err := s.Scrub(context.Background(), ScrubOptions{MinAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	want := ScrubResult{Files: 3, Orphans: 1, Missing: 1, Corrupt: 1, Freed: 25 + 4}
	if res != want {
		t.Fatalf("res = %+v, want %+v", res, want)
	}
	if _, err := s.Stat(short); err == nil {
		t.Error("short chunk still indexed")
	}
	if _, err := os.Stat(s.path(short)); !os.IsNotExist(err) {
		t.Error("short chunk file still on disk")
	}
	if _, err := s.Stat(keep); err != nil {
		t.Error("healthy chunk was removed")
	}
	if n, b := s.Usage(); n != 1 || b != 10 {
		t.Errorf("usage = %d, %d", n, b)
	}
}

func TestScrubDryRunChangesNothing(t *testing.T) {
	s, orphan, gone, short, _ := brokenStore(t)
	res, err := s.Scrub(context.Background(), ScrubOptions{MinAge: time.Hour, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := (ScrubResult{Files: 3, Orphans: 1, Missing: 1, Corrupt: 1, Freed: 29}); res != want {
		t.Fatalf("res = %+v, want %+v", res, want)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Error("dry run removed the orphan")
	}
	for _, k := range []Key{gone, short} {
		if _, err := s.Stat(k); err != nil {
			t.Error("dry run forgot a chunk")
		}
	}
	if _, err := os.Stat(s.path(short)); err != nil {
		t.Error("dry run removed a file")
	}
	// And a real run afterwards still finds everything.
	if res, _ := s.Scrub(context.Background(), ScrubOptions{MinAge: time.Hour}); res.Orphans+res.Missing+res.Corrupt != 3 {
		t.Errorf("after dry run: %+v", res)
	}
}

func TestOneScrubAtATime(t *testing.T) {
	s := open(t, t.TempDir())
	defer s.Close()
	s.scrubMu.Lock()
	_, err := s.Scrub(context.Background(), ScrubOptions{})
	s.scrubMu.Unlock()
	if !errors.Is(err, ErrScrubRunning) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.Scrub(context.Background(), ScrubOptions{}); err != nil {
		t.Fatal(err)
	}
}
