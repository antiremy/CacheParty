// Package dashboard serves a web page on the metrics listener showing live
// throughput, per-service and per-game traffic and cache usage, and lets an operator
// change the settings that can be applied without a restart, check the cache
// and purge a game from it.
//
// Reading is open, like /metrics. Changing settings or the cache needs
// DASHBOARD_PASSWORD, sent as a bearer token; without one both are
// read-only. A custom Authorization header cannot be sent cross-origin
// without a CORS preflight, which is never granted, so another site cannot
// make a browser change them.
package dashboard

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"cacheparty/internal/config"
	"cacheparty/internal/evict"
	"cacheparty/internal/store"
)

//go:embed static
var static embed.FS

// Stats are cumulative counters shown as totals since start.
type Stats struct {
	Requests         map[string]int64 `json:"requests"` // by cache status
	UpstreamRequests int64            `json:"upstreamRequests"`
	UpstreamErrors   int64            `json:"upstreamErrors"`
	Coalesced        int64            `json:"coalesced"`
	EvictedBytes     int64            `json:"evictedBytes"`
	SNISent          int64            `json:"sniSent"`
	SNIReceived      int64            `json:"sniReceived"`
	DiskRead         int64            `json:"diskRead"`    // chunk bytes read to serve hits
	DiskWritten      int64            `json:"diskWritten"` // chunk bytes written
}

// Config carries what the dashboard reads and controls.
type Config struct {
	Hostname string
	Version  string // of the running build
	Traffic  *Traffic
	Stats    func() Stats
	Cache    func() (chunks, bytes int64)
	Active   func() int64 // requests being served on :80

	// StatsFile is where the per-service, per-game, per-client and
	// per-host totals are kept between restarts; "" keeps them in memory
	// only.
	StatsFile string

	Settings *config.Config       // as loaded at startup
	Override *config.Overrides    // where changes are saved
	Apply    func(*config.Config) // pushes changed Live settings to the running cache
	Password string               // "" makes settings read-only

	// Verify checks the cache against its index (see store.FileStore.Scrub).
	Verify func(ctx context.Context, dryRun bool) (store.ScrubResult, error)
	// Purge removes every cached chunk of objects tagged app (see
	// evict.Evictor.Purge), or with dryRun only counts them.
	Purge func(app string, dryRun bool) evict.PurgeResult

	Log *slog.Logger
}

// Dashboard is the page, its API and the sampler behind its charts.
type Dashboard struct {
	cfg     Config
	started time.Time
	hist    *history
	current atomic.Pointer[config.Config]
	failMu  sync.Mutex // serializes failed logins, to slow guessing
	mux     *http.ServeMux
}

// New returns a Dashboard. Call Run to start sampling.
func New(cfg Config) *Dashboard {
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	d := &Dashboard{cfg: cfg, started: time.Now(), hist: newHistory(), mux: http.NewServeMux()}
	d.current.Store(cfg.Settings)
	if cfg.StatsFile != "" {
		seen, err := cfg.Traffic.load(cfg.StatsFile)
		if err != nil {
			cfg.Log.Warn("ignoring saved traffic totals", "file", cfg.StatsFile, "err", err)
		}
		for k, at := range seen {
			d.hist.seen[k] = at
		}
	}
	files, _ := fs.Sub(static, "static")
	d.mux.Handle("GET /", http.FileServerFS(files))
	d.mux.HandleFunc("GET /api/stats", d.serveStats)
	d.mux.HandleFunc("GET /api/services/{name}", d.serveApps)
	d.mux.HandleFunc("GET /api/settings", d.serveSettings)
	d.mux.HandleFunc("PUT /api/settings", d.updateSettings)
	d.mux.HandleFunc("POST /api/cache/verify", d.verifyCache)
	d.mux.HandleFunc("POST /api/cache/purge", d.purgeApp)
	return d
}

func (d *Dashboard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	d.mux.ServeHTTP(w, r)
}

