package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"cacheparty/internal/config"
	"cacheparty/internal/evict"
	"cacheparty/internal/store"
)

func TestVerifyCache(t *testing.T) {
	var gotDry []bool
	srv, _ := newTestDashboard(t, "hunter2", func(c *Config) {
		c.Verify = func(_ context.Context, dryRun bool) (store.ScrubResult, error) {
			gotDry = append(gotDry, dryRun)
			return store.ScrubResult{Files: 10, Orphans: 1, Missing: 2, Corrupt: 3, Freed: 400}, nil
		}
	})

	if code, _ := send(t, srv, "POST", "/api/cache/verify", `{}`, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no password: %d", code)
	}
	if code, _ := send(t, srv, "POST", "/api/cache/verify", `{}`, "hunter2", map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	if len(gotDry) != 0 {
		t.Fatal("verified without authorization")
	}

	code, body := send(t, srv, "POST", "/api/cache/verify", `{"dryRun":true}`, "hunter2", nil)
	if code != http.StatusOK {
		t.Fatalf("dry run: %d %s", code, body)
	}
	var got verifyJSON
	json.Unmarshal([]byte(body), &got)
	if want := (verifyJSON{true, 10, 1, 2, 3, 400}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if code, _ := send(t, srv, "POST", "/api/cache/verify", ``, "hunter2", nil); code != http.StatusOK {
		t.Fatalf("empty body: %d", code)
	}
	if len(gotDry) != 2 || !gotDry[0] || gotDry[1] {
		t.Fatalf("dry runs passed on: %v", gotDry)
	}
}

func TestVerifyCacheWhileScrubbing(t *testing.T) {
	srv, _ := newTestDashboard(t, "hunter2", func(c *Config) {
		c.Verify = func(context.Context, bool) (store.ScrubResult, error) {
			return store.ScrubResult{}, store.ErrScrubRunning
		}
	})
	if code, _ := send(t, srv, "POST", "/api/cache/verify", `{}`, "hunter2", nil); code != http.StatusConflict {
		t.Fatalf("code = %d", code)
	}
}

func TestVerifyCacheReadOnly(t *testing.T) {
	srv, _ := newTestDashboard(t, "")
	if code, _ := send(t, srv, "POST", "/api/cache/verify", `{}`, "x", nil); code != http.StatusForbidden {
		t.Fatalf("code = %d", code)
	}
}

func TestPurgeApp(t *testing.T) {
	type call struct {
		apps   string
		dryRun bool
	}
	var calls []call
	srv, _ := newTestDashboard(t, "hunter2", func(c *Config) {
		c.Purge = func(match func(string) bool, dryRun bool) evict.PurgeResult {
			calls = append(calls, call{strings.Join(matching(match), ","), dryRun})
			return evict.PurgeResult{Objects: 2, Chunks: 5, Bytes: 5 << 20}
		}
	})
	const depot = `"service":"steam","kind":"depot","id":"990081"`

	if code, _ := send(t, srv, "POST", "/api/cache/purge", `{`+depot+`,"confirm":true}`, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no password: %d", code)
	}
	for _, bad := range []string{`{}`, `{"service":"steam","kind":"depot"}`, `{"service":"steam","kind":"other","id":"other"}`, `{"service":"a/b","kind":"host","id":"x"}`} {
		if code, _ := send(t, srv, "POST", "/api/cache/purge", bad, "hunter2", nil); code != http.StatusBadRequest {
			t.Errorf("%s: %d", bad, code)
		}
	}
	if len(calls) != 0 {
		t.Fatalf("purged on a refused request: %v", calls)
	}

	code, body := send(t, srv, "POST", "/api/cache/purge", `{`+depot+`}`, "hunter2", nil)
	var got purgeJSON
	json.Unmarshal([]byte(body), &got)
	if want := (purgeJSON{"steam/depot/990081", false, 2, 5, 5 << 20}); code != http.StatusOK || got != want {
		t.Fatalf("preview: %d %+v, want %+v", code, got, want)
	}
	code, body = send(t, srv, "POST", "/api/cache/purge", `{`+depot+`,"confirm":true}`, "hunter2", nil)
	json.Unmarshal([]byte(body), &got)
	if code != http.StatusOK || !got.Confirmed {
		t.Fatalf("purge: %d %s", code, body)
	}
	if want := []call{{"steam/depot/990081", true}, {"steam/depot/990081", false}}; !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// cachedTags stands in for the tags of objects in the cache index.
var cachedTags = []string{
	"steam/depot/214491", "steam/depot/214492", "steam/depot/228988", "steam/depot/999",
	"steam/depot/990081", "steam/game/214490", "epicgames/depot/214491",
}

func matching(match func(string) bool) []string {
	var got []string
	for _, tag := range cachedTags {
		if match(tag) {
			got = append(got, tag)
		}
	}
	return got
}

func TestPurgeGame(t *testing.T) {
	var calls [][]string
	d := New(Config{
		Traffic: NewTraffic(), Stats: func() Stats { return Stats{} }, Settings: &config.Config{}, Log: quietLog(),
		Password: "hunter2",
		Purge: func(match func(string) bool, dryRun bool) evict.PurgeResult {
			got := matching(match)
			calls = append(calls, got)
			return evict.PurgeResult{Objects: len(got), Chunks: 2 * len(got), Bytes: 3}
		},
	})
	purge := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/cache/purge", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer hunter2")
		rec := httptest.NewRecorder()
		d.ServeHTTP(rec, r)
		return rec
	}
	const game = `{"service":"steam","kind":"game","id":"214490","confirm":true}`

	if rec := purge(game); rec.Code != http.StatusConflict || len(calls) != 0 {
		t.Fatalf("without names: %d %s, calls %v", rec.Code, rec.Body, calls)
	}

	s, err := parseSteamMap(strings.NewReader(testSteamMap))
	if err != nil {
		t.Fatal(err)
	}
	d.names.set(s)
	rec := purge(game)
	var got purgeJSON
	json.Unmarshal(rec.Body.Bytes(), &got)
	if want := (purgeJSON{"steam/game/214490", true, 2, 4, 3}); rec.Code != http.StatusOK || got != want {
		t.Fatalf("%d %+v, want %+v", rec.Code, got, want)
	}
	// The traffic totals saw none of these depots; the cache index decides.
	if want := []string{"steam/depot/214491", "steam/depot/214492"}; len(calls) != 1 || !slices.Equal(calls[0], want) {
		t.Fatalf("calls = %v, want [%v]", calls, want)
	}
}

func TestAppTag(t *testing.T) {
	for _, c := range []struct{ service, host, path, want string }{
		{"steam", "cache1-fra1.steamcontent.com", "/depot/990081/chunk/8af5e1", "steam/depot/990081"},
		{"epicgames", "download.epicgames.com", "/Builds/Org/o-x/Fortnite/default/a.chunk", "epicgames/app/Fortnite"},
		{"riot", "LoL.dyn.riotcdn.net:80", "/channels/public/x", "riot/host/lol.dyn.riotcdn.net"},
	} {
		r := httptest.NewRequest("GET", c.path, nil)
		r.Host = c.host
		if got := AppTag(c.service, r); got != c.want {
			t.Errorf("%s %s%s: %q, want %q", c.service, c.host, c.path, got, c.want)
		}
	}
}
