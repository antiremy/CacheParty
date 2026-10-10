package dashboard

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTrafficSaveLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "stats.json")
	tr := NewTraffic()
	r := httptest.NewRequest("GET", "http://cdn.example/depot/990081/chunk/abc", nil)
	r.RemoteAddr = "10.0.0.5:4000"
	r.Host = "cdn.example"
	f := tr.Flow("steam", r)
	f.Sent.Add(300)
	f.Parent.Sent.Add(300) // the service total, as proxy.Flow does
	f.Fetched.Add(100)
	f.Parent.Fetched.Add(100)
	f.Also[0].Sent.Add(300)
	f.Also[0].Fetched.Add(100)
	f.Also[1].Fetched.Add(100)
	hs, hr := tr.HTTPS("launcher.example")
	hs.Add(50)
	hr.Add(5)

	at := time.UnixMilli(1_700_000_000_000)
	steamApp := flowKey{Service: "steam", App: app{"depot", "990081"}}
	if err := tr.save(path, func(k flowKey) time.Time {
		if k == steamApp {
			return at
		}
		return time.Time{}
	}); err != nil {
		t.Fatal(err)
	}

	tr2 := NewTraffic()
	seen, err := tr2.load(path)
	if err != nil {
		t.Fatal(err)
	}
	want, got := tr.snapshot(), tr2.snapshot()
	if len(got) != len(want) || len(got) != 5 {
		t.Fatalf("got %d entries, want %d (5): %v", len(got), len(want), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%+v = %v, want %v", k, got[k], v)
		}
	}
	if !seen[steamApp].Equal(at) || len(seen) != 1 {
		t.Errorf("seen = %v", seen)
	}

	// Restored flows keep counting, and the game still feeds its service.
	g := tr2.gameFlow("steam", r)
	g.Sent.Add(1)
	g.Parent.Sent.Add(1)
	if v := tr2.snapshot()[steamApp]; v[0] != 301 {
		t.Errorf("after more traffic: %v", v)
	}
}

func TestTrafficLoadMissingAndBad(t *testing.T) {
	dir := t.TempDir()
	tr := NewTraffic()
	if _, err := tr.load(filepath.Join(dir, "none.json")); err != nil {
		t.Fatalf("missing file: %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	for _, body := range []string{"{not json", `{"version":99}`} {
		os.WriteFile(bad, []byte(body), 0o644)
		if _, err := tr.load(bad); err == nil {
			t.Errorf("%q: want an error", body)
		}
	}
	if n := len(tr.snapshot()); n != 0 {
		t.Errorf("bad files left %d entries", n)
	}
}

func TestSampleKeepsRestoredLastActive(t *testing.T) {
	h := newHistory()
	k := flowKey{Service: "steam"}
	at := time.Unix(500, 0)
	h.seen[k] = at
	h.sample(time.Unix(1000, 0), point{}, map[flowKey][2]int64{k: {10, 10}})
	if got := h.lastActive(k); !got.Equal(at) {
		t.Fatalf("lastActive = %v, want %v", got, at)
	}
	h.sample(time.Unix(1001, 0), point{}, map[flowKey][2]int64{k: {20, 20}})
	if got := h.lastActive(k); !got.Equal(time.Unix(1001, 0)) {
		t.Fatalf("lastActive after traffic = %v", got)
	}
}
