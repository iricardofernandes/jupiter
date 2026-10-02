package server

import (
	"slices"
	"sync"
	"time"
)

// cvcs holds security codes in process memory, never on disk, until the one
// authorization that uses them takes them out of the map, or until they expire. PCI DSS forbids storing
// the code after authorization; not storing it at all is simpler. A vault restart loses
// the codes it held, and with several vault instances a code lives only on the one that
// tokenized the card: the authorization then goes without it.
//
// Codes from web pages, which anyone can send, may fill at most half the store, so that
// a flood of them cannot leave the merchants' servers' cards without theirs. When that
// half is full, a new one takes the place of the oldest: under a flood, a customer's code
// is kept for minutes, which is enough for its payment, rather than not at all.
type cvcs struct {
	mu      sync.Mutex
	entries map[string]cvcEntry
	// public holds the tokens of codes from web pages, oldest first; some may have been
	// taken already.
	public []string
	inUse  int
	ttl    time.Duration
	max    int
	now    func() time.Time
}

type cvcEntry struct {
	code    []byte
	expires time.Time
	public  bool
}

func newCVCs(ttl time.Duration, maxEntries int, now func() time.Time) *cvcs {
	return &cvcs{entries: map[string]cvcEntry{}, ttl: ttl, max: maxEntries, now: now}
}

// put holds a code, and answers false when the store is full and it could not.
func (c *cvcs) put(token, code string, public bool) bool {
	if code == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[token]; ok {
		c.forget(token, old)
	}
	if public {
		for c.inUse >= c.max/2 && len(c.public) > 0 {
			oldest := c.public[0]
			c.public = c.public[1:]
			if e, ok := c.entries[oldest]; ok && e.public {
				c.forget(oldest, e)
			}
		}
	}
	if len(c.entries) >= c.max || (public && c.inUse >= c.max/2) {
		return false
	}
	c.entries[token] = cvcEntry{code: []byte(code), expires: c.now().Add(c.ttl), public: public}
	if public {
		c.inUse++
		c.public = append(c.public, token)
		if len(c.public) > c.max {
			c.public = slices.DeleteFunc(c.public, func(t string) bool { return !c.entries[t].public })
		}
	}
	return true
}

func (c *cvcs) forget(token string, e cvcEntry) {
	clear(e.code)
	delete(c.entries, token)
	if e.public {
		c.inUse--
	}
}

// take returns the code once and forgets it.
func (c *cvcs) take(token string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[token]
	if !ok {
		return ""
	}
	code := string(e.code)
	c.forget(token, e)
	if !c.now().Before(e.expires) {
		return ""
	}
	return code
}

// sweep forgets expired codes and returns how many it forgot.
func (c *cvcs) sweep() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	n := 0
	for token, e := range c.entries {
		if !now.Before(e.expires) {
			c.forget(token, e)
			n++
		}
	}
	return n
}
