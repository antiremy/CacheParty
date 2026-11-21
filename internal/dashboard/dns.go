package dashboard

import (
	"context"
	"errors"
	"net/http"

	"cacheparty/internal/dnssync"
)

// DNS sync: pointing the cache-domains names at this cache on a Pi-hole,
// AdGuard Home or Technitium server. Reading is open like the rest of the
// dashboard (the servers' passwords are never sent back); changes need
// DASHBOARD_PASSWORD.

type dnsJSON struct {
	Enabled  bool `json:"enabled"` // DNS_SYNC_FILE is set
	Editable bool `json:"editable"`
	dnssync.State
	// Detected are this machine's addresses, to suggest. In a container
	// they are the container's, which clients may not reach.
	Detected struct {
		IPv4 []addrJSON `json:"ipv4"`
		IPv6 []addrJSON `json:"ipv6"`
	} `json:"detected"`
}

func (d *Dashboard) dnsState() dnsJSON {
	out := dnsJSON{Enabled: d.cfg.DNSSync != nil, Editable: d.cfg.Password != ""}
	if d.cfg.DNSSync != nil {
		out.State = d.cfg.DNSSync.State()
	}
	out.Detected.IPv4, out.Detected.IPv6 = addresses()
	return out
}

func (d *Dashboard) serveDNS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, d.dnsState())
}

// dnsChange checks a request that changes DNS sync, answering it if it may
// not go ahead.
func (d *Dashboard) dnsChange(w http.ResponseWriter, r *http.Request) bool {
	if d.cfg.DNSSync == nil {
		writeError(w, http.StatusNotFound, "DNS sync is off: set DNS_SYNC_FILE to enable it")
		return false
	}
	return d.mayChange(w, r)
}

// updateDNS takes {"ips": [...], "exclude": [service, ...]}.
func (d *Dashboard) updateDNS(w http.ResponseWriter, r *http.Request) {
	if !d.dnsChange(w, r) {
		return
	}
	var body struct {
		IPs     []string `json:"ips"`
		Exclude []string `json:"exclude"`
	}
	if !readJSON(w, r, &body) {
		return
	}
	if err := d.cfg.DNSSync.SetAddresses(body.IPs, body.Exclude); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.cfg.Log.Info("DNS sync addresses changed from dashboard", "remote", r.RemoteAddr, "ips", body.IPs, "exclude", body.Exclude)
	writeJSON(w, http.StatusOK, d.dnsState())
}

// putDNSServer takes a dnssync.TargetInput; without an id it adds a server.
func (d *Dashboard) putDNSServer(w http.ResponseWriter, r *http.Request) {
	if !d.dnsChange(w, r) {
		return
	}
	var in dnssync.TargetInput
	if !readJSON(w, r, &in) {
		return
	}
	id, err := d.cfg.DNSSync.PutTarget(in)
	switch {
	case errors.Is(err, dnssync.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	d.cfg.Log.Info("DNS server saved from dashboard", "remote", r.RemoteAddr, "server", id, "kind", in.Kind, "url", in.URL)
	writeJSON(w, http.StatusOK, d.dnsState())
}

func (d *Dashboard) deleteDNSServer(w http.ResponseWriter, r *http.Request) {
	if !d.dnsChange(w, r) {
		return
	}
	id := r.PathValue("id")
	if err := d.cfg.DNSSync.DeleteTarget(id); err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	d.cfg.Log.Info("DNS server removed from dashboard", "remote", r.RemoteAddr, "server", id)
	writeJSON(w, http.StatusOK, d.dnsState())
}

type dnsSyncJSON struct {
	Result dnssync.Result `json:"result"`
	State  dnsJSON        `json:"state"`
}

// syncDNSServer takes {"dryRun": bool, "remove": bool} and answers when the
// sync is done. It runs on if the client goes away, so a server is never
// left half changed.
func (d *Dashboard) syncDNSServer(w http.ResponseWriter, r *http.Request) {
	if !d.dnsChange(w, r) {
		return
	}
	var opt dnssync.Options
	if !readJSON(w, r, &opt) {
		return
	}
	id := r.PathValue("id")
	res, err := d.cfg.DNSSync.Sync(context.WithoutCancel(r.Context()), id, opt)
	switch {
	case errors.Is(err, dnssync.ErrBusy):
		writeError(w, http.StatusConflict, "a DNS sync is already running; try again shortly")
		return
	case err != nil:
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if !opt.DryRun {
		d.cfg.Log.Info("DNS synced from dashboard", "remote", r.RemoteAddr, "server", id, "remove", opt.Remove,
			"added", res.Added, "removed", res.Removed, "err", res.Error)
	}
	writeJSON(w, http.StatusOK, dnsSyncJSON{res, d.dnsState()})
}
