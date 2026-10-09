package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cacheparty/internal/config"
	"cacheparty/internal/dnssync"
	"cacheparty/internal/domains"
)

func TestHistoryRatesAndResolutions(t *testing.T) {
	h := newHistory()
	t0 := time.Unix(1000, 0)
	for i := 0; i <= 120; i++ {
		// 100 B/s sent, 10 B/s fetched for 2 minutes.
		h.sample(t0.Add(time.Duration(i)*time.Second), point{Sent: int64(100 * i), Fetched: int64(10 * i)},
			map[flowKey][2]int64{{Service: "steam"}: {int64(100 * i), int64(10 * i)}})
	}
	fine, end := h.series(false)
	if len(fine) != 120 || fine[0].Sent != 100 || fine[119].Fetched != 10 {
		t.Fatalf("fine: %d points, first %+v", len(fine), fine[0])
	}
	if !end.Equal(t0.Add(120 * time.Second)) {
		t.Fatalf("fine end = %v", end)
	}
	coarse, cend := h.series(true)
	if len(coarse) != 2 || coarse[1].Sent != 6000 {
		t.Fatalf("coarse = %+v", coarse)
	}
	if !cend.Equal(end) {
		t.Fatalf("coarse end %v, want %v", cend, end)
	}
	if r := h.serviceRates()[flowKey{Service: "steam"}]; r != [2]float64{100, 10} {
		t.Fatalf("steam rate = %v", r)
	}
}

func TestStatsReportDiskRates(t *testing.T) {
	var read, written int64
	d := New(Config{
		Traffic:  NewTraffic(),
		Stats:    func() Stats { return Stats{DiskRead: read, DiskWritten: written} },
		Cache:    func() (int64, int64) { return 0, 0 },
		Active:   func() int64 { return 0 },
		Settings: &config.Config{CacheRoot: t.TempDir()},
	})
	t0 := time.Unix(1000, 0)
	for i := 0; i <= 3; i++ {
		read, written = int64(4000*i), int64(1000*i)
		d.sample(t0.Add(time.Duration(i) * time.Second))
	}
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	var st statsJSON
	if err := json.NewDecoder(rec.Body).Decode(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.DiskRead) != 3 || st.DiskRead[2] != 4000 || st.DiskWrite[2] != 1000 {
		t.Fatalf("disk read %v, write %v", st.DiskRead, st.DiskWrite)
	}
	if st.Totals.DiskRead != 12000 || st.Totals.DiskWritten != 3000 {
		t.Fatalf("totals %+v", st.Totals)
	}
}

func TestRingWraps(t *testing.T) {
	r := newRing(3)
	for i := 1; i <= 5; i++ {
		r.push(point{Sent: int64(i)})
	}
	got := r.points()
	if len(got) != 3 || got[0].Sent != 3 || got[2].Sent != 5 {
		t.Fatalf("points = %+v", got)
	}
}

func get(path string) *http.Request {
	return httptest.NewRequest("GET", "http://cdn.example:80"+path, nil)
}

func TestTrafficFoldsExtraServicesIntoOther(t *testing.T) {
	tr := NewTraffic()
	for i := 0; i < maxServices+10; i++ {
		tr.Flow(fmt.Sprintf("host%d.example", i), get("/")).Sent.Add(1)
	}
	services := 0
	for k := range tr.snapshot() {
		if k.isService() {
			services++
		}
	}
	if services != maxServices+1 {
		t.Fatalf("%d services tracked", services)
	}
	if v := tr.snapshot()[flowKey{Service: otherService, App: app{"host", "cdn.example"}}]; v[0] != 10 {
		t.Fatalf("other = %v", v)
	}
}

