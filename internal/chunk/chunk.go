// Package chunk holds the Range-header parsing and fixed-size-chunk math.
// Everything here is allocation-free: it runs on every request.
//
// Offsets are bytes; a Range is inclusive at both ends, like HTTP.
package chunk

import (
	"math"
	"strconv"
	"strings"
)

// Kind classifies a parsed Range header.
type Kind uint8

const (
	// KindNone: no Range header, or one we ignore (unknown unit, malformed,
	// or an invalid range such as "bytes=5-2"). Served as a full 200.
	KindNone Kind = iota
	// KindMulti: several ranges. Answered with the full object (200), which
	// RFC 9110 allows and which is always correct.
	KindMulti
	KindBytes  // "a-b"
	KindFrom   // "a-"
	KindSuffix // "-n"
)

// Spec is a parsed Range header, not yet resolved against an object size.
type Spec struct {
	Kind Kind
	A, B int64 // KindBytes: A-B. KindFrom: A. KindSuffix: A is the length.
}

// Range is an inclusive byte range. An empty object resolves to {0, -1}.
type Range struct{ Start, End int64 }

// Len is the number of bytes in r.
func (r Range) Len() int64 { return r.End - r.Start + 1 }

// Status is how a Spec resolves against a known object size.
type Status uint8

const (
	StatusFull          Status = iota // 200, whole object
	StatusPartial                     // 206, Range holds the slice
	StatusUnsatisfiable               // 416, Content-Range: bytes */size
)

// Parse parses a Range header value. It never fails: anything that is not a
// single valid byte range becomes KindNone or KindMulti, both of which
// resolve to the whole object.
func Parse(h string) Spec {
	h = strings.TrimSpace(h)
	if len(h) < 6 || !strings.EqualFold(h[:6], "bytes=") {
		return Spec{}
	}
	h = h[6:]
	if strings.IndexByte(h, ',') >= 0 {
		return Spec{Kind: KindMulti}
	}
	h = strings.TrimSpace(h)
	dash := strings.IndexByte(h, '-')
	if dash < 0 {
		return Spec{}
	}
	first, last := strings.TrimSpace(h[:dash]), strings.TrimSpace(h[dash+1:])
	switch {
	case first == "":
		n, ok := parseInt(last)
		if !ok {
			return Spec{}
		}
		return Spec{Kind: KindSuffix, A: n}
	case last == "":
		a, ok := parseInt(first)
		if !ok {
			return Spec{}
		}
		return Spec{Kind: KindFrom, A: a}
	}
	a, ok1 := parseInt(first)
	b, ok2 := parseInt(last)
	if !ok1 || !ok2 || b < a {
		return Spec{}
	}
	return Spec{Kind: KindBytes, A: a, B: b}
}

// parseInt parses a non-negative decimal. Values too large for int64
// saturate at MaxInt64 so that absurd ranges clamp or fail as unsatisfiable
// instead of wrapping around.
func parseInt(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	var v int64
	for i := 0; i < len(s); i++ {
		d := s[i] - '0'
		if d > 9 {
			return 0, false
		}
		if v > (math.MaxInt64-int64(d))/10 {
			v = math.MaxInt64
			// keep validating the remaining digits
			for i++; i < len(s); i++ {
				if s[i]-'0' > 9 {
					return 0, false
				}
			}
			return v, true
		}
		v = v*10 + int64(d)
	}
	return v, true
}

// Resolve applies s to an object of the given size.
func (s Spec) Resolve(size int64) (Range, Status) {
	switch s.Kind {
	case KindNone, KindMulti:
		return Range{0, size - 1}, StatusFull
	}
	if size <= 0 {
		return Range{}, StatusUnsatisfiable
	}
	switch s.Kind {
	case KindBytes:
		if s.A >= size {
			return Range{}, StatusUnsatisfiable
		}
		return Range{s.A, min(s.B, size-1)}, StatusPartial
	case KindFrom:
		if s.A >= size {
			return Range{}, StatusUnsatisfiable
		}
		return Range{s.A, size - 1}, StatusPartial
	default: // KindSuffix
		if s.A == 0 {
			return Range{}, StatusUnsatisfiable
		}
		return Range{max(0, size-s.A), size - 1}, StatusPartial
	}
}

// Hint is the offset to fetch first when the object size is not yet known:
// the start of the range when it has one, else 0 (suffix ranges need the size
// before they can be placed, so chunk 0 is fetched to learn it).
func (s Spec) Hint() int64 {
	if s.Kind == KindBytes || s.Kind == KindFrom {
		return s.A
	}
	return 0
}

// Index is the chunk holding byte offset off.
func Index(off, slice int64) int64 { return off / slice }

// Start is the first byte of chunk idx.
func Start(idx, slice int64) int64 { return idx * slice }

// End is the last byte of chunk idx, clamped to an object of the given size.
func End(idx, slice, size int64) int64 { return min(idx*slice+slice-1, size-1) }

// Count is the number of chunks in an object.
func Count(size, slice int64) int64 { return (size + slice - 1) / slice }

// Span is the inclusive chunk-index range covering r.
func Span(r Range, slice int64) (first, last int64) {
	return r.Start / slice, r.End / slice
}

// Within returns the part of chunk idx that r covers, as offsets from the
// start of the chunk: bytes [from, to). from == to when there is no overlap.
func Within(idx, slice int64, r Range) (from, to int64) {
	cs := idx * slice
	lo, hi := max(cs, r.Start), min(cs+slice-1, r.End)
	if hi < lo {
		return 0, 0
	}
	return lo - cs, hi - cs + 1
}

// AppendContentRange appends "bytes a-b/size" for a 206.
func AppendContentRange(b []byte, r Range, size int64) []byte {
	b = append(b, "bytes "...)
	b = strconv.AppendInt(b, r.Start, 10)
	b = append(b, '-')
	b = strconv.AppendInt(b, r.End, 10)
	b = append(b, '/')
	return strconv.AppendInt(b, size, 10)
}

// AppendUnsatisfiable appends "bytes */size" for a 416.
func AppendUnsatisfiable(b []byte, size int64) []byte {
	b = append(b, "bytes */"...)
	return strconv.AppendInt(b, size, 10)
}

// ParseContentRange parses an upstream "bytes a-b/total" header. total is -1
// when upstream sent "*"; for the unsatisfied form "bytes */total" start and
// end are -1.
func ParseContentRange(h string) (start, end, total int64, ok bool) {
	h = strings.TrimSpace(h)
	if len(h) < 6 || !strings.EqualFold(h[:6], "bytes ") {
		return 0, 0, 0, false
	}
	h = strings.TrimSpace(h[6:])
	slash := strings.IndexByte(h, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}
	rng, tot := h[:slash], h[slash+1:]
	total = -1
	if tot != "*" {
		var tok bool
		if total, tok = parseInt(tot); !tok || total == math.MaxInt64 {
			return 0, 0, 0, false
		}
	}
	if rng == "*" {
		if total < 0 {
			return 0, 0, 0, false
		}
		return -1, -1, total, true
	}
	dash := strings.IndexByte(rng, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	var ok1, ok2 bool
	start, ok1 = parseInt(rng[:dash])
	end, ok2 = parseInt(rng[dash+1:])
	if !ok1 || !ok2 || end < start || end == math.MaxInt64 || (total >= 0 && end >= total) {
		return 0, 0, 0, false
	}
	return start, end, total, true
}