// Run samples once a second until ctx ends.
func (d *Dashboard) Run(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	nextSave := time.Now().Add(saveEvery)
	for {
		now := time.Now()
		d.sample(now)
		if now.After(nextSave) {
			d.SaveStats()
			nextSave = now.Add(saveEvery)
		}
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		}
	}
}

// SaveStats writes the traffic totals to Config.StatsFile, if set. Run does
// so every minute; call it once more at shutdown.
func (d *Dashboard) SaveStats() {
	if d.cfg.StatsFile == "" {
		return
	}
	if err := d.cfg.Traffic.save(d.cfg.StatsFile, d.hist.lastActive); err != nil {
		d.cfg.Log.Warn("save traffic totals", "file", d.cfg.StatsFile, "err", err)
	}
}

func (d *Dashboard) sample(now time.Time) {
	svc := d.cfg.Traffic.snapshot()
	var p point
	for k, v := range svc {
		if k.isService() {
			p.Sent += v[0]
			p.Fetched += v[1]
		}
	}
	st := d.cfg.Stats()
	p.SNI = st.SNISent + st.SNIReceived
	p.DiskRead, p.DiskWrite = st.DiskRead, st.DiskWritten
	d.hist.sample(now, p, svc)
}

type serviceJSON struct {
	Name        string  `json:"name"`
	SentRate    float64 `json:"sentRate"`
	FetchedRate float64 `json:"fetchedRate"`
	Sent        int64   `json:"sent"`
	Fetched     int64   `json:"fetched"`
}

// peerJSON is the traffic of one upstream host: bytes fetched from it.
type peerJSON struct {
	Name  string  `json:"name"`
	Rate  float64 `json:"rate"`
	Bytes int64   `json:"bytes"`
}

func sortServices(l []serviceJSON) {
	sort.Slice(l, func(i, j int) bool {
		a, b := l[i], l[j]
		if a.SentRate != b.SentRate {
			return a.SentRate > b.SentRate
		}
		if a.Sent != b.Sent {
			return a.Sent > b.Sent
		}
		return a.Name < b.Name
	})
}

// httpsJSON is the traffic relayed for one server name on the HTTPS
// passthrough. Rate counts both directions, as the passthrough chart does.
type httpsJSON struct {
	Name     string  `json:"name"`
	Rate     float64 `json:"rate"`
	Sent     int64   `json:"sent"`     // to clients
	Received int64   `json:"received"` // from clients
}

func sortPeers(l []peerJSON) {
	sort.Slice(l, func(i, j int) bool {
		a, b := l[i], l[j]
		if a.Rate != b.Rate {
			return a.Rate > b.Rate
		}
		if a.Bytes != b.Bytes {
			return a.Bytes > b.Bytes
		}
		return a.Name < b.Name
	})
}

type statsJSON struct {
	Hostname  string        `json:"hostname"`
	Version   string        `json:"version"`
	Uptime    float64       `json:"uptime"`
	Step      int           `json:"step"` // seconds per point
	End       int64         `json:"end"`  // unix ms when the newest point ends
	Sent      []float64     `json:"sent"` // bytes/s to clients
	Fetched   []float64     `json:"fetched"`
	SNI       []float64     `json:"sni"`
	DiskRead  []float64     `json:"diskRead"` // bytes/s read from chunk files
	DiskWrite []float64     `json:"diskWrite"`
	SNIOn     bool          `json:"sniEnabled"`
	Saved     bool          `json:"saved"` // traffic totals are kept across restarts
	Services  []serviceJSON `json:"services"`
	Clients   []serviceJSON `json:"clients"`   // traffic of each client IP
	Upstreams []peerJSON    `json:"upstreams"` // bytes fetched from each upstream host
	HTTPS     []httpsJSON   `json:"https"`     // bytes relayed for each HTTPS server name
	Active    int64         `json:"active"`
	Totals    Stats         `json:"totals"`
	IPv4      []addrJSON    `json:"ipv4"`
	IPv6      []addrJSON    `json:"ipv6"`
	Cache     struct {
		Chunks    int64 `json:"chunks"`
		Bytes     int64 `json:"bytes"`
		Limit     int64 `json:"limit"`
		MinFree   int64 `json:"minFree"`
		DiskTotal int64 `json:"diskTotal"` // 0 if unknown
		DiskFree  int64 `json:"diskFree"`
	} `json:"cache"`
}

