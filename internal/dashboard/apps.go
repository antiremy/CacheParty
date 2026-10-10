package dashboard

import (
	"net"
	"net/http"
	"strings"
)

// app is what a request downloads within its service, such as one Steam
// depot. Kind says what ID is: "depot", "product", "app" or "host".
type app struct {
	Kind string
	ID   string
}

// tag is how a's objects are labelled in the cache index (ObjectMeta.App):
// "<service>/<kind>/<id>", such as "steam/depot/990081". IDs never hold a
// slash, so a tag splits back into its parts.
func (a app) tag(service string) string { return service + "/" + a.Kind + "/" + a.ID }

// AppTag names the game r downloads through service in the form purges use;
// it is proxy.Config.App.
func AppTag(service string, r *http.Request) string { return appOf(service, r).tag(service) }

// maxAppID bounds the IDs kept, since they come from client requests.
const maxAppID = 100

// appOf picks out the game a request belongs to from its URL, where the
// service's URL layout names one:
//
//	steam      /depot/<depot id>/...
//	blizzard   /tpr/<product>/...
//	epicgames  /Builds/<app>/... or /Builds/Org/<org>/<app>/...
//
// Anything else is grouped by CDN host, which often still separates games
// (lol.dyn.riotcdn.net and valorant.dyn.riotcdn.net, say).
func appOf(service string, r *http.Request) app {
	seg := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	at := func(i int) string {
		if i < len(seg) {
			return seg[i]
		}
		return ""
	}
	switch {
	case service == "steam" && at(0) == "depot" && isDigits(at(1)):
		return app{"depot", at(1)}
	case service == "blizzard" && at(0) == "tpr" && isName(at(1)) && at(2) != "":
		return app{"product", at(1)}
	case service == "epicgames" && at(0) == "Builds":
		id := at(1)
		if id == "Org" {
			id = at(3)
		}
		if isName(id) && id != "Org" {
			return app{"app", id}
		}
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(host)
	if len(host) > maxAppID {
		host = host[:maxAppID]
	}
	return app{"host", host}
}

func isDigits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, c := range []byte(s) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isName accepts the short path segments services use as product names.
func isName(s string) bool {
	if s == "" || len(s) > maxAppID {
		return false
	}
	for _, c := range []byte(s) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
		if !ok {
			return false
		}
	}
	return true
}
