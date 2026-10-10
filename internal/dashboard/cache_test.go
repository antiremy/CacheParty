package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

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
		app    string
		dryRun bool
	}
	var calls []call
	srv, _ := newTestDashboard(t, "hunter2", func(c *Config) {
		c.Purge = func(app string, dryRun bool) evict.PurgeResult {
			calls = append(calls, call{app, dryRun})
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