func (d *Dashboard) serveStats(w http.ResponseWriter, r *http.Request) {
	coarse := r.URL.Query().Get("range") == "24h"
	pts, end := d.hist.series(coarse)
	step := 1
	if coarse {
		step = coarseEvery
	}
	out := statsJSON{
		Hostname:  d.cfg.Hostname,
		Version:   d.cfg.Version,
		Uptime:    time.Since(d.started).Seconds(),
		Step:      step,
		End:       end.UnixMilli(),
		Sent:      make([]float64, len(pts)),
		Fetched:   make([]float64, len(pts)),
		SNI:       make([]float64, len(pts)),
		DiskRead:  make([]float64, len(pts)),
		DiskWrite: make([]float64, len(pts)),
		SNIOn:     d.cfg.Settings.SNIAddr != "",
		Saved:     d.cfg.StatsFile != "",
		Active:    d.cfg.Active(),
		Totals:    d.cfg.Stats(),
		Services:  []serviceJSON{},
		Clients:   []serviceJSON{},
		Upstreams: []peerJSON{},
		HTTPS:     []httpsJSON{},
	}
	out.IPv4, out.IPv6 = addresses()
	for i, p := range pts {
		out.Sent[i] = float64(p.Sent) / float64(step)
		out.Fetched[i] = float64(p.Fetched) / float64(step)
		out.SNI[i] = float64(p.SNI) / float64(step)
		out.DiskRead[i] = float64(p.DiskRead) / float64(step)
		out.DiskWrite[i] = float64(p.DiskWrite) / float64(step)
	}
	rates := d.hist.serviceRates()
	for k, v := range d.cfg.Traffic.snapshot() {
		switch {
		case k.isService():
			rt := rates[k]
			out.Services = append(out.Services, serviceJSON{k.Service, rt[0], rt[1], v[0], v[1]})
		case k.Group == groupClient:
			rt := rates[k]
			out.Clients = append(out.Clients, serviceJSON{k.Name, rt[0], rt[1], v[0], v[1]})
		case k.Group == groupUpstream:
			out.Upstreams = append(out.Upstreams, peerJSON{k.Name, rates[k][1], v[1]})
		case k.Group == groupHTTPS:
			rt := rates[k]
			out.HTTPS = append(out.HTTPS, httpsJSON{k.Name, rt[0] + rt[1], v[0], v[1]})
		}
	}
	sortServices(out.Clients)
	sortPeers(out.Upstreams)
	sort.Slice(out.HTTPS, func(i, j int) bool {
		a, b := out.HTTPS[i], out.HTTPS[j]
		if a.Rate != b.Rate {
			return a.Rate > b.Rate
		}
		if ta, tb := a.Sent+a.Received, b.Sent+b.Received; ta != tb {
			return ta > tb
		}
		return a.Name < b.Name
	})
	sortServices(out.Services)

	cfg := d.current.Load()
	out.Cache.Chunks, out.Cache.Bytes = d.cfg.Cache()
	out.Cache.Limit, out.Cache.MinFree = cfg.CacheDiskSize, cfg.MinFreeDisk
	var fs syscall.Statfs_t
	if syscall.Statfs(cfg.CacheRoot, &fs) == nil {
		out.Cache.DiskTotal = int64(fs.Blocks) * int64(fs.Bsize)
		out.Cache.DiskFree = int64(fs.Bavail) * int64(fs.Bsize)
	}
	writeJSON(w, http.StatusOK, out)
}

