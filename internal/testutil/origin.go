package testutil

import (
	"cmp"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Req is one request the fake origin received.
type Req struct {
	Path  string
	Query string
	Range string
	AE    string // Accept-Encoding
}

// Obj describes a path served by Origin.
type Obj struct {
	Body         []byte
	ContentType  string
	LastModified time.Time

	IgnoreRange bool   // answer every GET with a full 200
	NoLength    bool   // with IgnoreRange: omit Content-Length (chunked)
	Redirect    string // answer 302 to this location instead
	Status      int    // answer this status with a short body instead
	Encoding    string // set Content-Encoding on responses

	// Gate, if set, holds the response (after the request is recorded)
	// until it is closed. Started receives the path once the request
	// has arrived.
	Gate    chan struct{}
	Started chan string
}

// Origin is a fake CDN origin that records what it was asked for.
type Origin struct {
	*httptest.Server
	mu   sync.Mutex
	objs map[string]*Obj
	reqs []Req
}

// NewOrigin starts an Origin.
func NewOrigin(t testing.TB) *Origin {
	t.Helper()
	o := &Origin{objs: map[string]*Obj{}}
	o.Server = httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(o.Server.Close)
	return o
}

// Set registers (or replaces) a path.
func (o *Origin) Set(path string, obj *Obj) {
	o.mu.Lock()
	o.objs[path] = obj
	o.mu.Unlock()
}

// Requests returns a copy of every request seen, in order.
func (o *Origin) Requests() []Req {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]Req(nil), o.reqs...)
}

// Count is the number of requests for path.
func (o *Origin) Count(path string) int {
	n := 0
	for _, r := range o.Requests() {
		if r.Path == path {
			n++
		}
	}
	return n
}

// Ranges lists the Range headers received for path, ordered by start offset
// (chunks may be fetched concurrently, so arrival order is not stable).
func (o *Origin) Ranges(path string) []string {
	var out []string
	for _, r := range o.Requests() {
		if r.Path == path {
			out = append(out, r.Range)
		}
	}
	slices.SortStableFunc(out, func(a, b string) int { return cmp.Compare(rangeStart(a), rangeStart(b)) })
	return out
}

// rangeStart is the first offset of "bytes=a-b", or -1 if there is none.
func rangeStart(h string) int64 {
	a, _, _ := strings.Cut(strings.TrimPrefix(h, "bytes="), "-")
	n, err := strconv.ParseInt(a, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// Pattern returns deterministic, position-dependent bytes, so any shifted or
// swapped chunk is detected.
func Pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*131 + i/251 + 7)
	}
	return b
}

func (o *Origin) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	o.reqs = append(o.reqs, Req{r.URL.Path, r.URL.RawQuery, r.Header.Get("Range"), r.Header.Get("Accept-Encoding")})
	obj := o.objs[r.URL.Path]
	o.mu.Unlock()
	if obj == nil {
		http.NotFound(w, r)
		return
	}
	if obj.Started != nil {
		obj.Started <- r.URL.Path
	}
	if obj.Gate != nil {
		select {
		case <-obj.Gate:
		case <-r.Context().Done():
			return
		}
	}
	switch {
	case obj.Redirect != "":
		http.Redirect(w, r, obj.Redirect, http.StatusFound)
		return
	case obj.Status != 0:
		w.WriteHeader(obj.Status)
		fmt.Fprintf(w, "status %d", obj.Status)
		return
	}
	h := w.Header()
	if obj.ContentType != "" {
		h.Set("Content-Type", obj.ContentType)
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	if !obj.LastModified.IsZero() {
		h.Set("Last-Modified", obj.LastModified.UTC().Format(http.TimeFormat))
	}
	if obj.Encoding != "" {
		h.Set("Content-Encoding", obj.Encoding)
	}
	h.Set("ETag", `"upstream-etag"`)
	if obj.IgnoreRange {
		if !obj.NoLength {
			h.Set("Content-Length", strconv.Itoa(len(obj.Body)))
		}
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			w.Write(obj.Body)
		}
		return
	}
	// ServeContent implements single-range requests, 206 and 416 for us.
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(obj.Body)))
}
