// Package metrics is a small Prometheus text-format registry. It covers only
// what CacheParty needs (counters, labelled counters, gauges read at scrape
// time), which keeps the client library out of the binary.
package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Counter is a monotonically increasing value. The zero value is ready to
// use, and a nil *Counter ignores updates, so code can be handed no metrics.
type Counter struct{ v atomic.Int64 }

// Add adds n (which must not be negative).
func (c *Counter) Add(n int64) {
	if c != nil {
		c.v.Add(n)
	}
}

// Inc adds 1.
func (c *Counter) Inc() { c.Add(1) }

// Value returns the current count.
func (c *Counter) Value() int64 {
	if c == nil {
		return 0
	}
	return c.v.Load()
}

// Vec is a counter family with one label.
type Vec struct {
	mu sync.RWMutex
	m  map[string]*Counter
}

// With returns the counter for label value v, creating it on first use.
// A nil *Vec returns nil, which is a valid no-op *Counter.
func (v *Vec) With(val string) *Counter {
	if v == nil {
		return nil
	}
	v.mu.RLock()
	c := v.m[val]
	v.mu.RUnlock()
	if c != nil {
		return c
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if c = v.m[val]; c == nil {
		if v.m == nil {
			v.m = map[string]*Counter{}
		}
		c = new(Counter)
		v.m[val] = c
	}
	return c
}

type family struct {
	name, help, typ, label string
	counter                *Counter
	vec                    *Vec
	gauge                  func() float64
}

// Registry holds metrics and renders them.
type Registry struct {
	mu       sync.Mutex
	families []*family
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) add(f *family) {
	r.mu.Lock()
	r.families = append(r.families, f)
	r.mu.Unlock()
}

// Counter registers and returns a counter.
func (r *Registry) Counter(name, help string) *Counter {
	c := new(Counter)
	r.add(&family{name: name, help: help, typ: "counter", counter: c})
	return c
}

// CounterVec registers a counter family keyed by one label.
func (r *Registry) CounterVec(name, help, label string) *Vec {
	v := new(Vec)
	r.add(&family{name: name, help: help, typ: "counter", label: label, vec: v})
	return v
}

// Gauge registers a gauge computed by fn at each scrape.
func (r *Registry) Gauge(name, help string, fn func() float64) {
	r.add(&family{name: name, help: help, typ: "gauge", gauge: fn})
}

// WriteTo renders the registry in the Prometheus text exposition format.
func (r *Registry) WriteTo(w io.Writer) (int64, error) {
	r.mu.Lock()
	fams := append([]*family(nil), r.families...)
	r.mu.Unlock()
	sort.Slice(fams, func(i, j int) bool { return fams[i].name < fams[j].name })

	var b strings.Builder
	for _, f := range fams {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.name, f.help, f.name, f.typ)
		switch {
		case f.counter != nil:
			fmt.Fprintf(&b, "%s %d\n", f.name, f.counter.Value())
		case f.gauge != nil:
			fmt.Fprintf(&b, "%s %g\n", f.name, f.gauge())
		case f.vec != nil:
			f.vec.mu.RLock()
			vals := make([]string, 0, len(f.vec.m))
			for v := range f.vec.m {
				vals = append(vals, v)
			}
			sort.Strings(vals)
			for _, v := range vals {
				fmt.Fprintf(&b, "%s{%s=\"%s\"} %d\n", f.name, f.label, escape(v), f.vec.m[v].Value())
			}
			f.vec.mu.RUnlock()
		}
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escape(s string) string { return labelEscaper.Replace(s) }

// Handler serves the registry.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = r.WriteTo(w)
	})
}