type appJSON struct {
	Kind        string  `json:"kind"` // depot, product, app, host or other
	ID          string  `json:"id"`
	SentRate    float64 `json:"sentRate"`
	FetchedRate float64 `json:"fetchedRate"`
	Sent        int64   `json:"sent"`
	Fetched     int64   `json:"fetched"`
	LastActive  int64   `json:"lastActive"` // unix ms; 0 if not seen moving
}

type appsJSON struct {
	Service string    `json:"service"`
	Apps    []appJSON `json:"apps"`
}

// serveApps lists the games downloaded through one service in total.
func (d *Dashboard) serveApps(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	out := appsJSON{Service: name, Apps: []appJSON{}}
	rates := d.hist.serviceRates()
	for k, v := range d.cfg.Traffic.snapshot() {
		if k.Service != name || k.Group != "" || k.App == (app{}) {
			continue
		}
		rt := rates[k]
		a := appJSON{Kind: k.App.Kind, ID: k.App.ID, SentRate: rt[0], FetchedRate: rt[1], Sent: v[0], Fetched: v[1]}
		if t := d.hist.lastActive(k); !t.IsZero() {
			a.LastActive = t.UnixMilli()
		}
		out.Apps = append(out.Apps, a)
	}
	sort.Slice(out.Apps, func(i, j int) bool {
		a, b := out.Apps[i], out.Apps[j]
		if a.SentRate != b.SentRate {
			return a.SentRate > b.SentRate
		}
		if a.Sent != b.Sent {
			return a.Sent > b.Sent
		}
		return a.ID < b.ID
	})
	writeJSON(w, http.StatusOK, out)
}

type addrJSON struct {
	Interface string `json:"interface"`
	Address   string `json:"address"`
}

// addresses lists the server's IPv4 and IPv6 addresses on interfaces that
// are up. Read on every request, so a DHCP or SLAAC change shows up.
func addresses() (v4, v6 []addrJSON) {
	v4, v6 = []addrJSON{}, []addrJSON{}
	ifaces, err := net.Interfaces()
	if err != nil {
		return v4, v6
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		v4, v6 = appendAddrs(v4, v6, ifc.Name, addrs)
	}
	return v4, v6
}

// appendAddrs adds the addresses clients could use: loopback, link-local
// and unspecified addresses are left out.
func appendAddrs(v4, v6 []addrJSON, iface string, addrs []net.Addr) ([]addrJSON, []addrJSON) {
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || !ipn.IP.IsGlobalUnicast() {
			continue
		}
		if ip4 := ipn.IP.To4(); ip4 != nil {
			v4 = append(v4, addrJSON{iface, ip4.String()})
		} else {
			v6 = append(v6, addrJSON{iface, ipn.IP.String()})
		}
	}
	return v4, v6
}

// settingInfo describes the variables the dashboard shows, in display
// order. DASHBOARD_PASSWORD is deliberately absent.
var settingInfo = []struct{ Key, Label, Help string }{
	{"CACHE_DISK_SIZE", "Cache size limit", "Least recently used chunks are evicted above this. Sizes take k, m, g or t."},
	{"MIN_FREE_DISK", "Minimum free disk", "Chunks are evicted to keep at least this much free on the volume."},
	{"CACHE_MAX_AGE", "Maximum age", "Chunks not read for this long are evicted, e.g. 90d, 12w, 1y."},
	{"CACHE_MAX_PREFETCHES", "Read-ahead fetches", "Chunk fetches allowed to run ahead of clients, across all clients. 0 disables read-ahead."},
	{"UPSTREAM_RATE_LIMIT", "Download speed limit", "Combined cap on downloads from the internet: cache misses, bypassed requests and HTTPS passthrough. Bits per second with k, m or g, e.g. 500m. Cache hits are not limited. 0 removes the limit."},
	{"BYPASS_DOMAINS", "Bypassed domains", "Comma-separated domains whose traffic skips the cache and goes straight to the origin, e.g. example.com, *.cdn.example.net. example.com also covers its subdomains; *.example.com covers subdomains only. Empty bypasses nothing."},
	{"CACHE_SLICE_SIZE", "Slice size", "Changing it starts a fresh cache."},
	{"UPSTREAM_DNS", "Upstream DNS", ""},
	{"CACHE_DOMAINS_REPO", "cache-domains repository", ""},
	{"CACHE_DOMAINS_BRANCH", "cache-domains branch", ""},
	{"NOFETCH", "Skip cache-domains update", ""},
	{"NGINX_LOG_FORMAT", "Access log format", ""},
	{"CACHE_ROOT", "Cache directory", ""},
	{"LOG_DIR", "Log directory", ""},
	{"DOMAINS_DIR", "cache-domains directory", ""},
	{"HTTP_ADDR", "HTTP listener", ""},
	{"SNI_ADDR", "HTTPS passthrough listener", ""},
	{"METRICS_ADDR", "Metrics and dashboard listener", ""},
	{"SETTINGS_FILE", "Settings file", ""},
	{"STATS_FILE", "Traffic totals file", "Where the dashboard's totals are kept across restarts; off keeps them in memory only."},
}

