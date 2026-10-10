// Package ratelimit caps the combined download rate of upstream connections.
//
// Every connection to the internet (cache misses, bypassed requests and the
// :443 passthrough) is dialed through the resolver; wrapping those
// connections with Limiter.Conn makes them share one budget. Only reads are
// limited: what comes in from the internet is what saturates the line. A
// throttled read leaves data in the kernel's receive buffer, so TCP flow
// control slows the sender rather than anything being dropped.
package ratelimit

import (
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxRead caps one limited read so a wait stays short and many
	// connections share the budget in small, interleaved slices.
	maxRead = 32 << 10
	// burstTime is how much unused budget may build up while idle.
	burstTime = 50 * time.Millisecond
)

// Limiter is a token bucket shared by every connection it wraps. The zero
// value is unlimited.
type Limiter struct {
	rate atomic.Int64 // bytes per second; 0 is unlimited

	mu     sync.Mutex
	tokens float64 // may go negative: a debt that later reads wait out
	last   time.Time
	now    func() time.Time // time.Now; replaced in tests
}

// New returns a Limiter allowing bytesPerSec; 0 means unlimited.
func New(bytesPerSec int64) *Limiter {
	l := &Limiter{now: time.Now}
	l.SetRate(bytesPerSec)
	return l
}

// SetRate changes the limit while connections are open; 0 removes it.
func (l *Limiter) SetRate(bytesPerSec int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens, l.last = 0, l.now()
	l.rate.Store(max(bytesPerSec, 0))
}

// take spends n bytes of budget and returns how long the caller must wait
// before using them. Callers queue in the order they take: each one's wait
// covers the debt left by those before it.
func (l *Limiter) take(n int) time.Duration {
	rate := float64(l.rate.Load())
	if rate == 0 {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.tokens = min(l.tokens+now.Sub(l.last).Seconds()*rate, rate*burstTime.Seconds())
	l.last = now
	l.tokens -= float64(n)
	if l.tokens >= 0 {
		return 0
	}
	return time.Duration(-l.tokens / rate * float64(time.Second))
}

// Conn wraps c so its reads count against the limit.
func (l *Limiter) Conn(c net.Conn) net.Conn {
	return &conn{Conn: c, lim: l, closed: make(chan struct{})}
}

type conn struct {
	net.Conn
	lim *Limiter

	closeOnce sync.Once
	closed    chan struct{} // ends a wait when the connection is closed
}

func (c *conn) Read(p []byte) (int, error) {
	if c.lim.rate.Load() == 0 {
		return c.Conn.Read(p)
	}
	if len(p) > maxRead {
		p = p[:maxRead]
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		// Wait after reading, once the real size is known; the bytes stay
		// with us meanwhile, and the sender is held back by the full buffer.
		if d := c.lim.take(n); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-t.C:
			case <-c.closed:
				t.Stop()
			}
		}
	}
	return n, err
}

func (c *conn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// CloseWrite passes a half-close through, which the :443 relay relies on.
func (c *conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}