func TestTrafficCountsGamesWithinServices(t *testing.T) {
	tr := NewTraffic()
	tr.Flow("steam", get("/depot/1001/chunk/aa")).Sent.Add(5)
	tr.Flow("steam", get("/depot/1001/manifest/1/5/2")).Sent.Add(7)
	tr.Flow("steam", get("/depot/2002/chunk/bb")).Fetched.Add(3)
	for i := 0; i < maxApps+4; i++ {
		tr.Flow("steam", get(fmt.Sprintf("/depot/%d/chunk/cc", 10000+i))).Sent.Add(1)
	}
	// Bytes counted on a game's flow show up in its service through Parent;
	// the proxy adds them that way, here they are checked directly.
	snap := tr.snapshot()
	if v := snap[flowKey{Service: "steam", App: app{"depot", "1001"}}]; v != [2]int64{12, 0} {
		t.Fatalf("depot 1001 = %v", v)
	}
	if v := snap[flowKey{Service: "steam", App: app{"depot", "2002"}}]; v != [2]int64{0, 3} {
		t.Fatalf("depot 2002 = %v", v)
	}
	if v := snap[flowKey{Service: "steam", App: otherApp}]; v[0] != 6 {
		t.Fatalf("other depots = %v, want the 6 past the limit", v)
	}
}

func TestTrafficCountsClientsAndUpstreams(t *testing.T) {
	tr := NewTraffic()
	req := func(addr, url string) *http.Request {
		r := httptest.NewRequest("GET", url, nil)
		r.RemoteAddr = addr
		return r
	}
	// The proxy reports through the whole chain; Also feeds the lists.
	a := tr.Flow("steam", req("192.168.1.5:5000", "http://cdn1.example/depot/1/x"))
	a.Parent.Sent.Add(0) // service flow exists
	for _, f := range a.Also {
		f.Sent.Add(10)
		f.Fetched.Add(4)
	}
	for i := 0; i < maxClients+3; i++ {
		for _, f := range tr.Flow("steam", req(fmt.Sprintf("10.%d.%d.1:1", i/250, i%250), "http://cdn2.example/x")).Also {
			f.Sent.Add(1)
		}
	}
	snap := tr.snapshot()
	if v := snap[flowKey{Group: groupClient, Name: "192.168.1.5"}]; v != [2]int64{10, 4} {
		t.Fatalf("client = %v", v)
	}
	if v := snap[flowKey{Group: groupUpstream, Name: "cdn1.example"}]; v != [2]int64{0, 4} {
		t.Fatalf("upstream = %v", v)
	}
	if v := snap[flowKey{Group: groupClient, Name: otherName}]; v[0] != 4 {
		t.Fatalf("other clients = %v", v)
	}
}

func TestTrafficCountsHTTPSDomains(t *testing.T) {
	tr := NewTraffic()
	sent, received := tr.HTTPS("launcher.example")
	sent.Add(100)
	received.Add(7)
	if s2, _ := tr.HTTPS("launcher.example"); s2 != sent {
		t.Fatal("the same name got new counters")
	}
	for i := 0; i < maxHTTPS+2; i++ {
		s, _ := tr.HTTPS(fmt.Sprintf("h%d.example", i))
		s.Add(1)
	}
	snap := tr.snapshot()
	if v := snap[flowKey{Group: groupHTTPS, Name: "launcher.example"}]; v != [2]int64{100, 7} {
		t.Fatalf("launcher = %v", v)
	}
	if v := snap[flowKey{Group: groupHTTPS, Name: otherName}]; v[0] != 3 {
		t.Fatalf("other = %v, want the 3 past the limit", v)
	}

	d := New(Config{
		Traffic: tr, Settings: &config.Config{SNIAddr: ":443", CacheRoot: t.TempDir()},
		Stats: func() Stats { return Stats{} }, Cache: func() (int64, int64) { return 0, 0 }, Active: func() int64 { return 0 },
	})
	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, httptest.NewRequest("GET", "/api/stats", nil))
	var out statsJSON
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.HTTPS) != maxHTTPS+1 || out.HTTPS[0] != (httpsJSON{Name: "launcher.example", Sent: 100, Received: 7}) {
		t.Fatalf("https: %d entries, first %+v", len(out.HTTPS), out.HTTPS[0])
	}
}

