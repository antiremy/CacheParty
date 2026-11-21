package dnssync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// technitium speaks the Technitium DNS Server API. Each name goes into a
// conditional forwarder zone that forwards to the server itself
// ("this-server"): the records added there answer for the cache-domains
// names and everything else in the zone resolves as it did before. One zone
// covers each domain and the names under it, e.g. steamcontent.com holds
// both *.steamcontent.com and lancache.steamcontent.com.
//
// Keys are "zone <zone>" and "record <zone> <name> <ip>".
type technitium struct {
	*client
}

func (t *technitium) keys(patterns []string, ips []netip.Addr) []string {
	bases := map[string]bool{}
	for _, p := range patterns {
		bases[strings.TrimPrefix(p, "*.")] = true
	}
	// The zone for a name is its shortest ancestor (or itself) among the
	// bases.
	zoneOf := func(name string) string {
		zone := name
		for d := name; ; {
			if bases[d] {
				zone = d
			}
			_, rest, ok := strings.Cut(d, ".")
			if !ok {
				return zone
			}
			d = rest
		}
	}
	var out []string
	zones := map[string]bool{}
	for _, p := range patterns {
		z := zoneOf(strings.TrimPrefix(p, "*."))
		if !zones[z] {
			zones[z] = true
			out = append(out, "zone "+z)
		}
		for _, ip := range ips {
			out = append(out, "record "+z+" "+p+" "+ip.String())
		}
	}
	return out
}

// call posts form to path and decodes the "response" member into out.
func (t *technitium) call(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.t.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+t.t.Secret)
	var res struct {
		Status       string          `json:"status"`
		ErrorMessage string          `json:"errorMessage"`
		Response     json.RawMessage `json:"response"`
	}
	if err := t.do(req, &res, nil); err != nil {
		return err
	}
	switch res.Status {
	case "ok":
	case "invalid-token":
		return fmt.Errorf("the API token was refused")
	case "error":
		return fmt.Errorf("%s: %s", path, res.ErrorMessage)
	default:
		return fmt.Errorf("%s: unexpected status %q", path, res.Status)
	}
	if out == nil || len(res.Response) == 0 {
		return nil
	}
	return json.Unmarshal(res.Response, out)
}

func (t *technitium) present(ctx context.Context, keys []string) (map[string]bool, error) {
	have := map[string]bool{}
	exists := map[string]bool{}
	for page := 1; ; page++ {
		var out struct {
			TotalPages int `json:"totalPages"`
			Zones      []struct {
				Name string `json:"name"`
			} `json:"zones"`
		}
		form := url.Values{"pageNumber": {strconv.Itoa(page)}, "zonesPerPage": {"1000"}}
		if err := t.call(ctx, "/api/zones/list", form, &out); err != nil {
			return nil, err
		}
		for _, z := range out.Zones {
			exists[strings.ToLower(z.Name)] = true
		}
		if page >= out.TotalPages { // 0 from versions without paging
			break
		}
	}
	read := map[string]bool{}
	for _, k := range keys {
		f := strings.Fields(k)
		z := f[1]
		if !exists[z] || read[z] {
			continue
		}
		read[z] = true
		have["zone "+z] = true
		var out struct {
			Records []struct {
				Name  string `json:"name"`
				Type  string `json:"type"`
				RData struct {
					IPAddress string `json:"ipAddress"`
				} `json:"rData"`
			} `json:"records"`
		}
		form := url.Values{"zone": {z}, "domain": {z}, "listZone": {"true"}}
		if err := t.call(ctx, "/api/zones/records/get", form, &out); err != nil {
			return nil, err
		}
		for _, r := range out.Records {
			if r.Type != "A" && r.Type != "AAAA" {
				continue
			}
			if ip, err := netip.ParseAddr(r.RData.IPAddress); err == nil {
				have["record "+z+" "+strings.ToLower(r.Name)+" "+ip.String()] = true
			}
		}
	}
	return have, nil
}

func recordForm(f []string) url.Values {
	typ := "A"
	if netip.MustParseAddr(f[3]).Is6() {
		typ = "AAAA"
	}
	return url.Values{"zone": {f[1]}, "domain": {f[2]}, "type": {typ}, "ipAddress": {f[3]}}
}

func (t *technitium) apply(ctx context.Context, add, remove []string) (added, removed []string, err error) {
	dropZones := map[string]bool{}
	for _, k := range remove {
		if f := strings.Fields(k); f[0] == "zone" {
			dropZones[f[1]] = true
		}
	}
	// Records first, except in zones about to go, whose records go with
	// them.
	var inDropped []string
	for _, k := range remove {
		f := strings.Fields(k)
		if f[0] != "record" {
			continue
		}
		if dropZones[f[1]] {
			inDropped = append(inDropped, k)
			continue
		}
		if err := t.call(ctx, "/api/zones/records/delete", recordForm(f), nil); err != nil {
			return added, removed, err
		}
		removed = append(removed, k)
	}
	for _, k := range remove {
		f := strings.Fields(k)
		if f[0] != "zone" {
			continue
		}
		if err := t.call(ctx, "/api/zones/delete", url.Values{"zone": {f[1]}}, nil); err != nil {
			return added, removed, err
		}
		removed = append(removed, k)
		for _, r := range inDropped {
			if strings.Fields(r)[1] == f[1] {
				removed = append(removed, r)
			}
		}
	}
	// Zones before the records that go in them.
	for _, k := range add {
		f := strings.Fields(k)
		if f[0] != "zone" {
			continue
		}
		form := url.Values{"zone": {f[1]}, "type": {"Forwarder"}, "forwarder": {"this-server"}, "protocol": {"Udp"}}
		if err := t.call(ctx, "/api/zones/create", form, nil); err != nil {
			return added, removed, err
		}
		added = append(added, k)
	}
	for _, k := range add {
		f := strings.Fields(k)
		if f[0] != "record" {
			continue
		}
		if err := t.call(ctx, "/api/zones/records/add", recordForm(f), nil); err != nil {
			return added, removed, err
		}
		added = append(added, k)
	}
	return added, removed, nil
}

func (t *technitium) close(context.Context) {}
