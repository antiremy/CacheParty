package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestPercentile(t *testing.T) {
	d := []time.Duration{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	for p, want := range map[float64]time.Duration{0.5: 5, 0.99: 10, 0.1: 1, 1: 10} {
		if got := percentile(d, p); got != want {
			t.Errorf("p%v = %v, want %v", p, got, want)
		}
	}
	if percentile(nil, 0.5) != 0 {
		t.Error("empty input should give 0")
	}
}

func TestOriginRangeAndStats(t *testing.T) {
	o := &origin{}
	srv := httptest.NewServer(o)
	defer srv.Close()
	c := loadCfg{target: srv.Listener.Addr().String(), host: "x", origin: srv.Listener.Addr().String(), clients: 1}
	hc := c.client()
	path := objPath("t", 100000, "a")
	if _, n, err := c.fetch(hc, path, make([]byte, 4096)); err != nil || n != 100000 {
		t.Fatalf("full fetch: %d bytes, %v", n, err)
	}
	st, err := c.originStats()
	if err != nil || st.Requests != 1 || st.Bytes != 100000 {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	req := mustReq(t, c, path, "bytes=1000-1999")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 206 || len(body) != 1000 || body[0] != patternByte(1000) {
		t.Fatalf("range: status %d, %d bytes", resp.StatusCode, len(body))
	}
}

func mustReq(t *testing.T, c loadCfg, path, rng string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+c.host+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Range", rng)
	return req
}
