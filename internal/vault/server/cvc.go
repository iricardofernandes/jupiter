package server

import (
	"sync"
	"time"
)

// cvcs holds security codes in process memory, never on disk, until the one
// authorization that uses them takes them, or until they expire. PCI DSS forbids storing
// the code after authorization; not storing it at all is simpler. A vault restart loses
// the codes it held, and with several vault instances a code lives only on the one that
// tokenized the card: the authorization then goes without it.
type cvcs struct {
	mu      sync.Mutex
	entries map[string]cvcEntry
	ttl     time.Duration
	max     int
	now     func() time.Time
}

type cvcEntry struct {
	code    []byte
	expires time.Time
}

func newCVCs(ttl time.Duration, maxEntries int, now func() time.Time) *cvcs {
	return &cvcs{entries: map[string]cvcEntry{}, ttl: ttl, max: maxEntries, now: now}
}

func (c *cvcs) put(token, code string) {
	if code == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	old, ok := c.entries[token]
	if ok {
		clear(old.code)
	} else if len(c.entries) >= c.max {
		// Full: the card is kept without its code, and its authorization goes without.
		return
	}
	c.entries[token] = cvcEntry{code: []byte(code), expires: c.now().Add(c.ttl)}
}

// take returns the code once and forgets it.
func (c *cvcs) take(token string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[token]
	if !ok {
		return ""
	}
	delete(c.entries, token)
	defer clear(e.code)
	if !c.now().Before(e.expires) {
		return ""
	}
	return string(e.code)
}

// sweep forgets expired codes and returns how many it forgot.
func (c *cvcs) sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for token, e := range c.entries {
		if !now.Before(e.expires) {
			clear(e.code)
			delete(c.entries, token)
			n++
		}
	}
	return n
}
