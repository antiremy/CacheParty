package ratelimit

import (
	"io"
	"net"
	"testing"
	"time"
)

// source returns the reading end of a connection whose other end writes
// n bytes and then closes.
func source(t *testing.T, n int) net.Conn {
	t.Helper()
	a, b := net.Pipe()
	go func() {
		defer b.Close()
		buf := make([]byte, 64<<10)
		for n > 0 {
			k, err := b.Write(buf[:min(n, len(buf))])
			if err != nil {
				return
			}
			n -= k
		}
	}()
	t.Cleanup(func() { a.Close() })
	return a
}

func drain(t *testing.T, c net.Conn) (int64, time.Duration) {
	t.Helper()
	start := time.Now()
	n, err := io.Copy(io.Discard, c)
	if err != nil {
		t.Fatal(err)
	}
	return n, time.Since(start)
}

func TestTakeAccountsDebtAndBurst(t *testing.T) {
	now := time.Unix(0, 0)
	l := &Limiter{now: func() time.Time { return now }}
	l.SetRate(1000)

	if d := l.take(500); d != 500*time.Millisecond {
		t.Fatalf("first take waits %v, want 500ms", d)
	}
	// The next caller queues behind the first one's debt.
	if d := l.take(500); d != time.Second {
		t.Fatalf("second take waits %v, want 1s", d)
	}
	// A long idle spell only builds up burstTime's worth of budget.
	now = now.Add(time.Hour)
	if d := l.take(50); d != 0 {
		t.Fatalf("take within burst waits %v", d)
	}
	if d := l.take(100); d != 100*time.Millisecond {
		t.Fatalf("take past burst waits %v, want 100ms", d)
	}
}

func TestReadsAreLimited(t *testing.T) {
	l := New(1 << 20)
	n, took := drain(t, l.Conn(source(t, 300<<10)))
	if n != 300<<10 {
		t.Fatalf("read %d bytes", n)
	}
	// 300 KiB at 1 MiB/s is ~293ms, less the 50ms burst.
	if took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("took %v, want about 250ms", took)
	}
}

func TestConnectionsShareTheLimit(t *testing.T) {
	l := New(1 << 20)
	done := make(chan time.Duration, 2)
	for range 2 {
		c := l.Conn(source(t, 150<<10))
		go func() { _, d := drain(t, c); done <- d }()
	}
	slowest := max(<-done, <-done)
	if slowest < 200*time.Millisecond {
		t.Fatalf("two connections finished in %v; the limit is not shared", slowest)
	}
}

func TestUnlimitedAndSetRate(t *testing.T) {
	l := New(0)
	if _, took := drain(t, l.Conn(source(t, 4<<20))); took > time.Second {
		t.Fatalf("unlimited read took %v", took)
	}
	l.SetRate(1 << 20)
	if _, took := drain(t, l.Conn(source(t, 300<<10))); took < 200*time.Millisecond {
		t.Fatalf("read after SetRate took %v; limit not applied", took)
	}
	l.SetRate(0)
	if _, took := drain(t, l.Conn(source(t, 4<<20))); took > time.Second {
		t.Fatalf("read after removing the limit took %v", took)
	}
}

func TestCloseEndsWait(t *testing.T) {
	l := New(1000) // a 32 KiB read waits about half a minute
	c := l.Conn(source(t, 64<<10))
	go func() {
		time.Sleep(50 * time.Millisecond)
		c.Close()
	}()
	start := time.Now()
	_, _ = c.Read(make([]byte, 64<<10))
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("Read kept waiting %v after Close", took)
	}
}