func TestAppOf(t *testing.T) {
	for _, c := range []struct {
		service, url string
		want         app
	}{
		{"steam", "http://cache1-ams1.steamcontent.com/depot/1245621/chunk/0a1b2c", app{"depot", "1245621"}},
		{"steam", "http://cache1.steamcontent.com/depot/228988/manifest/123/5/456", app{"depot", "228988"}},
		{"steam", "http://cache1.steamcontent.com/depot/abc/chunk/1", app{"host", "cache1.steamcontent.com"}},
		{"steam", "http://clientconfig.akamai.steamstatic.com/appinfo/730/sha/x", app{"host", "clientconfig.akamai.steamstatic.com"}},
		{"blizzard", "http://level3.blizzard.com/tpr/wow/data/ab/cd/abcdef", app{"product", "wow"}},
		{"blizzard", "http://level3.blizzard.com/tpr/fenris/config/ab/cd/x", app{"product", "fenris"}},
		{"epicgames", "http://epicgames-download1.akamaized.net/Builds/Fortnite/CloudDir/ChunksV4/1/x.chunk", app{"app", "Fortnite"}},
		{"epicgames", "http://epicgames-download1.akamaized.net/Builds/Org/o-abc/9f8e7d/default/ChunksV4/x", app{"app", "9f8e7d"}},
		{"epicgames", "http://download.epicgames.com/Builds/Org/", app{"host", "download.epicgames.com"}},
		{"riot", "http://LoL.dyn.riotcdn.net:80/channels/public/x.bundle", app{"host", "lol.dyn.riotcdn.net"}},
		{"wsus", "http://[2001:db8::1]:80/x", app{"host", "2001:db8::1"}},
	} {
		if got := appOf(c.service, httptest.NewRequest("GET", c.url, nil)); got != c.want {
			t.Errorf("appOf(%s, %s) = %v, want %v", c.service, c.url, got, c.want)
		}
	}
}

