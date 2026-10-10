package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

const chunkSize = 1 << 20

type loadCfg struct {
	target   string // cache host:port to load
	host     string // Host header, must be in the cache's domain list
	origin   string // origin host:port, for /__stats
	label    string
	scenario string
	clients  int
	size     int64
	chunks   int
	runID    string
}

func runLoad(args []string) error {
	var c loadCfg
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	fs.StringVar(&c.target, "target", "127.0.0.1:80", "cache address")
	fs.StringVar(&c.host, "host", "bench.test", "Host header")
	fs.StringVar(&c.origin, "origin", "origin:80", "origin address for upstream stats")
	fs.StringVar(&c.label, "label", "", "target name in the report (default: -target)")
	fs.StringVar(&c.scenario, "scenario", "big", "big: every client GETs one large object; chunks: every client GETs all 1 MiB objects, in its own order")
	fs.IntVar(&c.clients, "clients", 50, "concurrent clients")
	fs.Int64Var(&c.size, "size", 256<<20, "object size for -scenario big")
	fs.IntVar(&c.chunks, "chunks", 128, "object count for -scenario chunks")
	fs.StringVar(&c.runID, "run", fmt.Sprint(time.Now().UnixNano()), "unique per run so the cold phase never hits a previous cache")
	wait := fs.Duration("wait", 60*time.Second, "how long to wait for the cache to answer /lancache-heartbeat")
	_ = fs.Parse(args)
	if c.label == "" {
		c.label = c.target
	}
	if err := waitReady(c, *wait); err != nil {
		return err
	}
	urls, unique, err := c.workload()
	if err != nil {
		return err
	}
	fmt.Print(tableHeader)
	for _, phase := range []string{"cold", "warm"} {
		rep, err := c.runPhase(phase, urls, unique)
		if err != nil {
			return err
		}
		rep.row(os.Stdout, c.label)
	}
	return nil
}

// workload returns the URL paths each client fetches and the number of
// distinct bytes they add up to.
func (c *loadCfg) workload() ([]string, int64, error) {
	switch c.scenario {
	case "big":
		return []string{objPath(c.runID, c.size, "big.bin")}, c.size, nil
	case "chunks":
		urls := make([]string, c.chunks)
		for i := range urls {
			urls[i] = objPath(c.runID, chunkSize, fmt.Sprintf("chunk%04d", i))
		}
		return urls, int64(c.chunks) * chunkSize, nil
	}
	return nil, 0, fmt.Errorf("unknown scenario %q", c.scenario)
}

func (c *loadCfg) client() *http.Client {
	d := net.Dialer{Timeout: 5 * time.Second}
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return d.DialContext(ctx, network, c.target)
		},
		DisableCompression:  true,
		MaxIdleConnsPerHost: c.clients,
	}}
}

func waitReady(c loadCfg, d time.Duration) error {
	hc := c.client()
	deadline := time.Now().Add(d)
	for {
		req, _ := http.NewRequest("GET", "http://"+c.host+"/lancache-heartbeat", nil)
		resp, err := hc.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s not ready after %s (last error: %v)", c.target, d, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (c *loadCfg) originStats() (originStats, error) {
	var s originStats
	resp, err := http.Get("http://" + c.origin + "/__stats")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	return s, json.NewDecoder(resp.Body).Decode(&s)
}

// runPhase has every client fetch every URL once, all clients starting
// together, and reports what the clients and the origin saw.
func (c *loadCfg) runPhase(name string, urls []string, unique int64) (*phaseReport, error) {
	before, err := c.originStats()
	if err != nil {
		return nil, err
	}
	hc := c.client()
	rep := &phaseReport{Name: name, Unique: unique}
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < c.clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			order := c.order(urls, i)
			buf := make([]byte, 64<<10)
			<-start
			for _, u := range order {
				ttfb, n, err := c.fetch(hc, u, buf)
				mu.Lock()
				rep.Requests++
				rep.Bytes += n
				if err != nil {
					rep.Errors++
					fmt.Fprintln(os.Stderr, "bench: error:", err)
				} else {
					rep.TTFB = append(rep.TTFB, ttfb)
				}
				mu.Unlock()
			}
		}()
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	rep.Wall = time.Since(t0)
	after, err := c.originStats()
	if err != nil {
		return nil, err
	}
	rep.Upstream = originStats{after.Requests - before.Requests, after.Bytes - before.Bytes}
	return rep, nil
}

// fetch GETs path and returns the time to the first body byte and the
// number of body bytes. A short, long or non-200 body is an error.
func (c *loadCfg) fetch(hc *http.Client, path string, buf []byte) (time.Duration, int64, error) {
	size, _ := objSize(path)
	req, _ := http.NewRequest("GET", "http://"+c.host+path, nil)
	t0 := time.Now()
	resp, err := hc.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	var ttfb time.Duration
	var n int64
	for {
		k, err := resp.Body.Read(buf)
		if k > 0 && n == 0 {
			ttfb = time.Since(t0)
			if resp.StatusCode == 200 && !patternOK(buf[:k]) {
				return 0, n, fmt.Errorf("%s: wrong bytes at offset 0", path)
			}
		}
		n += int64(k)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, n, fmt.Errorf("%s: %w", path, err)
		}
	}
	if resp.StatusCode != 200 || n != size {
		return 0, n, fmt.Errorf("%s: status %d, %d bytes, want 200 and %d", path, resp.StatusCode, n, size)
	}
	return ttfb, n, nil
}

// patternOK checks the start of a body against the origin's pattern.
func patternOK(b []byte) bool {
	for i := range b[:min(len(b), 4096)] {
		if b[i] != patternByte(int64(i)) {
			return false
		}
	}
	return true
}

// order is the sequence client i fetches: the chunks scenario shuffles it
// per client, as real clients do not walk a depot in lockstep.
func (c *loadCfg) order(urls []string, i int) []string {
	if c.scenario != "chunks" {
		return urls
	}
	out := make([]string, len(urls))
	for j, k := range rand.New(rand.NewSource(int64(i))).Perm(len(urls)) {
		out[j] = urls[k]
	}
	return out
}
