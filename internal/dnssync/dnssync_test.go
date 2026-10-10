package dnssync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"cacheparty/internal/domains"
)

var testPatterns = []domains.Pattern{
	{Service: "blizzard", Pattern: "*.blizzard.com"},
	{Service: "blizzard", Pattern: "level3.blizzard.com"},
	{Service: "steam", Pattern: "*.steamcontent.com"},
	{Service: "steam", Pattern: "lancache.steamcontent.com"},
}

// fakeServer is an in-memory DNS server API. entries lists what it holds
// in a form common to the fakes: "name ip" per record, "zone name" per
// zone.
type fakeServer interface {
	http.Handler
	entries() []string
	seed(entry string) // a record made by hand
}

// fakePihole holds misc.dnsmasq_lines.
type fakePihole struct {
	mu       sync.Mutex
	lines    []string
	sessions int
}

func (f *fakePihole) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/api/auth" {
		switch r.Method {
		case http.MethodPost:
			var in struct{ Password string }
			json.NewDecoder(r.Body).Decode(&in)
			if in.Password != "pw" {
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"valid": false, "message": "password incorrect"}})
				return
			}
			f.sessions++
			json.NewEncoder(w).Encode(map[string]any{"session": map[string]any{"valid": true, "sid": "s1"}})
		case http.MethodDelete:
			f.sessions--
			w.WriteHeader(http.StatusNoContent)
		}
		return
	}
	if r.Header.Get("X-FTL-SID") != "s1" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/config/misc/dnsmasq_lines":
		json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{"misc": map[string]any{"dnsmasq_lines": f.lines}}})
	case r.Method == http.MethodPatch && r.URL.Path == "/api/config":
		var in piholeLines
		json.NewDecoder(r.Body).Decode(&in)
		f.lines = in.Config.Misc.DnsmasqLines
		json.NewEncoder(w).Encode(map[string]any{"config": map[string]any{}})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakePihole) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, l := range f.lines {
		if rest, ok := strings.CutPrefix(l, "address=/"); ok {
			d, ip, _ := strings.Cut(rest, "/")
			out = append(out, d+" "+ip)
		} else {
			out = append(out, l)
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakePihole) seed(e string) {
	d, ip, _ := strings.Cut(e, " ")
	f.lines = append(f.lines, "address=/"+strings.TrimPrefix(d, "*.")+"/"+ip)
}

// fakeAdGuard holds rewrites.
type fakeAdGuard struct {
	mu       sync.Mutex
	rewrites []rewrite
}

func (f *fakeAdGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, _ := r.BasicAuth(); u != "admin" || p != "pw" {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	var in rewrite
	switch r.URL.Path {
	case "/control/rewrite/list":
		json.NewEncoder(w).Encode(f.rewrites)
	case "/control/rewrite/add":
		json.NewDecoder(r.Body).Decode(&in)
		f.rewrites = append(f.rewrites, in)
	case "/control/rewrite/delete":
		json.NewDecoder(r.Body).Decode(&in)
		i := slices.Index(f.rewrites, in)
		if i < 0 {
			http.Error(w, "rewrite rule not found", http.StatusBadRequest)
			return
		}
		f.rewrites = slices.Delete(f.rewrites, i, i+1)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAdGuard) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.rewrites {
		out = append(out, r.Domain+" "+r.Answer)
	}
	sort.Strings(out)
	return out
}

func (f *fakeAdGuard) seed(e string) {
	d, ip, _ := strings.Cut(e, " ")
	f.rewrites = append(f.rewrites, rewrite{d, ip})
}

// fakeTechnitium holds zones of A/AAAA records.
type fakeTechnitium struct {
	mu    sync.Mutex
	zones map[string][][2]string // zone -> (name, ip)
}

func (f *fakeTechnitium) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(resp any) { json.NewEncoder(w).Encode(map[string]any{"status": "ok", "response": resp}) }
	fail := func(msg string) { json.NewEncoder(w).Encode(map[string]any{"status": "error", "errorMessage": msg}) }
	if r.Header.Get("Authorization") != "Bearer tok" {
		json.NewEncoder(w).Encode(map[string]any{"status": "invalid-token"})
		return
	}
	r.ParseForm()
	zone := r.PostForm.Get("zone")
	rec := [2]string{r.PostForm.Get("domain"), r.PostForm.Get("ipAddress")}
	switch r.URL.Path {
	case "/api/zones/list":
		var zs []map[string]string
		for z := range f.zones {
			zs = append(zs, map[string]string{"name": z, "type": "Forwarder"})
		}
		reply(map[string]any{"zones": zs, "totalPages": 1})
	case "/api/zones/create":
		if _, ok := f.zones[zone]; ok {
			fail("zone already exists")
			return
		}
		if r.PostForm.Get("forwarder") != "this-server" {
			fail("want this-server")
			return
		}
		f.zones[zone] = nil
		reply(map[string]any{"domain": zone})
	case "/api/zones/delete":
		delete(f.zones, zone)
		reply(nil)
	case "/api/zones/records/get":
		var recs []map[string]any
		for _, x := range f.zones[zone] {
			typ := "A"
			if strings.Contains(x[1], ":") {
				typ = "AAAA"
			}
			recs = append(recs, map[string]any{"name": x[0], "type": typ, "rData": map[string]string{"ipAddress": x[1]}})
		}
		recs = append(recs, map[string]any{"name": zone, "type": "FWD", "rData": map[string]string{"forwarder": "this-server"}})
		reply(map[string]any{"records": recs})
	case "/api/zones/records/add":
		if _, ok := f.zones[zone]; !ok {
			fail("no such zone")
			return
		}
		if typ := r.PostForm.Get("type"); (typ == "AAAA") != strings.Contains(rec[1], ":") {
			fail("wrong type " + typ)
			return
		}
		f.zones[zone] = append(f.zones[zone], rec)
		reply(nil)
	case "/api/zones/records/delete":
		i := slices.Index(f.zones[zone], rec)
		if i < 0 {
			fail("no such record")
			return
		}
		f.zones[zone] = slices.Delete(f.zones[zone], i, i+1)
		reply(nil)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeTechnitium) entries() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for z, recs := range f.zones {
		out = append(out, "zone "+z)
		for _, x := range recs {
			out = append(out, x[0]+" "+x[1])
		}
	}
	sort.Strings(out)
	return out
}

func (f *fakeTechnitium) seed(e string) {
	d, ip, _ := strings.Cut(e, " ")
	f.zones["steamcontent.com"] = append(f.zones["steamcontent.com"], [2]string{d, ip})
}

func TestSyncBackends(t *testing.T) {
	cases := []struct {
		kind     string
		fake     fakeServer
		in       TargetInput
		first    []string // after the first sync, with the hand-made entries
		afterIPs []string // after switching addresses and excluding blizzard
	}{
		{
			kind: PiHole, fake: &fakePihole{lines: []string{"server=/lan/192.168.1.1"}},
			in: TargetInput{Secret: "pw"},
			first: []string{
				"blizzard.com 10.0.0.5", "lancache.steamcontent.com 10.0.0.5", "level3.blizzard.com 10.0.0.5",
				"server=/lan/192.168.1.1", "steamcontent.com 10.0.0.5",
			},
			afterIPs: []string{
				"lancache.steamcontent.com 10.0.0.6", "lancache.steamcontent.com fd00::6",
				"server=/lan/192.168.1.1", "steamcontent.com 10.0.0.5", "steamcontent.com 10.0.0.6", "steamcontent.com fd00::6",
			},
		},
		{
			kind: AdGuard, fake: &fakeAdGuard{rewrites: []rewrite{{"nas.lan", "192.168.1.9"}}},
			in: TargetInput{Username: "admin", Secret: "pw"},
			first: []string{
				"*.blizzard.com 10.0.0.5", "*.steamcontent.com 10.0.0.5", "lancache.steamcontent.com 10.0.0.5",
				"level3.blizzard.com 10.0.0.5", "nas.lan 192.168.1.9",
			},
			afterIPs: []string{
				"*.steamcontent.com 10.0.0.5", "*.steamcontent.com 10.0.0.6", "*.steamcontent.com fd00::6",
				"lancache.steamcontent.com 10.0.0.6", "lancache.steamcontent.com fd00::6", "nas.lan 192.168.1.9",
			},
		},
		{
			kind: Technitium, fake: &fakeTechnitium{zones: map[string][][2]string{"home.lan": {{"nas.home.lan", "192.168.1.9"}}}},
			in: TargetInput{Secret: "tok"},
			first: []string{
				"*.blizzard.com 10.0.0.5", "*.steamcontent.com 10.0.0.5", "lancache.steamcontent.com 10.0.0.5",
				"level3.blizzard.com 10.0.0.5", "nas.home.lan 192.168.1.9",
				"zone blizzard.com", "zone home.lan", "zone steamcontent.com",
			},
			afterIPs: []string{
				"*.steamcontent.com 10.0.0.5", "*.steamcontent.com 10.0.0.6", "*.steamcontent.com fd00::6",
				"lancache.steamcontent.com 10.0.0.6", "lancache.steamcontent.com fd00::6", "nas.home.lan 192.168.1.9",
				"zone home.lan", "zone steamcontent.com",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			srv := httptest.NewServer(tc.fake)
			defer srv.Close()
			path := filepath.Join(t.TempDir(), "dns-sync.json")
			m, err := Open(path, testPatterns, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := m.SetAddresses([]string{"10.0.0.5"}, nil); err != nil {
				t.Fatal(err)
			}
			in := tc.in
			in.Kind, in.URL = tc.kind, srv.URL+"/admin/"
			id, err := m.PutTarget(in)
			if err != nil {
				t.Fatal(err)
			}
			sync := func(opt Options) Result {
				t.Helper()
				res, err := m.Sync(context.Background(), id, opt)
				if err != nil {
					t.Fatal(err)
				}
				if res.Error != "" {
					t.Fatalf("sync error: %s", res.Error)
				}
				return res
			}
			check := func(want []string) {
				t.Helper()
				if got := tc.fake.entries(); !slices.Equal(got, want) {
					t.Fatalf("server holds\n  %q\nwant\n  %q", got, want)
				}
			}

			// The steam wildcard already exists, made by hand: it is
			// left alone and never becomes ours.
			tc.fake.seed("*.steamcontent.com 10.0.0.5")
			before := tc.fake.entries()
			if res := sync(Options{DryRun: true}); res.Added == 0 || res.Unchanged == 0 {
				t.Fatalf("dry run = %+v", res)
			}
			check(before)

			first := sync(Options{})
			check(tc.first)
			if again := sync(Options{}); again.Added != 0 || again.Removed != 0 || again.Unchanged != first.Added+first.Unchanged {
				t.Fatalf("second sync = %+v after %+v", again, first)
			}

			if err := m.SetAddresses([]string{"10.0.0.6", "fd00::6", "10.0.0.6"}, []string{"blizzard"}); err != nil {
				t.Fatal(err)
			}
			sync(Options{})
			check(tc.afterIPs)

			// Settings and what was added survive a reopen.
			m, err = Open(path, testPatterns, nil)
			if err != nil {
				t.Fatal(err)
			}
			st := m.State()
			if len(st.Targets) != 1 || !st.Targets[0].HasSecret || st.Targets[0].Last == nil || !slices.Equal(st.IPs, []string{"10.0.0.6", "fd00::6"}) {
				t.Fatalf("state after reopen = %+v", st)
			}
			if !st.Services[0].Excluded || st.Services[1].Excluded || st.Services[0].Domains != 2 {
				t.Fatalf("services = %+v", st.Services)
			}

			sync(Options{Remove: true})
			check(before)
			if st := m.State(); st.Targets[0].Managed != 0 {
				t.Fatalf("still managing %d records", st.Targets[0].Managed)
			}
			if p, ok := tc.fake.(*fakePihole); ok && p.sessions != 0 {
				t.Fatalf("%d Pi-hole sessions left open", p.sessions)
			}
		})
	}
}

func TestSyncReportsServerErrors(t *testing.T) {
	for _, tc := range []struct {
		in   TargetInput
		fake http.Handler
		want string
	}{
		{TargetInput{Kind: PiHole, Secret: "nope"}, &fakePihole{}, "password incorrect"},
		{TargetInput{Kind: AdGuard, Username: "admin", Secret: "nope"}, &fakeAdGuard{}, "403"},
		{TargetInput{Kind: Technitium, Secret: "nope"}, &fakeTechnitium{}, "token was refused"},
	} {
		srv := httptest.NewServer(tc.fake)
		m, _ := Open(filepath.Join(t.TempDir(), "f.json"), testPatterns, nil)
		m.SetAddresses([]string{"10.0.0.5"}, nil)
		tc.in.URL = srv.URL
		id, err := m.PutTarget(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		res, err := m.Sync(context.Background(), id, Options{})
		srv.Close()
		if err != nil || !strings.Contains(res.Error, tc.want) {
			t.Errorf("%s: result %+v, err %v; want error containing %q", tc.in.Kind, res, err, tc.want)
		}
	}
}

func TestPutTargetValidatesAndKeepsSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.json")
	m, _ := Open(path, nil, nil)
	for _, in := range []TargetInput{
		{Kind: "bind", URL: "http://x"},
		{Kind: PiHole, URL: "ftp://x"},
		{Kind: PiHole, URL: "pi.hole"},
		{Kind: Technitium, URL: "http://x:5380"}, // no token
	} {
		if _, err := m.PutTarget(in); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
	id, err := m.PutTarget(TargetInput{Kind: AdGuard, URL: "http://adguard.lan/", Username: "admin", Secret: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.PutTarget(TargetInput{ID: id, Kind: AdGuard, URL: "http://adguard.lan", Username: "admin", Auto: true}); err != nil {
		t.Fatal(err)
	}
	st := m.State()
	if tg := st.Targets[0]; !tg.HasSecret || !tg.Auto || tg.URL != "http://adguard.lan" {
		t.Fatalf("target = %+v", tg)
	}
	if b, _ := json.Marshal(st); strings.Contains(string(b), "s3cret") {
		t.Fatal("state exposes the secret")
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("sync file mode %v, err %v", fi.Mode(), err)
	}
	if err := m.SetAddresses([]string{"0.0.0.0"}, nil); err == nil {
		t.Fatal("unspecified address accepted")
	}
	if err := m.DeleteTarget(id); err != nil || len(m.State().Targets) != 0 {
		t.Fatalf("delete: %v", err)
	}
	if err := m.DeleteTarget(id); err != ErrNotFound {
		t.Fatalf("second delete: %v", err)
	}
}
