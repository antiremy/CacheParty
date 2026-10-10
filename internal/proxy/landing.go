package proxy

import (
	"html/template"
	"net"
	"net/http"
	"net/netip"

	"cacheparty/internal/domains"
)

// forCache reports whether r is addressed to the cache itself rather than to
// a CDN: it has no Host (HTTP/1.0), or its Host is an IP address. Clients
// only reach the cache through lancache-dns, which answers for names, so a
// bare IP means someone typed the server's address into a browser. Proxying
// it would only loop back to us.
func forCache(r *http.Request) bool {
	if r.Host == "" {
		return true
	}
	_, err := netip.ParseAddr(domains.StripPort(r.Host))
	return err == nil
}

var landingPage = template.Must(template.New("landing").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Hostname}} · CacheParty</title>
<style>
:root { color-scheme: light dark; --bg: #f6f7f9; --fg: #1d2330; --muted: #5b6475; --card: #fff; --line: #dde1e8; --accent: #2f6fde; }
@media (prefers-color-scheme: dark) { :root { --bg: #12151b; --fg: #e6e9ef; --muted: #98a1b3; --card: #1a1e26; --line: #2a303b; --accent: #6b9bff; } }
body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--fg); font: 16px/1.5 system-ui, -apple-system, "Segoe UI", sans-serif; }
main { box-sizing: border-box; width: min(32rem, 100% - 2rem); padding: 2rem; background: var(--card); border: 1px solid var(--line); border-radius: 12px; }
h1 { margin: 0 0 .25rem; font-size: 1.4rem; }
p { margin: .75rem 0 0; color: var(--muted); }
code { font-size: .9em; }
a.button { display: inline-block; margin-top: 1.25rem; padding: .55rem 1rem; border-radius: 8px; background: var(--accent); color: #fff; text-decoration: none; font-weight: 600; }
</style>
</head>
<body>
<main>
<h1>CacheParty is running</h1>
<p>This is the LAN cache on <code>{{.Hostname}}</code>. Game downloads reach it through lancache-dns; there is nothing to browse on this port.</p>
{{if .Dashboard}}<a class="button" href="{{.Dashboard}}">Open the dashboard</a>{{end}}
</main>
</body>
</html>
`))

// serveLanding answers a request addressed to the cache itself (see
// forCache). It is not a download, so it is left out of the access log and
// metrics.
func (h *Handler) serveLanding(w http.ResponseWriter, r *http.Request, rec *record) {
	rec.skipLog = true
	if r.URL.Path != "/" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		rec.status = http.StatusNotFound
		return
	}
	data := struct{ Hostname, Dashboard string }{Hostname: h.cfg.Hostname}
	if h.cfg.DashboardPort != "" {
		host := domains.StripPort(r.Host)
		if host == "" {
			// No Host: use the address the client connected to.
			if a, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				host, _, _ = net.SplitHostPort(a.String())
			}
		}
		if host != "" {
			data.Dashboard = "http://" + net.JoinHostPort(host, h.cfg.DashboardPort) + "/"
		}
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/html; charset=utf-8")
	hdr.Set("Cache-Control", "no-store")
	rec.status = http.StatusOK
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if err := landingPage.Execute(w, data); err != nil {
		h.cfg.Log.Debug("landing page write failed", "err", err)
	}
}
