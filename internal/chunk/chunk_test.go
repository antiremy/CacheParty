package chunk

import (
	"fmt"
	"math"
	"testing"
)

func TestParseTable(t *testing.T) {
	big := "99999999999999999999999" // overflows int64
	for _, tc := range []struct {
		in   string
		want Spec
	}{
		{"", Spec{}},
		{"bytes=0-4", Spec{KindBytes, 0, 4}},
		{"bytes=5-5", Spec{KindBytes, 5, 5}},
		{"BYTES=0-4", Spec{KindBytes, 0, 4}},
		{"  bytes=0-4  ", Spec{KindBytes, 0, 4}},
		{"bytes= 0 - 4 ", Spec{KindBytes, 0, 4}},
		{"bytes=7-", Spec{KindFrom, 7, 0}},
		{"bytes=-3", Spec{KindSuffix, 3, 0}},
		{"bytes=-0", Spec{KindSuffix, 0, 0}},
		{"bytes=0-1,5-6", Spec{Kind: KindMulti}},
		{"bytes=0-1, 5-", Spec{Kind: KindMulti}},
		{"bytes=0-1,garbage", Spec{Kind: KindMulti}},
		{"bytes=5-2", Spec{}}, // invalid: ignored
		{"bytes=-", Spec{}},
		{"bytes=", Spec{}},
		{"bytes=abc", Spec{}},
		{"bytes=a-b", Spec{}},
		{"bytes=1-b", Spec{}},
		{"bytes=+1-2", Spec{}},
		{"bytes=1.5-2", Spec{}},
		{"bytes=--1", Spec{}},
		{"bytes=1-2-3", Spec{}},
		{"items=0-4", Spec{}},
		{"bytes", Spec{}},
		{"bytes 0-4", Spec{}},
		{"bytes=" + big + "-", Spec{KindFrom, math.MaxInt64, 0}},
		{"bytes=0-" + big, Spec{KindBytes, 0, math.MaxInt64}},
		{"bytes=-" + big, Spec{KindSuffix, math.MaxInt64, 0}},
		{"bytes=9223372036854775807-", Spec{KindFrom, math.MaxInt64, 0}},
		{"bytes=" + big + "x-", Spec{}}, // garbage after an overflow is still garbage
	} {
		if got := Parse(tc.in); got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

// ref resolves by marking individual bytes: a deliberately different
// algorithm from Resolve.
func ref(k Kind, a, b, size int64) (Range, Status) {
	if k == KindNone || k == KindMulti {
		return Range{0, size - 1}, StatusFull
	}
	lo, hi := int64(-1), int64(-1)
	for i := int64(0); i < size; i++ {
		in := false
		switch k {
		case KindBytes:
			in = i >= a && i <= b
		case KindFrom:
			in = i >= a
		case KindSuffix:
			in = a > 0 && i >= size-a
		}
		if in {
			if lo < 0 {
				lo = i
			}
			hi = i
		}
	}
	if lo < 0 {
		return Range{}, StatusUnsatisfiable
	}
	return Range{lo, hi}, StatusPartial
}

func TestResolveExhaustive(t *testing.T) {
	const maxSize = 12
	n := 0
	check := func(hdr string, k Kind, a, b int64) {
		for size := int64(0); size <= maxSize; size++ {
			spec := Parse(hdr)
			if spec.Kind != k {
				t.Fatalf("Parse(%q).Kind = %d, want %d", hdr, spec.Kind, k)
			}
			got, gst := spec.Resolve(size)
			want, wst := ref(k, a, b, size)
			if gst != wst || (gst != StatusUnsatisfiable && got != want) {
				t.Errorf("%q size=%d: got %+v/%d, want %+v/%d", hdr, size, got, gst, want, wst)
			}
			n++
		}
	}
	for a := int64(0); a <= maxSize+2; a++ {
		check(fmt.Sprintf("bytes=%d-", a), KindFrom, a, 0)
		check(fmt.Sprintf("bytes=-%d", a), KindSuffix, a, 0)
		for b := a; b <= maxSize+2; b++ {
			check(fmt.Sprintf("bytes=%d-%d", a, b), KindBytes, a, b)
		}
	}
	check("", KindNone, 0, 0)
	check("bytes=0-1,3-4", KindMulti, 0, 0)
	t.Logf("%d cases", n)
}

func TestResolveHugeNumbers(t *testing.T) {
	r, st := Parse("bytes=0-99999999999999999999").Resolve(100)
	if st != StatusPartial || r != (Range{0, 99}) {
		t.Errorf("clamp: %+v %d", r, st)
	}
	if _, st = Parse("bytes=99999999999999999999-").Resolve(100); st != StatusUnsatisfiable {
		t.Errorf("huge start: %d", st)
	}
	r, st = Parse("bytes=-99999999999999999999").Resolve(100)
	if st != StatusPartial || r != (Range{0, 99}) {
		t.Errorf("huge suffix: %+v %d", r, st)
	}
}

func TestHint(t *testing.T) {
	for in, want := range map[string]int64{
		"": 0, "bytes=40-50": 40, "bytes=40-": 40, "bytes=-5": 0, "bytes=1-2,3-4": 0,
	} {
		if got := Parse(in).Hint(); got != want {
			t.Errorf("Hint(%q) = %d, want %d", in, got, want)
		}
	}
}

// Every byte of every range of every small object must land in exactly the
// chunk Index says, and Span/Within/Start/End must agree with that.
func TestChunkMathExhaustive(t *testing.T) {
	for slice := int64(1); slice <= 6; slice++ {
		for size := int64(1); size <= 20; size++ {
			if got, want := Count(size, slice), (size+slice-1)/slice; got != want {
				t.Fatalf("Count(%d,%d)=%d want %d", size, slice, got, want)
			}
			// chunks tile the object exactly
			var covered int64
			for i := int64(0); i < Count(size, slice); i++ {
				s, e := Start(i, slice), End(i, slice, size)
				if s != covered || e < s || e >= size || e-s+1 > slice {
					t.Fatalf("size=%d slice=%d chunk %d: [%d,%d] after %d", size, slice, i, s, e, covered)
				}
				covered = e + 1
			}
			if covered != size {
				t.Fatalf("size=%d slice=%d: chunks cover %d", size, slice, covered)
			}
			for a := int64(0); a < size; a++ {
				for b := a; b < size; b++ {
					r := Range{a, b}
					first, last := Span(r, slice)
					if first != Index(a, slice) || last != Index(b, slice) {
						t.Fatalf("Span(%v,%d) = %d,%d", r, slice, first, last)
					}
					var total int64
					for i := first; i <= last; i++ {
						from, to := Within(i, slice, r)
						if from < 0 || to <= from || to > slice {
							t.Fatalf("Within(%d,%d,%v) = %d,%d", i, slice, r, from, to)
						}
						// absolute bytes must be inside r and inside chunk i
						if abs := Start(i, slice) + from; abs < a || Start(i, slice)+to-1 > b {
							t.Fatalf("Within(%d,%d,%v) = %d,%d escapes range", i, slice, r, from, to)
						}
						total += to - from
					}
					if total != r.Len() {
						t.Fatalf("size=%d slice=%d r=%v: chunks give %d bytes, want %d", size, slice, r, total, r.Len())
					}
					// chunks outside the span contribute nothing
					if _, to := Within(first-1, slice, r); first > 0 && to != 0 {
						t.Fatalf("chunk before span overlaps")
					}
					if _, to := Within(last+1, slice, r); to != 0 {
						t.Fatalf("chunk after span overlaps")
					}
				}
			}
		}
	}
}

func TestContentRangeFormat(t *testing.T) {
	if got := string(AppendContentRange(nil, Range{2, 4}, 10)); got != "bytes 2-4/10" {
		t.Error(got)
	}
	if got := string(AppendUnsatisfiable(nil, 10)); got != "bytes */10" {
		t.Error(got)
	}
}

func TestParseContentRange(t *testing.T) {
	for _, tc := range []struct {
		in        string
		s, e, tot int64
		ok        bool
	}{
		{"bytes 0-9/100", 0, 9, 100, true},
		{"bytes 5-5/6", 5, 5, 6, true},
		{"BYTES 0-9/100", 0, 9, 100, true},
		{"bytes 0-9/*", 0, 9, -1, true},
		{"bytes */100", -1, -1, 100, true},
		{"bytes */0", -1, -1, 0, true},
		{"bytes */*", 0, 0, 0, false},
		{"bytes 9-0/100", 0, 0, 0, false},
		{"bytes 0-100/100", 0, 0, 0, false}, // end beyond total
		{"bytes 0-9", 0, 0, 0, false},
		{"bytes a-b/c", 0, 0, 0, false},
		{"items 0-9/100", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"bytes 0-9/99999999999999999999", 0, 0, 0, false},
	} {
		s, e, tot, ok := ParseContentRange(tc.in)
		if ok != tc.ok || (ok && (s != tc.s || e != tc.e || tot != tc.tot)) {
			t.Errorf("ParseContentRange(%q) = %d,%d,%d,%v want %d,%d,%d,%v", tc.in, s, e, tot, ok, tc.s, tc.e, tc.tot, tc.ok)
		}
	}
}

func TestNoAllocations(t *testing.T) {
	var sink int64
	n := testing.AllocsPerRun(100, func() {
		s := Parse("bytes=1048576-3145727")
		r, _ := s.Resolve(1 << 30)
		f, l := Span(r, 1<<20)
		from, to := Within(f, 1<<20, r)
		sink += f + l + from + to + End(l, 1<<20, 1<<30) + Count(1<<30, 1<<20)
		var buf [48]byte
		_ = AppendContentRange(buf[:0], r, 1<<30)
		_, _, _, _ = ParseContentRange("bytes 0-1048575/1073741824")
	})
	if n != 0 {
		t.Errorf("%v allocs/run, want 0", n)
	}
	_ = sink
}

var sinkR Range
var sinkI int64

func BenchmarkParse(b *testing.B) {
	for _, h := range []string{"bytes=1048576-3145727", "bytes=1048576-", "bytes=-4096", "bytes=0-1,5-9", ""} {
		b.Run(h, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				_ = Parse(h)
			}
		})
	}
}

func BenchmarkResolve(b *testing.B) {
	spec := Parse("bytes=1048576-3145727")
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sinkR, _ = spec.Resolve(1 << 30)
	}
}

func BenchmarkParseResolveSpan(b *testing.B) {
	b.ReportAllocs()
	var buf [64]byte
	for i := 0; i < b.N; i++ {
		r, _ := Parse("bytes=1048576-3145727").Resolve(1 << 30)
		first, last := Span(r, 1<<20)
		for c := first; c <= last; c++ {
			from, to := Within(c, 1<<20, r)
			sinkI += from + to
		}
		_ = AppendContentRange(buf[:0], r, 1<<30)
	}
}
