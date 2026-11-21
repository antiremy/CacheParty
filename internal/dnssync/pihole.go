package dnssync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"
)

// pihole speaks the Pi-hole v6 API. Names are dnsmasq lines
// (misc.dnsmasq_lines) of the form address=/domain/ip, which answer for the
// domain and every name under it, so "*.example.com" and "example.com"
// become the same line. Pi-hole restarts its resolver when the lines
// change, which takes a moment.
type pihole struct {
	*client
	sid   string
	lines []string // misc.dnsmasq_lines as last read
}

func (p *pihole) keys(patterns []string, ips []netip.Addr) []string {
	var out []string
	for _, pat := range patterns {
		d := strings.TrimPrefix(pat, "*.")
		for _, ip := range ips {
			out = append(out, "address=/"+d+"/"+ip.String())
		}
	}
	return out
}

func piholeError(b []byte) string {
	var e struct {
		Error struct{ Message, Hint string } `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil || e.Error.Message == "" {
		return ""
	}
	if e.Error.Hint != "" {
		return e.Error.Message + " (" + e.Error.Hint + ")"
	}
	return e.Error.Message
}

func (p *pihole) request(ctx context.Context, method, path string, in, out any) error {
	req, err := p.jsonRequest(ctx, method, path, in)
	if err != nil {
		return err
	}
	if p.sid != "" {
		req.Header.Set("X-FTL-SID", p.sid)
	}
	return p.do(req, out, piholeError)
}

func (p *pihole) login(ctx context.Context) error {
	var out struct {
		Session struct {
			Valid   bool   `json:"valid"`
			SID     string `json:"sid"`
			Message string `json:"message"`
		} `json:"session"`
	}
	if err := p.request(ctx, http.MethodPost, "/api/auth", map[string]string{"password": p.t.Secret}, &out); err != nil {
		return err
	}
	if !out.Session.Valid {
		msg := out.Session.Message
		if msg == "" {
			msg = "wrong password"
		}
		return fmt.Errorf("login refused: %s", msg)
	}
	p.sid = out.Session.SID // empty when Pi-hole has no password
	return nil
}

type piholeLines struct {
	Config struct {
		Misc struct {
			DnsmasqLines []string `json:"dnsmasq_lines"`
		} `json:"misc"`
	} `json:"config"`
}

func (p *pihole) present(ctx context.Context, _ []string) (map[string]bool, error) {
	if err := p.login(ctx); err != nil {
		return nil, err
	}
	var out piholeLines
	if err := p.request(ctx, http.MethodGet, "/api/config/misc/dnsmasq_lines", nil, &out); err != nil {
		return nil, err
	}
	p.lines = out.Config.Misc.DnsmasqLines
	have := map[string]bool{}
	for _, l := range p.lines {
		have[strings.TrimSpace(l)] = true
	}
	return have, nil
}

func (p *pihole) apply(ctx context.Context, add, remove []string) (added, removed []string, err error) {
	if len(add)+len(remove) == 0 {
		return nil, nil, nil
	}
	lines := slices.DeleteFunc(slices.Clone(p.lines), func(l string) bool {
		return slices.Contains(remove, strings.TrimSpace(l))
	})
	lines = append(lines, add...)
	var in piholeLines
	in.Config.Misc.DnsmasqLines = lines
	if err := p.request(ctx, http.MethodPatch, "/api/config", in, nil); err != nil {
		return nil, nil, err
	}
	return add, remove, nil
}

func (p *pihole) close(ctx context.Context) {
	// Pi-hole has a few API seats; give this one back.
	if p.sid != "" {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = p.request(ctx, http.MethodDelete, "/api/auth", nil, nil)
	}
}