type settingJSON struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Help   string `json:"help"`
	Value  string `json:"value"`
	Source string `json:"source"` // dashboard, env or default
	Live   bool   `json:"live"`
}

type settingsJSON struct {
	Editable bool          `json:"editable"`
	Settings []settingJSON `json:"settings"`
}

func (d *Dashboard) settings() settingsJSON {
	out := settingsJSON{Editable: d.cfg.Password != ""}
	o := d.cfg.Override
	for _, s := range settingInfo {
		out.Settings = append(out.Settings, settingJSON{
			Key: s.Key, Label: s.Label, Help: s.Help,
			Value:  config.Raw(o.Getenv, s.Key),
			Source: o.Source(s.Key),
			Live:   slices.Contains(config.Live, s.Key),
		})
	}
	return out
}

func (d *Dashboard) serveSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.settings())
}

// updateSettings takes {"values": {"KEY": "value" | null}}; null drops the
// dashboard's value so the environment (or default) applies again.
func (d *Dashboard) updateSettings(w http.ResponseWriter, r *http.Request) {
	if !d.mayChange(w, r) {
		return
	}
	var body struct {
		Values map[string]*string `json:"values"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	for k, v := range body.Values {
		if v != nil {
			s := strings.TrimSpace(*v)
			body.Values[k] = &s
		}
	}
	cfg, err := d.cfg.Override.Set(body.Values)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.current.Store(cfg)
	d.cfg.Apply(cfg)
	changed := make([]any, 0, 2*len(body.Values)+2)
	changed = append(changed, "remote", r.RemoteAddr)
	for k, v := range body.Values {
		if v == nil {
			changed = append(changed, k, "(reset)")
		} else {
			changed = append(changed, k, *v)
		}
	}
	d.cfg.Log.Info("settings changed from dashboard", changed...)
	writeJSON(w, http.StatusOK, d.settings())
}

// mayChange checks a request that changes settings or the cache, answering
// it if it may not.
func (d *Dashboard) mayChange(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case d.cfg.Password == "":
		writeError(w, http.StatusForbidden, "read-only: set DASHBOARD_PASSWORD to allow changes")
	case !sameOrigin(r):
		writeError(w, http.StatusForbidden, "cross-origin request refused")
	case !d.authorized(r):
		writeError(w, http.StatusUnauthorized, "wrong password")
	default:
		return true
	}
	return false
}

func (d *Dashboard) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if ok && subtle.ConstantTimeCompare([]byte(token), []byte(d.cfg.Password)) == 1 {
		return true
	}
	// One failure at a time, each taking a second, bounds guessing to one
	// password a second however many requests are in flight.
	d.failMu.Lock()
	time.Sleep(time.Second)
	d.failMu.Unlock()
	return false
}

// sameOrigin rejects a request whose Origin header names another host.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not from a browser page, or same-origin in older browsers
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
