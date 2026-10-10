// Command cacheparty is a LAN game-download cache, a drop-in replacement for
// lancachenet/monolithic.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	"cacheparty/internal/accesslog"
	"cacheparty/internal/config"
	"cacheparty/internal/dashboard"
	"cacheparty/internal/domains"
	"cacheparty/internal/evict"
	"cacheparty/internal/metrics"
	"cacheparty/internal/proxy"
	"cacheparty/internal/ratelimit"
	"cacheparty/internal/resolver"
	"cacheparty/internal/sni"
	"cacheparty/internal/store"
	"cacheparty/internal/upstream"
)

// version is set at build time with -ldflags "-X main.version=...";
// see buildVersion.
var version string

// buildVersion returns version, or for a build without it the commit Go
// recorded (with "-dirty" for uncommitted changes), or "dev".
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if rev == "" {
		return "dev"
	}
	return rev[:min(7, len(rev))] + dirty
}

// commands are the subcommands; without one, CacheParty serves.
var commands = map[string]func(args []string) error{
	"healthcheck": func([]string) error { return healthcheck() },
	"verify":      func(args []string) error { return verifyCmd(args, os.Stdout) },
	"purge":       func(args []string) error { return purgeCmd(args, os.Stdin, os.Stdout) },
}

func main() {
	if len(os.Args) > 1 {
		if cmd := commands[os.Args[1]]; cmd != nil {
			if err := cmd(os.Args[2:]); err != nil {
				if !errors.Is(err, flag.ErrHelp) {
					fmt.Fprintf(os.Stderr, "%s: %v\n", os.Args[1], err)
				}
				os.Exit(1)
			}
			return
		}
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "cacheparty:", err)
		os.Exit(1)
	}
}

