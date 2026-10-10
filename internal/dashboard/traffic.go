package dashboard

import (
	"net"
	"net/http"
	"sync"

	"cacheparty/internal/metrics"
	"cacheparty/internal/proxy"
)

// maxServices bounds how many cache identifiers are tracked separately. An
// unmatched Host is its own identifier, so the set is open-ended; the rest
// share otherService. maxApps does the same for each service's games.
const (
	maxServices  = 64
	maxApps      = 500
	maxClients   = 1000
	maxHosts     = 500
	maxHTTPS     = 500
	otherService = "other"
)

var otherApp = app{"other", "other"}

// Groups of flowKey besides services: the bytes each client IP was sent, the
// bytes fetched from each upstream host, and the bytes relayed for each
// HTTPS server name (sent to clients, and received from them as Fetched).
const (
	groupClient   = "client"
	groupUpstream = "upstream"
	groupHTTPS    = "https"
)

// otherName stands for the clients or hosts past their limit.
const otherName = "other"

// flowKey names a counted stream: a service's total when App is zero, one
// of its games, or, with Group set, a client or upstream host (in Name).
type flowKey struct {
	Service string
	App     app
	Group   string
	Name    string
}

// isService reports whether k is a service's total.
func (k flowKey) isService() bool { return k.Group == "" && k.App == (app{}) }

// Traffic hands out per-service and per-game byte counters; its Flow method
// is proxy.Config.Flows.
type Traffic struct {
	mu       sync.RWMutex
	services map[string]*serviceFlows
	clients  map[string]*proxy.Flow // by client IP
	hosts    map[string]*proxy.Flow // by upstream host; Fetched only
	https    map[string]*proxy.Flow // by server name; Fetched is from clients
}

type serviceFlows struct {
	total *proxy.Flow
	apps  map[app]*proxy.Flow
}

// NewTraffic returns an empty Traffic.
func NewTraffic() *Traffic {
	return &Traffic{
		services: map[string]*serviceFlows{},
		clients:  map[string]*proxy.Flow{},
		hosts:    map[string]*proxy.Flow{},
		https:    map[string]*proxy.Flow{},
	}
}

func newFlow(parent *proxy.Flow) *proxy.Flow {
	return &proxy.Flow{Sent: new(metrics.Counter), Fetched: new(metrics.Counter), Parent: parent}
}

// Flow returns the counters for r: those of its game within service, which
// also count toward the service, and toward r's client and upstream host.
func (t *Traffic) Flow(service string, r *http.Request) *proxy.Flow {
	g := t.gameFlow(service, r)
	return &proxy.Flow{
		Sent: g.Sent, Fetched: g.Fetched, Parent: g.Parent,
		Also: []*proxy.Flow{
			t.named(t.clients, maxClients, clientOf(r), func() *proxy.Flow { return &proxy.Flow{Sent: new(metrics.Counter), Fetched: new(metrics.Counter)} }),
			t.named(t.hosts, maxHosts, hostOf(r), func() *proxy.Flow { return &proxy.Flow{Fetched: new(metrics.Counter)} }),
		},
	}
}

// HTTPS returns the counters for the bytes relayed to and from clients for
// one server name on the HTTPS passthrough; it is sni.Config.Domain.
func (t *Traffic) HTTPS(name string) (sent, received *metrics.Counter) {
	f := t.named(t.https, maxHTTPS, name, func() *proxy.Flow { return newFlow(nil) })
	return f.Sent, f.Fetched
}

// clientOf is the IP address r came from.
func clientOf(r *http.Request) string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if len(ip) > maxAppID {
		ip = ip[:maxAppID]
	}
	return ip
}

// hostOf is the upstream host r asks for.
func hostOf(r *http.Request) string { return appOf("", r).ID }

// named returns the flow for name in m, creating it with mk, or the shared
// "other" flow once m holds limit names.
func (t *Traffic) named(m map[string]*proxy.Flow, limit int, name string, mk func() *proxy.Flow) *proxy.Flow {
	t.mu.RLock()
	f := m[name]
	t.mu.RUnlock()
	if f != nil {
		return f
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if f = m[name]; f != nil {
		return f
	}
	if len(m) >= limit {
		name = otherName
		if f = m[name]; f != nil {
			return f
		}
	}
	f = mk()
	m[name] = f
	return f
}

// gameFlow returns the counters for r's game within service, creating them
// on first use.
func (t *Traffic) gameFlow(service string, r *http.Request) *proxy.Flow {
	a := appOf(service, r)
	t.mu.RLock()
	s := t.services[service]
	var f *proxy.Flow
	if s != nil {
		f = s.apps[a]
	}
	t.mu.RUnlock()
	if f != nil {
		return f
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	if s = t.services[service]; s == nil {
		if len(t.services) >= maxServices {
			service = otherService
			s = t.services[service]
		}
		if s == nil {
			s = &serviceFlows{total: newFlow(nil), apps: map[app]*proxy.Flow{}}
			t.services[service] = s
		}
	}
	if f = s.apps[a]; f != nil {
		return f
	}
	if len(s.apps) >= maxApps {
		a = otherApp
		if f = s.apps[a]; f != nil {
			return f
		}
	}
	f = newFlow(s.total)
	s.apps[a] = f
	return f
}

// snapshot returns the cumulative sent and fetched bytes of every service
// and every game.
func (t *Traffic) snapshot() map[flowKey][2]int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[flowKey][2]int64, 2*len(t.services)+len(t.clients)+len(t.hosts)+len(t.https))
	for name, s := range t.services {
		out[flowKey{Service: name}] = [2]int64{s.total.Sent.Value(), s.total.Fetched.Value()}
		for a, f := range s.apps {
			out[flowKey{Service: name, App: a}] = [2]int64{f.Sent.Value(), f.Fetched.Value()}
		}
	}
	for name, f := range t.clients {
		out[flowKey{Group: groupClient, Name: name}] = [2]int64{f.Sent.Value(), f.Fetched.Value()}
	}
	for name, f := range t.hosts {
		out[flowKey{Group: groupUpstream, Name: name}] = [2]int64{0, f.Fetched.Value()}
	}
	for name, f := range t.https {
		out[flowKey{Group: groupHTTPS, Name: name}] = [2]int64{f.Sent.Value(), f.Fetched.Value()}
	}
	return out
}
