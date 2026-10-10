package metrics

import (
	"strings"
	"testing"
)

func TestRender(t *testing.T) {
	r := NewRegistry()
	c := r.Counter("b_total", "B.")
	v := r.CounterVec("a_total", "A.", "status")
	r.Gauge("c_bytes", "C.", func() float64 { return 2.5 })
	c.Add(3)
	v.With("HIT").Inc()
	v.With(`we"ird`).Add(2)

	var sb strings.Builder
	if _, err := r.WriteTo(&sb); err != nil {
		t.Fatal(err)
	}
	want := `# HELP a_total A.
# TYPE a_total counter
a_total{status="HIT"} 1
a_total{status="we\"ird"} 2
# HELP b_total B.
# TYPE b_total counter
b_total 3
# HELP c_bytes C.
# TYPE c_bytes gauge
c_bytes 2.5
`
	if sb.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", sb.String(), want)
	}
}

func TestNilIsNoop(t *testing.T) {
	var c *Counter
	c.Inc()
	var v *Vec
	v.With("x").Add(5)
	if c.Value() != 0 {
		t.Fatal("nil counter should read 0")
	}
}
