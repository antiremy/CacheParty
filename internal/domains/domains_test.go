package domains

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func testMatcher(t testing.TB) *Matcher {
	t.Helper()
	m := NewMatcher()
	for _, e := range []struct{ name, pat string }{
		{"steam", "lancache.steamcontent.com"},
		{"steam", "*.steamcontent.com"},
		{"blizzard", "dist.blizzard.com"},
		{"blizzard", "*.blizzard.com"},
		{"blizzard", "*.cdn.blizzard.com"},
		{"special", "eu.cdn.blizzard.com"},
		{"epic", "*.epicgames.com"},
	} {
		if err := m.Add(e.name, e.pat); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func TestLookup(t *testing.T) {
	m := testMatcher(t)
	tests := []struct {
		host string
		want string
		ok   bool
	}{
		{"lancache.steamcontent.com", "steam", true},
		{"cache1-lax1.steamcontent.com", "steam", true},
		{"a.b.steamcontent.com", "steam", true},
		{"steamcontent.com", "", false}, // wildcard does not match the apex
		{"dist.blizzard.com", "blizzard", true},
		{"DIST.Blizzard.COM", "blizzard", true},
		{"dist.blizzard.com.", "blizzard", true},
		{"foo.blizzard.com", "blizzard", true},
		{"us.cdn.blizzard.com", "blizzard", true},
		{"eu.cdn.blizzard.com", "special", true}, // exact beats wildcard
		{"blizzard.com", "", false},
		{"evilsteamcontent.com", "", false}, // proper suffix matching, not substring
		{"steamcontent.com.evil.org", "", false},
		{"download.epicgames.com", "epic", true},
		{"example.org", "", false},
		{"", "", false},
		{".steamcontent.com", "", false},
	}
	for _, tt := range tests {
		got, ok := m.Lookup(tt.host)
		if got != tt.want || ok != tt.ok {
			t.Errorf("Lookup(%q) = %q,%v; want %q,%v", tt.host, got, ok, tt.want, tt.ok)
		}
	}
}

func TestIdentify(t *testing.T) {
	m := testMatcher(t)
	tests := []struct{ host, ua, want string }{
		{"cache1.steamcontent.com:80", "", "steam"},
		{"anything.example.org", "Valve/Steam HTTP Client 1.0 (appid)", "steam"},
		{"anything.example.org", "valve/steam http client 1.0", "anything.example.org"},
		{"anything.example.org", "Valve/Steam HTTP Client 1.05", "steam"}, // regex has no end anchor
		{"Unknown.Example.org:8080", "curl", "unknown.example.org"},
		{"10.0.0.5:80", "", "10.0.0.5"},
		{"[::1]:80", "", "::1"},
		{"dist.blizzard.com", "Blizzard Downloader", "blizzard"},
	}
	for _, tt := range tests {
		if got := m.Identify(tt.host, tt.ua); got != tt.want {
			t.Errorf("Identify(%q,%q) = %q; want %q", tt.host, tt.ua, got, tt.want)
		}
	}
}

func TestAddFirstWinsAndErrors(t *testing.T) {
	m := NewMatcher()
	_ = m.Add("a", "x.com")
	_ = m.Add("b", "x.com")
	_ = m.Add("a", "*.y.com")
	_ = m.Add("b", "*.y.com")
	if got, _ := m.Lookup("x.com"); got != "a" {
		t.Errorf("exact: got %q", got)
	}
	if got, _ := m.Lookup("q.y.com"); got != "a" {
		t.Errorf("wild: got %q", got)
	}
	for _, bad := range []string{"", "*.", "foo*.com", "*.a.*.com", "a.*.com"} {
		if err := m.Add("n", bad); err == nil {
			t.Errorf("Add(%q) should fail", bad)
		}
	}
	if m.Len() != 2 {
		t.Errorf("Len = %d", m.Len())
	}
}

func TestLookupDoesNotAllocate(t *testing.T) {
	m := testMatcher(t)
	if n := testing.AllocsPerRun(100, func() {
		m.Identify("cache1-lax1.steamcontent.com:80", "curl/8")
		m.Identify("nothing.example.org", "curl/8")
	}); n != 0 {
		t.Errorf("allocs = %v, want 0", n)
	}
}

func BenchmarkIdentify(b *testing.B) {
	m := testMatcher(b)
	for i := 0; i < 5000; i++ { // a realistically large list
		_ = m.Add("filler", "host"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+".example"+string(rune('a'+i/676))+".net")
	}
	b.Run("wildcard", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Identify("cache1-lax1.steamcontent.com:80", "curl/8")
		}
	})
	b.Run("miss", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Identify("nothing.example.org:80", "curl/8")
		}
	})
	b.Run("steam-ua", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			m.Identify("x.example.org", "Valve/Steam HTTP Client 1.0")
		}
	})
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"cache_domains.json": `{"cache_domains":[
			{"name":"steam","domain_files":["steam.txt"]},
			{"name":"epic","domain_files":["epic.txt","../escape.txt"]}]}`,
		"steam.txt": "# comment\n\n  *.steamcontent.com  \nlancache.steamcontent.com\r\n",
		"epic.txt":  "*.epicgames.com\nbad*host.com\n",
	})
	m, skipped, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Identify("x.steamcontent.com", ""); got != "steam" {
		t.Errorf("steam: %q", got)
	}
	if got := m.Identify("dl.epicgames.com", ""); got != "epic" {
		t.Errorf("epic: %q", got)
	}
	if len(skipped) != 2 {
		t.Errorf("skipped = %v", skipped)
	}
	if _, _, err := LoadDir(t.TempDir()); err == nil {
		t.Error("expected error for missing cache_domains.json")
	}
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "repo-master/", Typeflag: tar.TypeDir, Mode: 0o755})
	for name, content := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content))})
		_, _ = tw.Write([]byte(content))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestFetchTarballAndSwap(t *testing.T) {
	archive := makeTarGz(t, map[string]string{
		"repo-master/cache_domains.json": `{"cache_domains":[]}`,
		"repo-master/sub/a.txt":          "a",
		"repo-master/../../evil.txt":     "nope",
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(archive) }))
	defer srv.Close()

	parent := t.TempDir()
	dir := filepath.Join(parent, "cachedomains")
	writeTree(t, dir, map[string]string{"stale.txt": "old"})

	tmp := t.TempDir()
	if err := fetchTarball(context.Background(), srv.Client(), srv.URL, tmp); err != nil {
		t.Fatal(err)
	}
	if err := swapDir(tmp, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sub", "a.txt")); err != nil {
		t.Errorf("extracted file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "stale.txt")); err == nil {
		t.Error("old content should be replaced")
	}
	for _, p := range []string{filepath.Join(parent, "evil.txt"), dir + ".old"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s should not exist", p)
		}
	}
}

func TestFetchTarballHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	defer srv.Close()
	if err := fetchTarball(context.Background(), srv.Client(), srv.URL, t.TempDir()); err == nil {
		t.Fatal("expected error on 404")
	}
}

func TestTarballURL(t *testing.T) {
	u, ok := TarballURL("https://github.com/uklans/cache-domains.git", "master")
	if !ok || u != "https://codeload.github.com/uklans/cache-domains/tar.gz/refs/heads/master" {
		t.Errorf("got %q,%v", u, ok)
	}
	if _, ok := TarballURL("https://gitlab.com/a/b.git", "main"); ok {
		t.Error("non-github should fall back to git")
	}
	if _, ok := TarballURL("https://github.com/onlyowner", "main"); ok {
		t.Error("malformed path should not match")
	}
}
