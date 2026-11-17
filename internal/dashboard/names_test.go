package dashboard

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGameNamesReloadAndKeepLastGood(t *testing.T) {
	path := filepath.Join(t.TempDir(), "names.json")
	d := New(Config{GameNamesFile: path})
	read := func() string {
		t.Helper()
		w := httptest.NewRecorder()
		d.ServeHTTP(w, httptest.NewRequest("GET", "/api/game-names", nil))
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("response: %v", w)
		}
		return strings.TrimSpace(w.Body.String())
	}
	if got := read(); got != `{"names":{},"steamAppIds":{}}` {
		t.Fatal(got)
	}
	good := `{"names":{"steam":{"depot":{"214491":"Alien: Isolation"}}},"steamAppIds":{"214491":"214490"}}`
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(good)
	if got := read(); got != good {
		t.Fatal(got)
	}
	for _, bad := range []string{`{`, `{"names":null}`, `{"names":{"steam":{"depot":{"214491":""}}}}`,
		`{"names":{},"steamAppIds":{"214491":"214490"}}`,
		strings.Replace(good, `"214490"`, `"https://example.com/"`, 1), good + strings.Repeat(" ", 4<<20)} {
		write(bad)
		if got := read(); got != good {
			t.Fatalf("lost last good names: %s", got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if got := read(); got != good {
		t.Fatal(got)
	}
	write(`{"names":{},"steamAppIds":{}}`)
	if got := read(); got != `{"names":{},"steamAppIds":{}}` {
		t.Fatal(got)
	}
}

func TestGameNamesDisabled(t *testing.T) {
	d := New(Config{})
	w := httptest.NewRecorder()
	d.ServeHTTP(w, httptest.NewRequest("GET", "/api/game-names", nil))
	if strings.TrimSpace(w.Body.String()) != `{"names":{},"steamAppIds":{}}` {
		t.Fatal(w.Body.String())
	}
}
