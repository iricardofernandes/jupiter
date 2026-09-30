package server

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// limiter is a token bucket per client address for the public route: it bounds how fast
// one address can make tokens, and with them encrypted rows and security codes held in
// memory. An edge in front of the vault would add limits of its own.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
	now     func() time.Time
	seen    int
}

type bucket struct {
	tokens float64
	last   time.Time
}

const idleBucket = 10 * time.Minute

func newLimiter(perSecond float64, burst int, now func() time.Time) *limiter {
	return &limiter{buckets: map[string]*bucket{}, rate: perSecond, burst: float64(burst), now: now}
}

func (l *limiter) allow(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.forgetIdle(now)
	b, ok := l.buckets[host]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[host] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// forgetIdle drops buckets unused for a while, every thousand requests, so the map
// holds only recent addresses.
func (l *limiter) forgetIdle(now time.Time) {
	l.seen++
	if l.seen%1000 != 0 {
		return
	}
	for host, b := range l.buckets {
		if now.Sub(b.last) > idleBucket {
			delete(l.buckets, host)
		}
	}
}