func run() error {
	// Values saved from the dashboard override the environment.
	overrides, warnings, err := config.LoadOverrides(config.Raw(os.Getenv, "SETTINGS_FILE"), os.Getenv)
	if err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	cfg, err := config.Load(overrides.Getenv)
	if err != nil {
		return err
	}

	errLog, err := accesslog.Open(filepath.Join(cfg.LogDir, "error.log"), 0)
	if err != nil {
		return fmt.Errorf("open error.log: %w", err)
	}
	defer errLog.Close()
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, errLog), nil))
	slog.SetDefault(log)
	for _, w := range warnings {
		log.Warn(w)
	}

	accessW, err := accesslog.Open(filepath.Join(cfg.LogDir, "access.log"), 0)
	if err != nil {
		return fmt.Errorf("open access.log: %w", err)
	}
	defer accessW.Close()

	res, err := resolver.New(cfg.UpstreamDNS, resolver.Options{})
	if err != nil {
		return err
	}
	// Every upstream connection is dialed here, so one limiter covers cache
	// misses, bypassed requests and the :443 passthrough alike.
	limiter := ratelimit.New(cfg.RateLimit)
	dialer := limitedResolver{res, limiter}
	up := upstream.New(dialer, upstream.Options{})

	matcher, err := loadDomains(cfg, up, log)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(cfg.CacheRoot, 0o755); err != nil {
		return fmt.Errorf("create cache root: %w", err)
	}
	reg := metrics.NewRegistry()
	m := newMetrics(reg)
	chunks, err := store.Open(cfg.CacheRoot, store.Options{Log: log, BytesWritten: m.cacheWritten})
	if err != nil {
		return fmt.Errorf("open cache store in %s: %w", cfg.CacheRoot, err)
	}
	defer func() {
		if err := chunks.Close(); err != nil {
			log.Error("close cache store", "err", err)
		}
	}()

	hostname, err := os.Hostname()
	if err != nil {
		return err
	}

	reg.Gauge("cacheparty_cache_chunks", "Chunks currently cached.", func() float64 { n, _ := chunks.Usage(); return float64(n) })
	reg.Gauge("cacheparty_cache_bytes", "Bytes currently cached.", func() float64 { _, b := chunks.Usage(); return float64(b) })
	traffic := dashboard.NewTraffic()
	handler := proxy.New(proxy.Config{
		Hostname: hostname,
		Version:  buildVersion(),
		Matcher:  matcher,
		Upstream: up,
		Access:   accesslog.NewLogger(accessW, cfg.LogFormat),
		Log:      log,

		Store:         chunks,
		SliceSize:     cfg.SliceSize,
		MaxPrefetches: proxyPrefetches(cfg.MaxPrefetches),

		Requests:         m.requests,
		BytesSent:        m.bytesSent,
		UpstreamRequests: m.upstreamRequests,
		UpstreamErrors:   m.upstreamErrors,
		Coalesced:        m.coalesced,
		CacheRead:        m.cacheRead,
		DroppedChunks:    m.droppedChunks,
		Flows:            traffic.Flow,
		App:              dashboard.AppTag,
		DashboardPort:    addrPort(cfg.MetricsAddr),
	})
	handler.SetBypassDomains(cfg.BypassDomains)
	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: game downloads legitimately run for hours.
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	evictor := evict.New(evict.Config{
		Store:     chunks,
		SliceSize: cfg.SliceSize,
		Dir:       cfg.CacheRoot,
		MaxBytes:  cfg.CacheDiskSize,
		MinFree:   cfg.MinFreeDisk,
		MaxAge:    cfg.CacheMaxAge,
		Log:       log,
		OnSweep: func(r evict.Result) {
			m.evictedChunks.Add(int64(r.Expired + r.Evicted))
			m.evictedBytes.Add(r.Freed)
		},
	})
	bg := make(chan struct{})
	go func() { defer close(bg); evictor.Run(ctx) }()
	go scrubLoop(ctx, chunks, log)
	go reopenOnHUP(ctx, log, accessW, errLog)
	errc := make(chan error, 3)
	go func() { errc <- srv.ListenAndServe() }()

	var sniSrv *sni.Server
	if cfg.SNIAddr != "" {
		streamW, err := accesslog.Open(filepath.Join(cfg.LogDir, "stream-access.log"), 0)
		if err != nil {
			return fmt.Errorf("open stream-access.log: %w", err)
		}
		defer streamW.Close()
		go reopenOnHUP(ctx, log, streamW)
		ln, err := net.Listen("tcp", cfg.SNIAddr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", cfg.SNIAddr, err)
		}
		sniSrv = sni.New(sni.Config{
			Resolver:      dialer,
			Log:           log,
			Access:        streamW,
			Conns:         m.sniConns,
			BytesSent:     m.sniSent,
			BytesReceived: m.sniReceived,
			Domain:        traffic.HTTPS,
		})
		go func() { errc <- sniSrv.Serve(ln) }()
	}

	var metricsSrv *http.Server
	var dash *dashboard.Dashboard
	if cfg.MetricsAddr != "" {
		mux := http.NewServeMux()
		dash = dashboard.New(dashboard.Config{
			Hostname:  hostname,
			Version:   buildVersion(),
			Traffic:   traffic,
			StatsFile: cfg.StatsFile,
			Stats:     m.dashboardStats,
			Cache:     chunks.Usage,
			Active:    handler.Active,
			Settings:  cfg,
			Override:  overrides,
			Apply: func(c *config.Config) {
				evictor.SetLimits(evict.Limits{MaxBytes: c.CacheDiskSize, MinFree: c.MinFreeDisk, MaxAge: c.CacheMaxAge})
				handler.SetMaxPrefetches(proxyPrefetches(c.MaxPrefetches))
				limiter.SetRate(c.RateLimit)
				handler.SetBypassDomains(c.BypassDomains)
			},
			Password: cfg.DashboardPassword,
			Verify: func(ctx context.Context, dryRun bool) (store.ScrubResult, error) {
				return chunks.Scrub(ctx, store.ScrubOptions{MinAge: time.Hour, DryRun: dryRun})
			},
			Purge: evictor.Purge,
			Log:   log,
		})
		go dash.Run(ctx)
		mux.Handle("/", dash)
		mux.Handle("/metrics", reg.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			io.WriteString(w, "ok\n")
		})
		metricsSrv = &http.Server{Addr: cfg.MetricsAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		go func() { errc <- metricsSrv.ListenAndServe() }()
	}
	n, size := chunks.Usage()
	log.Info("listening", "version", buildVersion(), "addr", cfg.HTTPAddr, "cached_chunks", n, "cached_bytes", size, "domains", matcher.Len(), "hostname", hostname,
		"sni_addr", cfg.SNIAddr, "metrics_addr", cfg.MetricsAddr)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	stop()
	<-bg
	shutCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if metricsSrv != nil {
		_ = metricsSrv.Shutdown(shutCtx)
	}
	if sniSrv != nil {
		_ = sniSrv.Shutdown(shutCtx)
	}
	if err := srv.Shutdown(shutCtx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// Chunk fills outlive the requests that start them; let them land
	// before the deferred store Close.
	if err := handler.Wait(shutCtx); err != nil {
		log.Warn("chunk fills still running at shutdown", "err", err)
	}
	if dash != nil {
		dash.SaveStats()
	}
	return nil
}

// appMetrics are the counters handed to the components that update them.
type appMetrics struct {
	requests, bytesSent                         *metrics.Vec
	upstreamRequests, upstreamErrors, coalesced *metrics.Counter
	droppedChunks                               *metrics.Counter
	evictedChunks, evictedBytes                 *metrics.Counter
	sniConns                                    *metrics.Vec
	sniSent, sniReceived                        *metrics.Counter
	cacheRead, cacheWritten                     *metrics.Counter
}

