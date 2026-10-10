package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminClientAddress(t *testing.T) {
	t.Setenv("DASHBOARD_PASSWORD", "pw")
	for addr, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080",
		"0.0.0.0:8080":   "http://127.0.0.1:8080",
		"[::]:8080":      "http://127.0.0.1:8080",
		"10.0.0.5:9000":  "http://10.0.0.5:9000",
		"localhost:8081": "http://localhost:8081",
	} {
		c, err := newAdminClient(addr)
		if err != nil || c.base != want {
			t.Errorf("%s: %v, %v; want %s", addr, c, err, want)
		}
	}
	if _, err := newAdminClient("off"); err == nil {
		t.Error("METRICS_ADDR off: no error")
	}
	t.Setenv("DASHBOARD_PASSWORD", "")
	if _, err := newAdminClient(":8080"); err == nil || !strings.Contains(err.Error(), "DASHBOARD_PASSWORD") {
		t.Errorf("no password: %v", err)
	}
}

// fakeDashboard answers path with reply, after checking the password, and
// records the request bodies.
func fakeDashboard(t *testing.T, path string, reply any) *[]map[string]any {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != path {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer pw" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"wrong password"}`))
			return
		}
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("METRICS_ADDR", srv.Listener.Addr().String())
	t.Setenv("DASHBOARD_PASSWORD", "pw")
	return &bodies
}

func TestVerifyCmd(t *testing.T) {
	bodies := fakeDashboard(t, "/api/cache/verify", map[string]any{
		"dryRun": true, "files": 12, "orphans": 1, "missing": 2, "corrupt": 0, "freed": 3 << 20,
	})
	var out strings.Builder
	if err := verifyCmd([]string{"-dry-run"}, &out); err != nil {
		t.Fatal(err)
	}
	if len(*bodies) != 1 || (*bodies)[0]["dryRun"] != true {
		t.Fatalf("sent %v", *bodies)
	}
	for _, want := range []string{"checked 12 chunk files", "orphan files (not in the index): 1", "missing files (in the index, not on disk): 2", "would remove 3.0 MiB", "error.log"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}

	t.Setenv("DASHBOARD_PASSWORD", "wrong")
	if err := verifyCmd(nil, &out); err == nil || err.Error() != "wrong password" {
		t.Errorf("wrong password: %v", err)
	}
}

func TestPurgeCmd(t *testing.T) {
	bodies := fakeDashboard(t, "/api/cache/purge", map[string]any{"objects": 3, "chunks": 7, "bytes": 7 << 20})

	var out strings.Builder
	err := purgeCmd([]string{"steam/depot/990081"}, strings.NewReader("n\n"), &out)
	if err == nil || len(*bodies) != 1 {
		t.Fatalf("declined: err %v, %d requests", err, len(*bodies))
	}
	if b := (*bodies)[0]; b["service"] != "steam" || b["kind"] != "depot" || b["id"] != "990081" || b["confirm"] != false {
		t.Fatalf("preview sent %v", b)
	}
	if !strings.Contains(out.String(), "steam/depot/990081: 7 chunks in 3 files, 7.0 MiB") {
		t.Errorf("preview output:\n%s", out.String())
	}

	// No terminal (docker exec without -i) reads as "no".
	if err := purgeCmd([]string{"steam/depot/990081"}, strings.NewReader(""), &out); err == nil {
		t.Error("removed without confirmation")
	}

	out.Reset()
	if err := purgeCmd([]string{"steam/depot/990081"}, strings.NewReader("y\n"), &out); err != nil {
		t.Fatal(err)
	}
	if n := len(*bodies); n != 4 || (*bodies)[n-1]["confirm"] != true {
		t.Fatalf("confirmed: %v", *bodies)
	}
	if !strings.Contains(out.String(), "removed 7 chunks, freed 7.0 MiB") {
		t.Errorf("output:\n%s", out.String())
	}

	if err := purgeCmd([]string{"-yes", "steam/depot/990081"}, strings.NewReader(""), &out); err != nil || len(*bodies) != 6 {
		t.Fatalf("-yes: %v, %d requests", err, len(*bodies))
	}
	for _, bad := range [][]string{nil, {"steam"}, {"steam/depot"}, {"steam//1"}, {"a/b/c/d"}, {"a/b/c", "x"}} {
		if err := purgeCmd(bad, nil, &out); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPurgeCmdNothingCached(t *testing.T) {
	bodies := fakeDashboard(t, "/api/cache/purge", map[string]any{})
	var out strings.Builder
	if err := purgeCmd([]string{"steam/depot/1"}, nil, &out); err != nil || len(*bodies) != 1 {
		t.Fatalf("%v, %d requests", err, len(*bodies))
	}
	if !strings.Contains(out.String(), "nothing cached") {
		t.Errorf("output: %s", out.String())
	}
}
