package dashboard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cacheparty/internal/config"
)

const testSteamMap = `{
  "metadata": {"lastUpdated": "2026-11-18T13:00:00Z"},
  "depotMappings": {
    "214491": {"ownerId": 214490, "appIds": [214490], "appNames": ["Alien: Isolation"], "appTypes": ["game"]},
    "214492": {"ownerId": 214490, "appIds": [214490], "appNames": ["Alien: Isolation"]},
    "228988": {"ownerId": 228980, "appIds": [228980, 730], "appNames": ["Steamworks Common Redistributables", "Counter-Strike"]},
    "300": {"ownerId": 1, "appIds": [2], "appNames": ["Not the owner"]},
    "301": {"ownerId": 3, "appIds": [3], "appNames": ["  "]},
    "abc": {"ownerId": 4, "appIds": [4], "appNames": ["Bad depot"]}
  }
}`

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestParseSteamMapSkipsOddRecords(t *testing.T) {
	s, err := parseSteamMap(strings.NewReader(`{"depotMappings": {
		"1": {"ownerId": 1.5, "appIds": [1], "appNames": ["Float owner"]},
		"2": {"ownerId": 2, "appIds": ["2"], "appNames": ["String app"]},
		"3": {"ownerId": 3, "appIds": [3], "appNames": ["Fine"]}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Depots) != 1 || s.Apps[3] != "Fine" {
		t.Fatalf("depots %v, apps %v", s.Depots, s.Apps)
	}
}

func TestParseSteamMap(t *testing.T) {
	s, err := parseSteamMap(strings.NewReader(testSteamMap))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Depots) != 3 || s.Depots[214491] != 214490 || s.Depots[228988] != 228980 {
		t.Fatalf("depots = %v", s.Depots)
	}
	if s.Apps[214490] != "Alien: Isolation" || s.Apps[228980] != "Steamworks Common Redistributables" {
		t.Fatalf("apps = %v", s.Apps)
	}
	if _, err := parseSteamMap(strings.NewReader(`{"depotMappings": {"1": `)); err == nil {
		t.Fatal("a truncated map was accepted")
	}
}

// steamServer serves a lancache-pics release of body; digest overrides its
// SHA-256. It counts downloads of the map.
func steamServer(t *testing.T, tag, body, digest string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	if digest == "" {
		sum := sha256.Sum256([]byte(body))
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	var downloads atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/release", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"tag_name": tag, "assets": []map[string]string{
			{"name": "other.json", "browser_download_url": srv.URL + "/nothing", "digest": "sha256:00"},
			{"name": steamAsset, "browser_download_url": srv.URL + "/map", "digest": digest},
		}})
	})
	mux.HandleFunc("/map", func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		io.WriteString(w, body)
	})
	old, oldMin := steamRelease, minSteamDepots
	steamRelease, minSteamDepots = srv.URL+"/release", 1
	t.Cleanup(func() { steamRelease, minSteamDepots = old, oldMin })
	return srv, &downloads
}

func TestNamesUpdateAndReload(t *testing.T) {
	srv, downloads := steamServer(t, "v1", testSteamMap, "")
	file := filepath.Join(t.TempDir(), "game-names.json")
	n := newNames(srv.Client(), file, quietLog())
	if err := n.update(context.Background()); err != nil {
		t.Fatal(err)
	}
	if name, game := n.lookup("steam", app{"depot", "214492"}); name != "Alien: Isolation" || game != "214490" {
		t.Fatalf("lookup = %q %q", name, game)
	}
	if name, _ := n.lookup("steam", app{"depot", "999"}); name != "" {
		t.Fatalf("unknown depot named %q", name)
	}
	if err := n.update(context.Background()); err != nil || downloads.Load() != 1 {
		t.Fatalf("same release downloaded again: %d, %v", downloads.Load(), err)
	}

	// A restart reads the saved map instead of downloading it.
	again := newNames(nil, file, quietLog())
	again.load()
	if name, game := again.lookup("steam", app{"depot", "214491"}); name != "Alien: Isolation" || game != "214490" || again.release() != "v1" {
		t.Fatalf("reloaded lookup = %q %q, release %q", name, game, again.release())
	}
}

func TestNamesUpdateRejectsBadMaps(t *testing.T) {
	for name, tc := range map[string]struct{ body, digest string }{
		"wrong digest": {testSteamMap, "sha256:" + strings.Repeat("0", 64)},
		"no digest":    {testSteamMap, "md5:abc"},
		"not JSON":     {"<html>", ""},
	} {
		t.Run(name, func(t *testing.T) {
			srv, _ := steamServer(t, "v2", tc.body, tc.digest)
			n := newNames(srv.Client(), "", quietLog())
			if err := n.update(context.Background()); err == nil {
				t.Fatal("bad map accepted")
			}
			if got, _ := n.lookup("steam", app{"depot", "214491"}); got != "" {
				t.Fatalf("named %q from a bad map", got)
			}
		})
	}
	srv, _ := steamServer(t, "v3", testSteamMap, "")
	minSteamDepots = 10
	if err := newNames(srv.Client(), "", quietLog()).update(context.Background()); err == nil {
		t.Fatal("a map with too few depots was accepted")
	}
}

func TestNamesRunFollowsSetting(t *testing.T) {
	srv, downloads := steamServer(t, "v1", testSteamMap, "")
	n := newNames(srv.Client(), "", quietLog())
	var on atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.run(ctx, on.Load); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	if time.Sleep(50 * time.Millisecond); downloads.Load() != 0 {
		t.Fatal("downloaded while off")
	}
	on.Store(true)
	n.poke()
	waitFor("not downloaded when turned on", func() bool { return n.release() == "v1" })
	on.Store(false)
	n.poke()
	waitFor("names kept when turned off", func() bool { return n.release() == "" })
}

func TestNamesTurnedOffMidDownloadStopsIt(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mux.HandleFunc("/release", func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(stopped)
	})
	old := steamRelease
	steamRelease = srv.URL + "/release"
	t.Cleanup(func() { steamRelease = old })

	n := newNames(srv.Client(), "", quietLog())
	var on atomic.Bool
	on.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { n.run(ctx, on.Load); close(done) }()
	defer func() { cancel(); <-done }()

	<-started
	on.Store(false)
	n.poke()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("download kept going after names were turned off")
	}
}

func TestServeAppsGameNames(t *testing.T) {
	tr := NewTraffic()
	cfg := &config.Config{GameNames: true, GroupSteamDepots: true}
	d := New(Config{Traffic: tr, Stats: func() Stats { return Stats{} }, Settings: cfg, Log: quietLog()})
	s, err := parseSteamMap(strings.NewReader(testSteamMap))
	if err != nil {
		t.Fatal(err)
	}
	d.names.set(s)
	t0 := time.Unix(1000, 0)
	d.sample(t0)
	tr.Flow("steam", get("/depot/214491/chunk/a")).Sent.Add(100)
	tr.Flow("steam", get("/depot/214492/chunk/a")).Sent.Add(50)
	tr.Flow("steam", get("/depot/228988/chunk/a")).Sent.Add(10)
	tr.Flow("steam", get("/depot/999/chunk/a")).Sent.Add(1)
	tr.Flow("blizzard", get("/tpr/wow/data/a")).Sent.Add(3)
	tr.Flow("blizzard", get("/tpr/Hero-Live-a/data/a")).Sent.Add(2)
	tr.Flow("blizzard", get("/tpr/pro/data/a")).Sent.Add(1)
	d.sample(t0.Add(time.Second))

	reply := func(service string) appsJSON {
		t.Helper()
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, httptest.NewRequest("GET", "/api/services/"+service, nil))
		var got appsJSON
		if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	apps := func(service string) []appJSON { return reply(service).Apps }
	if reply("steam").Pictures {
		t.Fatal("pictures on by default")
	}

	got := apps("steam")
	if len(got) != 3 {
		t.Fatalf("steam rows = %+v", got)
	}
	g := got[0]
	if g.Kind != "game" || g.ID != "214490" || g.Name != "Alien: Isolation" || g.Sent != 150 || g.SentRate != 150 ||
		len(g.Depots) != 2 || g.Depots[0].ID != "214491" || g.LastActive != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("game row = %+v", g)
	}
	if one := got[1]; one.Kind != "depot" || one.ID != "228988" || one.Name != "Steamworks Common Redistributables" || one.Game != "228980" {
		t.Fatalf("single-depot row = %+v", one)
	}
	if unknown := got[2]; unknown.Kind != "depot" || unknown.Name != "" || unknown.Game != "" {
		t.Fatalf("unknown row = %+v", unknown)
	}
	// Names follow the CDN path: "pro" is Overwatch's product code, not a path.
	if b := apps("blizzard"); len(b) != 3 || b[0].Name != "World of Warcraft" || b[1].Name != "Heroes of the Storm" || b[2].Name != "" {
		t.Fatalf("blizzard rows = %+v", b)
	}

	d.current.Store(&config.Config{GameNames: true, GamePictures: true})
	if !reply("steam").Pictures || reply("blizzard").Pictures {
		t.Fatal("pictures should be on for Steam only")
	}
	d.current.Store(&config.Config{GamePictures: true})
	if reply("steam").Pictures {
		t.Fatal("pictures on with names off")
	}

	d.current.Store(&config.Config{GameNames: true})
	if got := apps("steam"); len(got) != 4 || got[0].Name != "Alien: Isolation" || got[0].Kind != "depot" {
		t.Fatalf("ungrouped rows = %+v", got)
	}
	d.current.Store(&config.Config{GroupSteamDepots: true})
	if got := apps("steam"); len(got) != 4 || got[0].Name != "" || got[0].Game != "" {
		t.Fatalf("rows with names off = %+v", got)
	}
}

func TestSettingsGameNames(t *testing.T) {
	srv, applied := newTestDashboard(t, "pw")
	if code, body := put(t, srv, `{"values":{"GAME_NAMES":"true","GROUP_STEAM_DEPOTS":"false","GAME_PICTURES":"true"}}`, "pw", nil); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if !applied.GameNames || applied.GroupSteamDepots || !applied.GamePictures {
		t.Fatalf("applied %+v", applied)
	}
	if code, _ := put(t, srv, `{"values":{"GAME_NAMES":"maybe"}}`, "pw", nil); code != http.StatusBadRequest {
		t.Fatalf("bad switch value: %d", code)
	}
}
