package accesslog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testEntry() *Entry {
	return &Entry{
		CacheID:     "steam",
		RemoteAddr:  "192.168.1.5",
		Time:        time.Date(2024, 3, 9, 14, 5, 7, 0, time.FixedZone("", -5*3600)),
		Method:      "GET",
		URI:         "/depot/1/chunk/abc?x=1",
		Proto:       "HTTP/1.1",
		Status:      206,
		BodyBytes:   1048576,
		UserAgent:   "Valve/Steam HTTP Client 1.0",
		CacheStatus: "HIT",
		Host:        "cache1.steamcontent.com",
		Range:       "bytes=0-1048575",
	}
}

func TestCachelogExact(t *testing.T) {
	want := `[steam] 192.168.1.5 / - - - [09/Mar/2024:14:05:07 -0500] "GET /depot/1/chunk/abc?x=1 HTTP/1.1" 206 1048576 "-" "Valve/Steam HTTP Client 1.0" "HIT" "cache1.steamcontent.com" "bytes=0-1048575"` + "\n"
	if got := string(AppendCachelog(nil, testEntry())); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestCachelogForwardedForAndEscaping(t *testing.T) {
	e := testEntry()
	e.XForwardedFor = "10.0.0.1, 10.0.0.2"
	e.Referer = `http://x/"q"\`
	e.UserAgent = "a\tb\xffé"
	e.CacheStatus = ""
	e.Range = ""
	got := string(AppendCachelog(nil, e))
	for _, frag := range []string{
		` / 10.0.0.1, 10.0.0.2 - - [`,
		`"http://x/\x22q\x22\x5C"`,
		`"a\x09b\xFF\xC3\xA9"`,
		`"-" "cache1.steamcontent.com" "-"` + "\n",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing %q in\n%s", frag, got)
		}
	}
}

func TestJSON(t *testing.T) {
	e := testEntry()
	e.UserAgent = "quote\"\nnewline"
	line := AppendJSON(nil, e)
	var m map[string]string
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("invalid json %q: %v", line, err)
	}
	if m["cache_identifier"] != "steam" || m["status"] != "206" || m["bytes_sent"] != "1048576" ||
		m["user_agent"] != "quote\"\nnewline" || m["referer"] != "-" || m["timestamp"] != "2024-03-09T14:05:07-05:00" {
		t.Fatalf("unexpected fields: %v", m)
	}
}

func TestAppendDoesNotAllocate(t *testing.T) {
	e := testEntry()
	buf := make([]byte, 0, 1024)
	if n := testing.AllocsPerRun(100, func() { buf = AppendCachelog(buf[:0], e) }); n != 0 {
		t.Errorf("allocs = %v", n)
	}
}

func BenchmarkAppendCachelog(b *testing.B) {
	e := testEntry()
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf = AppendCachelog(buf[:0], e)
	}
}

func TestWriterFlushesAndCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "access.log")
	w, err := Open(path, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	l := NewLogger(w, "cachelog")
	l.Log(testEntry())
	deadline := time.Now().Add(2 * time.Second)
	for {
		if b, _ := os.ReadFile(path); len(b) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background flush never happened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	l.Log(testEntry())
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); strings.Count(string(b), "\n") != 2 {
		t.Errorf("close should flush remaining lines, got %q", b)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Error("write after close should fail")
	}
	if err := w.Close(); err != nil {
		t.Errorf("double close: %v", err)
	}
}

func TestReopenFollowsRotatedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	w, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Write([]byte("before\n"))
	if err := os.Rename(path, path+".1"); err != nil { // what logrotate does
		t.Fatal(err)
	}
	w.Write([]byte("still old file\n"))
	if err := w.Reopen(); err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("after\n"))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(path + ".1")
	cur, _ := os.ReadFile(path)
	if string(old) != "before\nstill old file\n" || string(cur) != "after\n" {
		t.Fatalf("old = %q, new = %q", old, cur)
	}
}

func TestReopenFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "a.log")
	w, err := Open(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w.Write([]byte("one\n"))
	os.RemoveAll(filepath.Join(dir, "sub")) // the directory vanished
	if err := w.Reopen(); err == nil {
		t.Fatal("want an error")
	}
	if _, err := w.Write([]byte("two\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAppendStream(t *testing.T) {
	e := &StreamEntry{
		RemoteAddr: "10.0.0.5", Time: time.Date(2026, 10, 7, 15, 4, 5, 0, time.UTC),
		Protocol: "TCP", Status: 200, BytesSent: 1234, BytesReceived: 56,
		SessionTime: 1500 * time.Millisecond, ServerName: "cdn.example.com",
	}
	want := `10.0.0.5 [07/Oct/2026:15:04:05 +0000] TCP 200 1234 56 1.500 "cdn.example.com"` + "\n"
	if got := string(AppendStream(nil, e)); got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	e.ServerName, e.Status = "", 400
	if got := string(AppendStream(nil, e)); !strings.Contains(got, ` "-"`) {
		t.Fatalf("empty SNI should log -: %q", got)
	}
}
