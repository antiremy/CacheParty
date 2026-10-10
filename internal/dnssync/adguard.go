package dnssync

import (
	"context"
	"net/http"
	"net/netip"
	"strings"
)

// adguard speaks the AdGuard Home API. Names are DNS rewrites, one per
// name and address; AdGuard's "*.example.com" means what cache-domains
// means by it (subdomains only), so patterns go across as they are. A key
// is "domain answer".
type adguard struct {
	*client
}

func (a *adguard) keys(patterns []string, ips []netip.Addr) []string {
	var out []string
	for _, pat := range patterns {
		for _, ip := range ips {
			out = append(out, pat+" "+ip.String())
		}
	}
	return out
}

type rewrite struct {
	Domain string `json:"domain"`
	Answer string `json:"answer"`
}

func (a *adguard) request(ctx context.Context, method, path string, in, out any) error {
	req, err := a.jsonRequest(ctx, method, "/control"+path, in)
	if err != nil {
		return err
	}
	if a.t.Username != "" || a.t.Secret != "" {
		req.SetBasicAuth(a.t.Username, a.t.Secret)
	}
	return a.do(req, out, nil)
}

func (a *adguard) present(ctx context.Context, _ []string) (map[string]bool, error) {
	var list []rewrite
	if err := a.request(ctx, http.MethodGet, "/rewrite/list", nil, &list); err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, r := range list {
		have[strings.ToLower(r.Domain)+" "+r.Answer] = true
	}
	return have, nil
}

func (a *adguard) apply(ctx context.Context, add, remove []string) (added, removed []string, err error) {
	for _, k := range remove {
		d, ip, _ := strings.Cut(k, " ")
		if err := a.request(ctx, http.MethodPost, "/rewrite/delete", rewrite{d, ip}, nil); err != nil {
			return added, removed, err
		}
		removed = append(removed, k)
	}
	for _, k := range add {
		d, ip, _ := strings.Cut(k, " ")
		if err := a.request(ctx, http.MethodPost, "/rewrite/add", rewrite{d, ip}, nil); err != nil {
			return added, removed, err
		}
		added = append(added, k)
	}
	return added, removed, nil
}

func (a *adguard) close(context.Context) {}