func newMetrics(r *metrics.Registry) *appMetrics {
	m := &appMetrics{
		requests:         r.CounterVec("cacheparty_http_requests_total", "HTTP requests handled on :80, by cache status.", "cache_status"),
		bytesSent:        r.CounterVec("cacheparty_http_response_bytes_total", "Response body bytes sent to clients, by cache status.", "cache_status"),
		upstreamRequests: r.Counter("cacheparty_upstream_requests_total", "Chunk fetches sent to origin servers."),
		upstreamErrors:   r.Counter("cacheparty_upstream_errors_total", "Chunk fetches that got no response from the origin."),
		coalesced:        r.Counter("cacheparty_coalesced_requests_total", "Chunk requests that shared an in-progress upstream fetch."),
		droppedChunks:    r.Counter("cacheparty_dropped_chunks_total", "Cached chunks dropped and fetched again because their file could not be read."),
		evictedChunks:    r.Counter("cacheparty_evicted_chunks_total", "Chunks removed by the evictor (age or size)."),
		evictedBytes:     r.Counter("cacheparty_evicted_bytes_total", "Bytes freed by the evictor."),
		sniConns:         r.CounterVec("cacheparty_sni_connections_total", "Connections on :443, by result.", "result"),
		sniSent:          r.Counter("cacheparty_sni_bytes_sent_total", "Bytes relayed to :443 clients."),
		sniReceived:      r.Counter("cacheparty_sni_bytes_received_total", "Bytes relayed from :443 clients."),
		cacheRead:        r.Counter("cacheparty_cache_read_bytes_total", "Chunk bytes read from disk to serve hits."),
		cacheWritten:     r.Counter("cacheparty_cache_written_bytes_total", "Chunk bytes written to disk."),
	}
	return m
}

// limitedResolver dials through the resolver and counts what each
// connection downloads against the shared rate limit.
type limitedResolver struct {
	*resolver.Resolver
	lim *ratelimit.Limiter
}

func (r limitedResolver) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c, err := r.Resolver.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	return r.lim.Conn(c), nil
}

// proxyPrefetches maps CACHE_MAX_PREFETCHES to proxy.Config.MaxPrefetches,
// which reads 0 as "default" rather than "disabled".
func proxyPrefetches(n int) int {
	if n == 0 {
		return -1
	}
	return n
}

func (m *appMetrics) dashboardStats() dashboard.Stats {
	reqs := map[string]int64{}
	for _, s := range []string{"HIT", "MISS", "BYPASS"} {
		reqs[s] = m.requests.With(s).Value()
	}
	return dashboard.Stats{
		Requests:         reqs,
		UpstreamRequests: m.upstreamRequests.Value(),
		UpstreamErrors:   m.upstreamErrors.Value(),
		Coalesced:        m.coalesced.Value(),
		EvictedBytes:     m.evictedBytes.Value(),
		SNISent:          m.sniSent.Value(),
		SNIReceived:      m.sniReceived.Value(),
		DiskRead:         m.cacheRead.Value(),
		DiskWritten:      m.cacheWritten.Value(),
	}
}

// loadDomains refreshes cache-domains (unless NOFETCH) and builds the
// matcher. A failed refresh falls back to the copy already on disk; having
// no copy at all is fatal, because identifiers feed cache keys and silently
// running without the list would split the cache.
func loadDomains(cfg *config.Config, up *upstream.Client, log *slog.Logger) (*domains.Matcher, error) {
	if !cfg.NoFetch {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		err := domains.Fetch(ctx, up.HTTPClient(), cfg.DomainsRepo, cfg.DomainsBranch, cfg.DomainsDir)
		cancel()
		if err != nil {
			log.Warn("cache-domains update failed; using existing copy if present", "err", err)
		}
	}
	m, skipped, err := domains.LoadDir(cfg.DomainsDir)
	if err != nil {
		return nil, fmt.Errorf("load cache-domains from %s: %w", cfg.DomainsDir, err)
	}
	for _, s := range skipped {
		log.Warn("skipped cache-domains entry", "entry", s)
	}
	return m, nil
}

// scrubLoop reconciles the cache index with the files on disk: shortly after
// start (an unclean exit is when it matters most) and then daily.
func scrubLoop(ctx context.Context, st *store.FileStore, log *slog.Logger) {
	delay := 5 * time.Minute
	for {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return
		}
		delay = 24 * time.Hour
		res, err := st.Scrub(ctx, store.ScrubOptions{MinAge: time.Hour, Pause: 20 * time.Millisecond})
		switch {
		case ctx.Err() != nil:
			return
		case errors.Is(err, store.ErrScrubRunning):
			log.Info("cache scrub skipped: a check started from the dashboard is running")
		case err != nil:
			log.Error("cache scrub", "err", err)
		default:
			log.Info("cache scrub", "files", res.Files, "orphan_files", res.Orphans, "missing_files", res.Missing,
				"corrupt_files", res.Corrupt, "freed_bytes", res.Freed)
		}
	}
}

// reopenOnHUP reopens the log files on SIGHUP, for log rotation.
func reopenOnHUP(ctx context.Context, log *slog.Logger, ws ...*accesslog.Writer) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGHUP)
	defer signal.Stop(c)
	for {
		select {
		case <-c:
			for _, w := range ws {
				if err := w.Reopen(); err != nil {
					log.Error("reopen log", "err", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// healthcheck requests /lancache-heartbeat from the local proxy listener. It
// exists because the distroless image has no curl for a Docker HEALTHCHECK.
func healthcheck() error {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":80"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort("127.0.0.1", port) + "/lancache-heartbeat")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// addrPort returns the port of a listen address such as ":8080", or "" if
// it has none (including the empty, disabled address).
func addrPort(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return port
}
