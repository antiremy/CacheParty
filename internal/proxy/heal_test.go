package proxy

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cacheparty/internal/store"
	"cacheparty/internal/testutil"
)

// chunkFile is the file holding chunk idx of path, as the test cache stores
// it.
func (e *cenv) chunkFile(t *testing.T, path string, idx int64) string {
	t.Helper()
	k := store.ChunkKey(store.ObjectKey("testcdn", path), testSlice, idx)
	m, _ := filepath.Glob(filepath.Join(e.dir, "*", "*", fmt.Sprintf("%x", k[:])))
	if len(m) != 1 {
		t.Fatalf("chunk %d of %s: found %v", idx, path, m)
	}
	return m[0]
}

// primed returns an env holding all 3 chunks of /g.bin.
func primed(t *testing.T) (*cenv, []byte) {
	t.Helper()
	e := newCachedEnv(t)
	body := testutil.Pattern(3000)
	e.origin.Set("/g.bin", &testutil.Obj{Body: body})
	if _, got := e.get(t, "/g.bin", nil); !bytes.Equal(got, body) {
		t.Fatal("body mismatch on the first fetch")
	}
	eq(t, "chunks stored", e.chunks(), int64(3))
	return e, body
}

func TestMissingChunkFileIsFetchedAgain(t *testing.T) {
	e, body := primed(t)
	os.Remove(e.chunkFile(t, "/g.bin", 1))

	resp, got := e.get(t, "/g.bin", nil)
	eq(t, "status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
	eq(t, "origin ranges", strings.Join(e.origin.Ranges("/g.bin"), " "),
		"bytes=0-999 bytes=1000-1999 bytes=1000-1999 bytes=2000-2999")
	eq(t, "chunks stored", e.chunks(), int64(3))
	if !strings.Contains(e.errs.String(), "cached chunk file missing") {
		t.Errorf("not logged:\n%s", e.errs.String())
	}
}

// A chunk whose file shrank after it was verified is only found out while
// it is being sent, after the response headers. The client must still get
// the whole body.
func TestTruncatedChunkFileIsRepairedMidResponse(t *testing.T) {
	for name, rng := range map[string]string{
		"whole object":       "",
		"range in the chunk": "bytes=1500-2500",
	} {
		t.Run(name, func(t *testing.T) {
			e, body := primed(t)
			f := e.chunkFile(t, "/g.bin", 1)
			if err := os.Truncate(f, 400); err != nil {
				t.Fatal(err)
			}

			var hdr map[string]string
			want := body
			if rng != "" {
				hdr, want = map[string]string{"Range": rng}, body[1500:2501]
			}
			_, got := e.get(t, "/g.bin", hdr)
			if !bytes.Equal(got, want) {
				t.Fatalf("got %d bytes, want %d (or wrong bytes)", len(got), len(want))
			}
			eq(t, "refetches of chunk 1", e.origin.Count("/g.bin")-3, 1)
			if st, err := os.Stat(f); err != nil || st.Size() != testSlice {
				t.Errorf("chunk file not replaced: %v, %v", st, err)
			}
			if !strings.Contains(e.errs.String(), "cached chunk unreadable") {
				t.Errorf("not logged:\n%s", e.errs.String())
			}
			if lines := strings.Split(strings.TrimSpace(e.log.String()), "\n"); !strings.Contains(lines[len(lines)-1], `"MISS"`) {
				t.Errorf("access log should say MISS:\n%s", e.log.String())
			}

			// Served from the repaired file from now on.
			_, got = e.get(t, "/g.bin", hdr)
			if !bytes.Equal(got, want) {
				t.Fatal("body mismatch after repair")
			}
			eq(t, "origin requests", e.origin.Count("/g.bin"), 4)
		})
	}
}

func TestUnopenableChunkFileIsFetchedAgain(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open any file")
	}
	e, body := primed(t)
	f := e.chunkFile(t, "/g.bin", 0)
	if err := os.Chmod(f, 0); err != nil {
		t.Fatal(err)
	}

	resp, got := e.get(t, "/g.bin", nil)
	eq(t, "status", resp.StatusCode, 200)
	if !bytes.Equal(got, body) {
		t.Fatal("body mismatch")
	}
	eq(t, "refetches", e.origin.Count("/g.bin")-3, 1)
	if !strings.Contains(e.errs.String(), "cached chunk unreadable") {
		t.Errorf("not logged:\n%s", e.errs.String())
	}
	if st, err := os.Stat(f); err != nil || st.Mode().Perm() == 0 {
		t.Errorf("chunk file not replaced: %v, %v", st, err)
	}
}

func TestUnreadable(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "chunk")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.Write(make([]byte, 10))
	ch := store.Chunk{File: f, Size: 10}
	eq(t, "inside the file", unreadable(ch, 9), false)
	eq(t, "past its end", unreadable(ch, 10), true)
}

func TestObjectsRecordTheirApp(t *testing.T) {
	e := newCachedEnv(t)
	e.origin.Set("/depot/7/chunk/a", &testutil.Obj{Body: testutil.Pattern(1500)})
	e.origin.Set("/depot/7/chunk/b", &testutil.Obj{Body: testutil.Pattern(500)})
	app := func(path string) string {
		m, ok := e.st.Object(store.ObjectKey("testcdn", path))
		if !ok {
			t.Fatalf("%s not cached", path)
		}
		return m.App
	}

	// Cached by a version that recorded no app...
	e.get(t, "/depot/7/chunk/a", nil)
	eq(t, "app before", app("/depot/7/chunk/a"), "")

	e.h.cfg.App = func(service string, r *http.Request) string { return service + r.URL.Path[:8] }
	// ...gets one on its next hit, and new objects get one when filled.
	resp, _ := e.get(t, "/depot/7/chunk/a", nil)
	eq(t, "hit", resp.Header.Get("X-Upstream-Cache-Status"), "HIT")
	eq(t, "backfilled app", app("/depot/7/chunk/a"), "testcdn/depot/7")
	e.get(t, "/depot/7/chunk/b", nil)
	eq(t, "new object's app", app("/depot/7/chunk/b"), "testcdn/depot/7")
}
