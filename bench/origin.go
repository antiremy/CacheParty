package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var modTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

// objPrefix starts every object path: /obj/<size>/<name>. The body is
// generated, so the origin needs no registration and holds nothing in memory.
const objPrefix = "/obj/"

// originStats is what /__stats reports; the load generator diffs it
// across a phase to learn how much the cache fetched upstream.
type originStats struct {
	Requests int64 `json:"requests"`
	Bytes    int64 `json:"bytes"`
}

type origin struct {
	delay time.Duration // before the response starts
	rate  int64         // bytes/second per response, 0 = unlimited
	reqs  atomic.Int64
	bytes atomic.Int64
}

func runOrigin(args []string) error {
	fs := flag.NewFlagSet("origin", flag.ExitOnError)
	addr := fs.String("addr", ":80", "listen address")
	o := &origin{}
	fs.DurationVar(&o.delay, "delay", 0, "wait before each response (simulated WAN latency)")
	fs.Int64Var(&o.rate, "rate", 0, "bytes/second per response (0 = unlimited)")
	_ = fs.Parse(args)
	return http.ListenAndServe(*addr, o)
}

func (o *origin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__stats" {
		_ = json.NewEncoder(w).Encode(originStats{o.reqs.Load(), o.bytes.Load()})
		return
	}
	size, ok := objSize(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	o.reqs.Add(1)
	time.Sleep(o.delay)
	// An explicit Content-Type stops ServeContent from sniffing (reading) the body.
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", modTime, &patternReader{size: size, o: o})
}

// objSize parses the size out of /obj/<size>/<name>.
func objSize(path string) (int64, bool) {
	rest, ok := strings.CutPrefix(path, objPrefix)
	if !ok {
		return 0, false
	}
	s, _, ok := strings.Cut(rest, "/")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0
}

// objPath is the inverse of objSize.
func objPath(runID string, size int64, name string) string {
	return fmt.Sprintf("%s%d/%s/%s", objPrefix, size, runID, name)
}

// patternReader serves deterministic bytes, throttled by the origin's rate.
type patternReader struct {
	size, off int64
	o         *origin
}

func (p *patternReader) Read(b []byte) (int, error) {
	if p.off >= p.size {
		return 0, io.EOF
	}
	b = b[:min(int64(len(b)), p.size-p.off)]
	for i := range b {
		b[i] = patternByte(p.off + int64(i))
	}
	p.off += int64(len(b))
	p.o.bytes.Add(int64(len(b)))
	if p.o.rate > 0 {
		time.Sleep(time.Duration(int64(len(b)) * int64(time.Second) / p.o.rate))
	}
	return len(b), nil
}

func (p *patternReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekCurrent:
		off += p.off
	case io.SeekEnd:
		off += p.size
	}
	if off < 0 {
		return 0, fmt.Errorf("negative seek")
	}
	p.off = off
	return off, nil
}

func patternByte(i int64) byte { return byte(i*131 + i/251 + 7) }
