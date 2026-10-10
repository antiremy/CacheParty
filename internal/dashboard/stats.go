package dashboard

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"

	"cacheparty/internal/proxy"
)

// statsVersion is bumped when the file's layout changes; a file of another
// version is ignored.
const statsVersion = 1

// SaveEvery is how often Run writes the traffic totals to Config.StatsFile.
const saveEvery = time.Minute

// statsFile is the saved form of Traffic: the cumulative bytes of every
// service, game, client, upstream host and HTTPS server name, and when each
// last moved. HTTPS was added later; a file without it loads as none.
type statsFile struct {
	Version  int                    `json:"version"`
	Services map[string]savedFlows  `json:"services"`
	Clients  map[string]savedCounts `json:"clients"`
	Hosts    map[string]savedCounts `json:"hosts"`
	HTTPS    map[string]savedCounts `json:"https"`
}

type savedCounts struct {
	Sent    int64 `json:"sent"`
	Fetched int64 `json:"fetched"`
	Last    int64 `json:"last,omitempty"` // unix ms of the last bytes moved
}

type savedFlows struct {
	savedCounts
	Apps []savedApp `json:"apps"`
}

type savedApp struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	savedCounts
}

// save writes the totals to path atomically. last gives when a stream last
// moved bytes.
func (t *Traffic) save(path string, last func(flowKey) time.Time) error {
	snap := t.snapshot()
	ms := func(k flowKey) int64 {
		if at := last(k); !at.IsZero() {
			return at.UnixMilli()
		}
		return 0
	}
	counts := func(k flowKey) savedCounts {
		v := snap[k]
		return savedCounts{Sent: v[0], Fetched: v[1], Last: ms(k)}
	}
	f := statsFile{
		Version:  statsVersion,
		Services: map[string]savedFlows{},
		Clients:  map[string]savedCounts{},
		Hosts:    map[string]savedCounts{},
		HTTPS:    map[string]savedCounts{},
	}
	for k := range snap {
		switch {
		case k.isService():
			s := f.Services[k.Service]
			s.savedCounts = counts(k)
			f.Services[k.Service] = s
		case k.Group == groupClient:
			f.Clients[k.Name] = counts(k)
		case k.Group == groupUpstream:
			f.Hosts[k.Name] = counts(k)
		case k.Group == groupHTTPS:
			f.HTTPS[k.Name] = counts(k)
		case k.Group == "":
			s := f.Services[k.Service]
			s.Apps = append(s.Apps, savedApp{k.App.Kind, k.App.ID, counts(k)})
			f.Services[k.Service] = s
		}
	}
	for _, s := range f.Services {
		sort.Slice(s.Apps, func(i, j int) bool {
			a, b := s.Apps[i], s.Apps[j]
			return a.Kind+"/"+a.ID < b.Kind+"/"+b.ID
		})
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".stats-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// load adds the totals saved at path to t, which should be empty, and
// returns when each stream last moved bytes. A missing file is not an
// error; a file that cannot be read or is of another version is, and
// leaves t empty.
func (t *Traffic) load(path string) (map[flowKey]time.Time, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f statsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	if f.Version != statsVersion {
		return nil, errors.New("unsupported version")
	}

	seen := map[flowKey]time.Time{}
	note := func(k flowKey, c savedCounts) {
		if c.Last != 0 {
			seen[k] = time.UnixMilli(c.Last)
		}
	}
	set := func(fl *proxy.Flow, c savedCounts) {
		fl.Sent.Add(max(c.Sent, 0))
		fl.Fetched.Add(max(c.Fetched, 0))
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	for name, s := range f.Services {
		if len(t.services) >= maxServices {
			break
		}
		sf := &serviceFlows{total: newFlow(nil), apps: map[app]*proxy.Flow{}}
		set(sf.total, s.savedCounts)
		note(flowKey{Service: name}, s.savedCounts)
		for _, a := range s.Apps {
			if len(sf.apps) >= maxApps {
				break
			}
			k := app{a.Kind, a.ID}
			fl := newFlow(sf.total)
			set(fl, a.savedCounts)
			sf.apps[k] = fl
			note(flowKey{Service: name, App: k}, a.savedCounts)
		}
		t.services[name] = sf
	}
	for name, c := range f.Clients {
		if len(t.clients) >= maxClients {
			break
		}
		fl := &proxy.Flow{Sent: newFlow(nil).Sent, Fetched: newFlow(nil).Fetched}
		set(fl, c)
		t.clients[name] = fl
		note(flowKey{Group: groupClient, Name: name}, c)
	}
	for name, c := range f.Hosts {
		if len(t.hosts) >= maxHosts {
			break
		}
		fl := &proxy.Flow{Fetched: newFlow(nil).Fetched}
		set(fl, c)
		t.hosts[name] = fl
		note(flowKey{Group: groupUpstream, Name: name}, c)
	}
	for name, c := range f.HTTPS {
		if len(t.https) >= maxHTTPS {
			break
		}
		fl := newFlow(nil)
		set(fl, c)
		t.https[name] = fl
		note(flowKey{Group: groupHTTPS, Name: name}, c)
	}
	return seen, nil
}
