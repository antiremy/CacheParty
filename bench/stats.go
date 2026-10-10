package main

import (
	"fmt"
	"io"
	"slices"
	"time"
)

// percentile returns the p-th percentile (0..1) of sorted, by nearest rank.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p*float64(len(sorted))+0.5) - 1
	return sorted[max(0, min(i, len(sorted)-1))]
}

// phaseReport is the outcome of one phase (cold or warm) of a scenario.
type phaseReport struct {
	Name     string
	Wall     time.Duration
	Requests int
	Errors   int
	Bytes    int64 // delivered to clients
	TTFB     []time.Duration
	Upstream originStats
	Unique   int64 // distinct bytes the scenario asks for
}

func (p *phaseReport) mbps() float64 {
	return float64(p.Bytes) / p.Wall.Seconds() / 1e6
}

func (p *phaseReport) row(w io.Writer, label string) {
	slices.Sort(p.TTFB)
	fmt.Fprintf(w, "| %s | %s | %.0f | %s | %s | %.2f | %d | %d |\n",
		label, p.Name, p.mbps(),
		ms(percentile(p.TTFB, 0.50)), ms(percentile(p.TTFB, 0.99)),
		float64(p.Upstream.Bytes)/float64(p.Unique), p.Upstream.Requests, p.Errors)
}

func ms(d time.Duration) string { return fmt.Sprintf("%.1f", float64(d)/float64(time.Millisecond)) }

const tableHeader = "| target | phase | MB/s | TTFB p50 ms | TTFB p99 ms | upstream/unique | upstream reqs | errors |\n" +
	"|---|---|---:|---:|---:|---:|---:|---:|\n"