func TestServeApps(t *testing.T) {
	tr := NewTraffic()
	d := New(Config{
		Traffic:  tr,
		Stats:    func() Stats { return Stats{} },
		Settings: &config.Config{},
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	t0 := time.Unix(1000, 0)
	d.sample(t0)
	tr.Flow("steam", get("/depot/1001/chunk/a")).Sent.Add(100)
	tr.Flow("steam", get("/depot/2002/chunk/a")).Sent.Add(900)
	tr.Flow("blizzard", get("/tpr/wow/data/a")).Sent.Add(1)
	d.sample(t0.Add(time.Second))

	rec := httptest.NewRecorder()
	d.ServeHTTP(rec, httptest.NewRequest("GET", "/api/services/steam", nil))
	var got appsJSON
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got.Service != "steam" || len(got.Apps) != 2 {
		t.Fatalf("got %+v", got)
	}
	a := got.Apps[0]
	if a.ID != "2002" || a.Kind != "depot" || a.Sent != 900 || a.SentRate != 900 || a.LastActive != t0.Add(time.Second).UnixMilli() {
		t.Fatalf("first app = %+v", a)
	}

	rec = httptest.NewRecorder()
	d.ServeHTTP(rec, httptest.NewRequest("GET", "/api/services/nothing", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"apps":[]`) {
		t.Fatalf("unknown service: %d %s", rec.Code, rec.Body)
	}
}

// newTestDashboard serves a Dashboard; opts adjust its Config.
func newTestDashboard(t *testing.T, password string, opts ...func(*Config)) (*httptest.Server, *config.Config) {
	t.Helper()
	env := func(k string) string {
		if k == "CACHE_ROOT" {
			return t.TempDir()
		}
		return ""
	}
	o, _, err := config.LoadOverrides(filepath.Join(t.TempDir(), "settings.json"), env)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(o.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	applied := &config.Config{}
	c := Config{
		Hostname: "cachebox",
		Version:  "abc1234",
		Traffic:  NewTraffic(),
		Stats:    func() Stats { return Stats{} },
		Cache:    func() (int64, int64) { return 3, 3 << 20 },
		Active:   func() int64 { return 2 },
		Settings: cfg,
		Override: o,
		Apply:    func(c *config.Config) { *applied = *c },
		Password: password,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	for _, opt := range opts {
		opt(&c)
	}
	d := New(c)
	d.sample(time.Now())
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	return srv, applied
}

func put(t *testing.T, srv *httptest.Server, body, password string, hdr map[string]string) (int, string) {
	t.Helper()
	return send(t, srv, "PUT", "/api/settings", body, password, hdr)
}

func send(t *testing.T, srv *httptest.Server, method, path, body, password string, hdr map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if password != "" {
		req.Header.Set("Authorization", "Bearer "+password)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSettingsUpdate(t *testing.T) {
	srv, applied := newTestDashboard(t, "hunter2")

	if code, _ := put(t, srv, `{"values":{"CACHE_DISK_SIZE":"2t"}}`, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no password: %d", code)
	}
	if code, _ := put(t, srv, `{"values":{"CACHE_DISK_SIZE":"2t"}}`, "hunter2", map[string]string{"Origin": "http://evil.example"}); code != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", code)
	}
	if code, body := put(t, srv, `{"values":{"CACHE_DISK_SIZE":"huge"}}`, "hunter2", nil); code != http.StatusBadRequest || !strings.Contains(body, "CACHE_DISK_SIZE") {
		t.Fatalf("invalid value: %d %s", code, body)
	}
	if code, body := put(t, srv, `{"values":{"HTTP_ADDR":":81"}}`, "hunter2", nil); code != http.StatusBadRequest {
		t.Fatalf("restart-only setting: %d %s", code, body)
	}
	if applied.CacheDiskSize != 0 {
		t.Fatal("a rejected change was applied")
	}

	code, body := put(t, srv, `{"values":{"CACHE_DISK_SIZE":" 2t ","CACHE_MAX_PREFETCHES":"8"}}`, "hunter2", map[string]string{"Origin": srv.URL})
	if code != http.StatusOK {
		t.Fatalf("valid change: %d %s", code, body)
	}
	if applied.CacheDiskSize != 2<<40 || applied.MaxPrefetches != 8 {
		t.Fatalf("applied %d, %d", applied.CacheDiskSize, applied.MaxPrefetches)
	}
	var got settingsJSON
	json.Unmarshal([]byte(body), &got)
	for _, s := range got.Settings {
		if s.Key == "CACHE_DISK_SIZE" && (s.Value != "2t" || s.Source != "dashboard") {
			t.Fatalf("returned %+v", s)
		}
		if s.Key == "DASHBOARD_PASSWORD" || strings.Contains(s.Value, "hunter2") {
			t.Fatal("password exposed")
		}
	}

	// The stats endpoint reflects the new limit.
	resp, err := http.Get(srv.URL + "/api/stats")
	if err != nil {
		t.Fatal(err)
	}
	var st statsJSON
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.Cache.Limit != 2<<40 || st.Active != 2 || st.Hostname != "cachebox" || st.Version != "abc1234" {
		t.Fatalf("stats: limit %d active %d host %q version %q", st.Cache.Limit, st.Active, st.Hostname, st.Version)
	}
}

func TestSettingsBypassDomains(t *testing.T) {
	srv, applied := newTestDashboard(t, "hunter2")
	code, body := put(t, srv, `{"values":{"BYPASS_DOMAINS":" a.com, *.b.net ,bad host"}}`, "hunter2", nil)
	if code != http.StatusOK || applied.BypassDomains != "a.com, *.b.net ,bad host" {
		t.Fatalf("set: %d %s; applied %q", code, body, applied.BypassDomains)
	}
	var got settingsJSON
	json.Unmarshal([]byte(body), &got)
	for _, s := range got.Settings {
		if s.Key == "BYPASS_DOMAINS" && (!s.Live || s.Source != "dashboard" || s.Value != "a.com, *.b.net ,bad host") {
			t.Fatalf("returned %+v", s)
		}
	}
	if code, body = put(t, srv, `{"values":{"BYPASS_DOMAINS":null}}`, "hunter2", nil); code != http.StatusOK || applied.BypassDomains != "" {
		t.Fatalf("reset: %d %s; applied %q", code, body, applied.BypassDomains)
	}
}

func TestSettingsReadOnlyWithoutPassword(t *testing.T) {
	srv, _ := newTestDashboard(t, "")
	if code, _ := put(t, srv, `{"values":{"CACHE_DISK_SIZE":"2t"}}`, "anything", nil); code != http.StatusForbidden {
		t.Fatalf("read-only dashboard accepted a change: %d", code)
	}
	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	var got settingsJSON
	json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	if got.Editable {
		t.Fatal("editable without a password")
	}
}

func TestDNSSync(t *testing.T) {
	// An AdGuard Home that accepts anything.
	var added []string
	agh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/control/rewrite/list":
			io.WriteString(w, "[]")
		case "/control/rewrite/add":
			b, _ := io.ReadAll(r.Body)
			added = append(added, string(b))
		}
	}))
	defer agh.Close()
	m, err := dnssync.Open(filepath.Join(t.TempDir(), "dns.json"), []domains.Pattern{{Service: "steam", Pattern: "*.steamcontent.com"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := newTestDashboard(t, "hunter2", func(c *Config) { c.DNSSync = m })

	if code, _ := send(t, srv, "PUT", "/api/dns", `{"ips":["10.0.0.5"]}`, "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no password: %d", code)
	}
	if code, body := send(t, srv, "PUT", "/api/dns", `{"ips":["nope"]}`, "hunter2", nil); code != http.StatusBadRequest {
		t.Fatalf("bad address: %d %s", code, body)
	}
	if code, body := send(t, srv, "PUT", "/api/dns", `{"ips":["10.0.0.5"]}`, "hunter2", nil); code != http.StatusOK {
		t.Fatalf("set address: %d %s", code, body)
	}
	code, body := send(t, srv, "POST", "/api/dns/servers", `{"kind":"adguard","url":"`+agh.URL+`","username":"admin","secret":"agh-pass"}`, "hunter2", nil)
	if code != http.StatusOK || strings.Contains(body, "agh-pass") {
		t.Fatalf("add server: %d %s", code, body)
	}
	var st dnsJSON
	json.Unmarshal([]byte(body), &st)
	if !st.Enabled || !st.Editable || len(st.Targets) != 1 || !st.Targets[0].HasSecret || len(st.Services) != 1 {
		t.Fatalf("state %+v", st)
	}
	id := st.Targets[0].ID

	code, body = send(t, srv, "POST", "/api/dns/servers/"+id+"/sync", `{}`, "hunter2", nil)
	var res dnsSyncJSON
	json.Unmarshal([]byte(body), &res)
	if code != http.StatusOK || res.Result.Added != 1 || res.Result.Error != "" || res.State.Targets[0].Managed != 1 {
		t.Fatalf("sync: %d %s", code, body)
	}
	if len(added) != 1 || !strings.Contains(added[0], `"*.steamcontent.com"`) {
		t.Fatalf("AdGuard got %q", added)
	}
	if code, _ := send(t, srv, "POST", "/api/dns/servers/nope/sync", `{}`, "hunter2", nil); code != http.StatusNotFound {
		t.Fatalf("unknown server: %d", code)
	}
	if code, body := send(t, srv, "DELETE", "/api/dns/servers/"+id, ``, "hunter2", nil); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, body)
	}
}

func TestDNSSyncOff(t *testing.T) {
	srv, _ := newTestDashboard(t, "hunter2")
	if code, _ := send(t, srv, "PUT", "/api/dns", `{"ips":["10.0.0.5"]}`, "hunter2", nil); code != http.StatusNotFound {
		t.Fatalf("change with sync off: %d", code)
	}
	code, body := send(t, srv, "GET", "/api/dns", "", "", nil)
	if code != http.StatusOK || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("state with sync off: %d %s", code, body)
	}
}

func TestServesPage(t *testing.T) {
	srv, _ := newTestDashboard(t, "")
	for _, path := range []string{"/", "/app.js", "/app.css", "/dns.html", "/dns.js"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("%s: %d, CSP %q", path, resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
		}
	}
}

func TestAppendAddrsKeepsReachableAddresses(t *testing.T) {
	var addrs []net.Addr
	for _, s := range []string{"192.168.1.10/24", "127.0.0.1/8", "169.254.3.4/16", "2001:db8::5/64", "fe80::1/64", "::1/128", "fd00::7/64"} {
		ip, ipn, _ := net.ParseCIDR(s)
		ipn.IP = ip
		addrs = append(addrs, ipn)
	}
	v4, v6 := appendAddrs(nil, nil, "eth0", addrs)
	if len(v4) != 1 || v4[0] != (addrJSON{"eth0", "192.168.1.10"}) {
		t.Fatalf("ipv4 = %+v", v4)
	}
	if len(v6) != 2 || v6[0].Address != "2001:db8::5" || v6[1].Address != "fd00::7" {
		t.Fatalf("ipv6 = %+v", v6)
	}
}
