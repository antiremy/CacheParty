// Package dnssync points the cache-domains names at this cache on a LAN DNS
// server: Pi-hole (v6), AdGuard Home or Technitium DNS Server. It is an
// alternative to running lancache-dns.
//
// Each sync reads what the server holds, adds the records that are missing
// and removes the ones this package added earlier that are no longer wanted
// (a domain left the list, a service was excluded or an address changed).
// Records it did not add are never touched, so a hand-made entry for the
// same name survives. What each server was given is remembered in the sync
// file next to the server's settings.
package dnssync

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"cacheparty/internal/domains"
)

// Server kinds.
const (
	PiHole     = "pihole"
	AdGuard    = "adguard"
	Technitium = "technitium"
)

// Kinds lists the supported server kinds.
var Kinds = []string{PiHole, AdGuard, Technitium}

// autoEvery is how often servers with Auto set are synced after the one at
// start, which repairs records removed by hand.
const autoEvery = 6 * time.Hour

var (
	ErrNotFound = errors.New("no such DNS server")
	ErrBusy     = errors.New("a sync is already running")
)

// Result is the outcome of one sync.
type Result struct {
	Time      time.Time `json:"time"`
	DryRun    bool      `json:"dryRun"`
	Added     int       `json:"added"`   // or, on a dry run, would be added
	Removed   int       `json:"removed"` // likewise
	Unchanged int       `json:"unchanged"`
	Error     string    `json:"error,omitempty"`
}

// target is one DNS server as kept in the sync file.
type target struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	URL      string   `json:"url"`
	Username string   `json:"username,omitempty"` // AdGuard Home only
	Secret   string   `json:"secret,omitempty"`   // password or API token
	Insecure bool     `json:"insecure,omitempty"` // skip TLS certificate checks
	Auto     bool     `json:"auto,omitempty"`
	Managed  []string `json:"managed,omitempty"` // record keys this package added
	Last     *Result  `json:"last,omitempty"`
}

type file struct {
	IPs     []string  `json:"ips"`     // addresses the names point at
	Exclude []string  `json:"exclude"` // services left out
	Targets []*target `json:"targets"`
}

// TargetInput creates or changes a server. An empty ID creates one; an
// empty Secret on a change keeps the saved one.
type TargetInput struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Secret   string `json:"secret"`
	Insecure bool   `json:"insecure"`
	Auto     bool   `json:"auto"`
}

// TargetInfo is a server as shown on the dashboard, without its secret.
type TargetInfo struct {
	ID        string  `json:"id"`
	Kind      string  `json:"kind"`
	URL       string  `json:"url"`
	Username  string  `json:"username"`
	HasSecret bool    `json:"hasSecret"`
	Insecure  bool    `json:"insecure"`
	Auto      bool    `json:"auto"`
	Managed   int     `json:"managed"` // records added by this package
	Last      *Result `json:"last"`
}

// ServiceInfo is one cache-domains list.
type ServiceInfo struct {
	Name     string `json:"name"`
	Domains  int    `json:"domains"`
	Excluded bool   `json:"excluded"`
}

// State is everything the dashboard shows.
type State struct {
	IPs      []string      `json:"ips"`
	Services []ServiceInfo `json:"services"`
	Targets  []TargetInfo  `json:"targets"`
	Running  bool          `json:"running"`
}

// Manager keeps the sync file and runs syncs.
type Manager struct {
	path     string
	patterns []domains.Pattern
	log      *slog.Logger

	mu sync.Mutex // guards f
	f  file

	syncMu sync.Mutex // one sync at a time
}

// Open reads the sync file at path; a missing file means nothing is
// configured yet. patterns are the cache-domains entries to publish.
func Open(path string, patterns []domains.Pattern, log *slog.Logger) (*Manager, error) {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{path: path, patterns: patterns, log: log}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &m.f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// State returns the configuration and the last result of each server.
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := State{IPs: slices.Clone(m.f.IPs), Services: []ServiceInfo{}, Targets: []TargetInfo{}}
	if st.IPs == nil {
		st.IPs = []string{}
	}
	if m.syncMu.TryLock() {
		m.syncMu.Unlock()
	} else {
		st.Running = true
	}
	for _, p := range m.patterns {
		if n := len(st.Services); n > 0 && st.Services[n-1].Name == p.Service {
			st.Services[n-1].Domains++
			continue
		}
		st.Services = append(st.Services, ServiceInfo{Name: p.Service, Domains: 1, Excluded: slices.Contains(m.f.Exclude, p.Service)})
	}
	for _, t := range m.f.Targets {
		st.Targets = append(st.Targets, TargetInfo{
			ID: t.ID, Kind: t.Kind, URL: t.URL, Username: t.Username, HasSecret: t.Secret != "",
			Insecure: t.Insecure, Auto: t.Auto, Managed: len(t.Managed), Last: t.Last,
		})
	}
	return st
}

