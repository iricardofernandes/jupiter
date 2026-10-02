// Package ratelimit bounds how fast one client may call: a token bucket per key, an API
// key or a client's address, and the address a request comes from when proxies stand in
// front of the server. An edge in front adds limits of its own; these hold without one.
package ratelimit

import (
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Rate is how many requests a second a key may make, and how many at once.
type Rate struct {
	PerSecond float64
	Burst     int
}

const (
	idleBucket = 10 * time.Minute
	// maxBuckets bounds the memory a flood of new keys can take. Past it, keys not yet
	// seen share one overflow bucket until idle ones are forgotten, which is looked for
	// at most once a second: an edge, not this process, must stop a flood from that many
	// addresses.
	maxBuckets = 100_000
	overflow   = "\x00overflow"
)

// Buckets is a token bucket per key. The zero Rate allows everything.
type Buckets struct {
	rate    Rate
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
	seen    int
	swept   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(rate Rate, now func() time.Time) *Buckets {
	if now == nil {
		now = time.Now
	}
	return &Buckets{rate: rate, now: now, buckets: map[string]*bucket{}}
}

// Allow takes a token from key's bucket, and answers whether there was one.
func (b *Buckets) Allow(key string) bool {
	return b.take(key, true)
}

// Spent answers whether key's bucket is empty, without taking from it: what a caller
// checks before work it will only count if it fails.
func (b *Buckets) Spent(key string) bool {
	return !b.take(key, false)
}

func (b *Buckets) take(key string, consume bool) bool {
	if b == nil || b.rate.PerSecond <= 0 {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.forgetIdle(now, false)
	k, ok := b.buckets[key]
	if !ok {
		if !consume {
			return true // a key never seen has its whole burst
		}
		k = b.add(key, now)
	}
	k.tokens = min(float64(b.rate.Burst), k.tokens+now.Sub(k.last).Seconds()*b.rate.PerSecond)
	k.last = now
	if k.tokens < 1 {
		return false
	}
	if consume {
		k.tokens--
	}
	return true
}

// add makes key a bucket, or, past maxBuckets, gives it the overflow bucket.
func (b *Buckets) add(key string, now time.Time) *bucket {
	if len(b.buckets) >= maxBuckets && now.Sub(b.swept) >= time.Second {
		b.swept = now
		b.forgetIdle(now, true)
	}
	if len(b.buckets) >= maxBuckets {
		key = overflow
	}
	k, ok := b.buckets[key]
	if !ok {
		k = &bucket{tokens: float64(b.rate.Burst), last: now}
		b.buckets[key] = k
	}
	return k
}

// RetryAfter is how long until an empty bucket has a token again, in whole seconds.
func (b *Buckets) RetryAfter() int {
	if b == nil || b.rate.PerSecond <= 0 {
		return 0
	}
	return int(math.Ceil(1 / b.rate.PerSecond))
}

// forgetIdle drops buckets unused for a while, every thousand calls or when forced, so
// the map holds only recent keys.
func (b *Buckets) forgetIdle(now time.Time, force bool) {
	b.seen++
	if !force && b.seen%1000 != 0 {
		return
	}
	for key, k := range b.buckets {
		if now.Sub(k.last) > idleBucket {
			delete(b.buckets, key)
		}
	}
}

// Clients resolves the address a request comes from. Behind trusted proxies, it is the
// last address in X-Forwarded-For that is not one of them: the one the nearest proxy saw.
// An IPv6 client counts by its /64, which one subscriber is given whole.
type Clients struct {
	Trusted []netip.Prefix
}

// ParseTrusted reads a comma-separated list of addresses or CIDR prefixes.
func ParseTrusted(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, field := range strings.Split(list, ",") {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !strings.Contains(field, "/") {
			addr, err := netip.ParseAddr(field)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", field, err)
			}
			out = append(out, netip.PrefixFrom(addr.Unmap(), addr.Unmap().BitLen()))
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", field, err)
		}
		shortest := 32
		if prefix.Addr().Is4() {
			shortest = 8
		}
		if prefix.Bits() < shortest {
			return nil, fmt.Errorf("trusted proxy %q: a prefix shorter than /%d would let clients name themselves", field, shortest)
		}
		out = append(out, prefix.Masked())
	}
	return out, nil
}

// Of is the key of the client a request comes from.
func (c Clients) Of(r *http.Request) string {
	return Key(c.Address(r))
}

// Key is the key an address counts under: itself, or for IPv6 its /64.
func Key(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	if addr.Is6() {
		prefix, _ := addr.Prefix(64)
		return prefix.String()
	}
	return addr.String()
}

// Address is the address of the client a request comes from; the zero Addr when the
// connection's own cannot be read.
func (c Clients) Address(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	addr = addr.Unmap()
	if c.trusted(addr) {
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		for i := len(hops) - 1; i >= 0; i-- {
			hop, ok := parseHop(hops[i])
			if !ok {
				break
			}
			addr = hop
			if !c.trusted(addr) {
				break
			}
		}
	}
	return addr
}

// parseHop reads one X-Forwarded-For entry, which some proxies write with a port.
func parseHop(hop string) (netip.Addr, bool) {
	hop = strings.TrimSpace(hop)
	if addr, err := netip.ParseAddr(hop); err == nil {
		return addr.Unmap(), true
	}
	if addrPort, err := netip.ParseAddrPort(hop); err == nil {
		return addrPort.Addr().Unmap(), true
	}
	return netip.Addr{}, false
}

// Untrusted says a request was forwarded, by X-Forwarded-For, through a peer that is not
// a trusted proxy: every client behind that peer counts as the peer.
func (c Clients) Untrusted(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") == "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && !c.trusted(addr.Unmap())
}

func (c Clients) trusted(addr netip.Addr) bool {
	for _, p := range c.Trusted {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}