// SetAddresses sets the addresses the names point at and the services left
// out.
func (m *Manager) SetAddresses(ips, exclude []string) error {
	clean := []string{}
	for _, s := range ips {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" || a.IsUnspecified() || a.IsMulticast() {
			return fmt.Errorf("%q is not a usable IP address", s)
		}
		if a = a.Unmap(); !slices.Contains(clean, a.String()) {
			clean = append(clean, a.String())
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.f
	next.IPs = clean
	next.Exclude = slices.Compact(slices.Sorted(slices.Values(exclude)))
	return m.save(next)
}

// PutTarget creates or changes a server and returns its ID.
func (m *Manager) PutTarget(in TargetInput) (string, error) {
	if !slices.Contains(Kinds, in.Kind) {
		return "", fmt.Errorf("unknown server kind %q", in.Kind)
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", errors.New("the address must be a URL such as http://192.168.1.2")
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/admin")
	u.RawQuery, u.Fragment = "", ""
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.f
	next.Targets = slices.Clone(m.f.Targets)
	var t *target
	if in.ID == "" {
		t = &target{ID: newID()}
		next.Targets = append(next.Targets, t)
	} else {
		i := slices.IndexFunc(next.Targets, func(t *target) bool { return t.ID == in.ID })
		if i < 0 {
			return "", ErrNotFound
		}
		c := *next.Targets[i]
		t = &c
		next.Targets[i] = t
		if t.Kind != in.Kind || t.URL != u.String() {
			// Records added to another server mean nothing on this one.
			t.Managed, t.Last = nil, nil
		}
		if t.Kind != in.Kind {
			t.Secret = ""
		}
	}
	t.Kind, t.URL, t.Username, t.Insecure, t.Auto = in.Kind, u.String(), strings.TrimSpace(in.Username), in.Insecure, in.Auto
	if in.Secret != "" {
		t.Secret = in.Secret
	}
	if t.Kind == Technitium && t.Secret == "" {
		return "", errors.New("Technitium needs an API token")
	}
	return t.ID, m.save(next)
}

// DeleteTarget forgets a server. Records already on it stay; sync it with
// Remove first to take them off.
func (m *Manager) DeleteTarget(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.f
	next.Targets = slices.DeleteFunc(slices.Clone(m.f.Targets), func(t *target) bool { return t.ID == id })
	if len(next.Targets) == len(m.f.Targets) {
		return ErrNotFound
	}
	return m.save(next)
}

// Options change what Sync does.
type Options struct {
	DryRun bool `json:"dryRun"` // only count what would change
	Remove bool `json:"remove"` // take off every record added earlier instead of syncing
}

// Sync brings one server up to date. Failures talking to the server are
// reported in Result.Error; the error is ErrNotFound or ErrBusy.
func (m *Manager) Sync(ctx context.Context, id string, opt Options) (Result, error) {
	if !m.syncMu.TryLock() {
		return Result{}, ErrBusy
	}
	defer m.syncMu.Unlock()

	m.mu.Lock()
	i := slices.IndexFunc(m.f.Targets, func(t *target) bool { return t.ID == id })
	if i < 0 {
		m.mu.Unlock()
		return Result{}, ErrNotFound
	}
	t := *m.f.Targets[i]
	ips, exclude := slices.Clone(m.f.IPs), slices.Clone(m.f.Exclude)
	m.mu.Unlock()

	res := Result{Time: time.Now(), DryRun: opt.DryRun}
	managed := t.Managed
	var addrs []netip.Addr
	for _, s := range ips {
		addrs = append(addrs, netip.MustParseAddr(s))
	}
	var patterns []string
	if !opt.Remove {
		for _, p := range m.patterns {
			if !slices.Contains(exclude, p.Service) {
				patterns = append(patterns, p.Pattern)
			}
		}
	}
	if len(addrs) == 0 && !opt.Remove {
		res.Error = "set the cache's IP addresses first"
	} else {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		b := newBackend(&t)
		res, managed = reconcile(ctx, b, b.keys(patterns, addrs), t.Managed, opt.DryRun)
		b.close(ctx)
		cancel()
	}
	if opt.DryRun {
		return res, nil // nothing changed, so nothing to remember
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	next := m.f
	next.Targets = slices.Clone(m.f.Targets)
	if i := slices.IndexFunc(next.Targets, func(x *target) bool { return x.ID == id }); i >= 0 {
		c := *next.Targets[i]
		c.Managed, c.Last = managed, &res
		next.Targets[i] = &c
		if err := m.save(next); err != nil {
			// Keep what was added in memory at least, so a later sync
			// can still take it off.
			m.f = next
			m.log.Error("save DNS sync file", "file", m.path, "err", err)
		}
	}
	return res, nil
}

// Run syncs every server with Auto set now and then every few hours, until
// ctx ends.
func (m *Manager) Run(ctx context.Context) {
	for {
		m.mu.Lock()
		var ids []string
		for _, t := range m.f.Targets {
			if t.Auto {
				ids = append(ids, t.ID)
			}
		}
		m.mu.Unlock()
		for _, id := range ids {
			res, err := m.Sync(ctx, id, Options{})
			switch {
			case errors.Is(err, ErrBusy):
				// one started from the dashboard; the next round catches up
			case err != nil:
			case res.Error != "":
				m.log.Warn("DNS sync failed", "server", id, "err", res.Error)
			case res.Added+res.Removed > 0:
				m.log.Info("DNS sync", "server", id, "added", res.Added, "removed", res.Removed)
			}
		}
		select {
		case <-time.After(autoEvery):
		case <-ctx.Done():
			return
		}
	}
}

// backend speaks one server's API. Records are identified by keys whose
// format is the backend's own; keys returns the ones wanted.
type backend interface {
	keys(patterns []string, ips []netip.Addr) []string
	// present reports which of keys the server holds now; it may report
	// others as well.
	present(ctx context.Context, keys []string) (map[string]bool, error)
	// apply makes the changes, reporting those done even when it fails
	// part way.
	apply(ctx context.Context, add, remove []string) (added, removed []string, err error)
	close(ctx context.Context)
}

func newBackend(t *target) backend {
	c := newClient(t)
	switch t.Kind {
	case PiHole:
		return &pihole{client: c}
	case AdGuard:
		return &adguard{client: c}
	default:
		return &technitium{client: c}
	}
}

// reconcile diffs want against the server and, unless dryRun, applies the
// difference. It returns the keys now managed: the ones added earlier that
// are still there, plus those added now.
func reconcile(ctx context.Context, b backend, want, managed []string, dryRun bool) (Result, []string) {
	res := Result{Time: time.Now(), DryRun: dryRun}
	have, err := b.present(ctx, slices.Concat(want, managed))
	if err != nil {
		res.Error = err.Error()
		return res, managed
	}
	wanted := map[string]bool{}
	var add, remove []string
	for _, k := range want {
		if wanted[k] {
			continue
		}
		wanted[k] = true
		if have[k] {
			res.Unchanged++
		} else {
			add = append(add, k)
		}
	}
	for _, k := range managed {
		if have[k] && !wanted[k] {
			remove = append(remove, k)
		}
	}
	if dryRun {
		res.Added, res.Removed = len(add), len(remove)
		return res, managed
	}
	added, removed, err := b.apply(ctx, add, remove)
	res.Added, res.Removed = len(added), len(removed)
	if err != nil {
		res.Error = err.Error()
	}
	gone := map[string]bool{}
	for _, k := range removed {
		gone[k] = true
	}
	next := []string{}
	for _, k := range managed {
		if have[k] && !gone[k] {
			next = append(next, k)
		}
	}
	next = append(next, added...)
	sort.Strings(next)
	return res, slices.Compact(next)
}

// client is the HTTP side shared by the backends.
type client struct {
	t    *target
	http *http.Client
}

func newClient(t *target) *client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if t.Insecure {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &client{t: t, http: &http.Client{Transport: tr, Timeout: time.Minute}}
}

// do sends a request to path on the server and decodes a JSON answer into
// out (when not nil). A status other than 2xx is an error carrying msg's
// reading of the body.
func (c *client) do(req *http.Request, out any, msg func([]byte) string) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		detail := ""
		if msg != nil {
			detail = msg(body)
		}
		if detail == "" {
			detail = strings.TrimSpace(string(body))
			if len(detail) > 200 {
				detail = detail[:200] + "…"
			}
		}
		if detail != "" {
			detail = ": " + detail
		}
		return fmt.Errorf("%s %s: %s%s", req.Method, req.URL.Path, resp.Status, detail)
	}
	if out == nil || len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s %s: unexpected answer: %w", req.Method, req.URL.Path, err)
	}
	return nil
}

// jsonRequest makes a request to path on the server with in, if not nil,
// as its JSON body.
func (c *client) jsonRequest(ctx context.Context, method, path string, in any) (*http.Request, error) {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.t.URL+path, body)
	if err == nil && in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, err
}

// save writes f atomically, readable only by the owner because it holds
// the servers' passwords, and makes it current.
func (m *Manager) save(f file) error {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(m.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".dns-sync-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), m.path); err != nil {
		return err
	}
	m.f = f
	return nil
}

func newID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
