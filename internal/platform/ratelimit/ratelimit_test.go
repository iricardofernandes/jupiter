package ratelimit

import (
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestABucketRefills(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	b := New(Rate{PerSecond: 2, Burst: 3}, c.Now)
	for i := range 3 {
		if !b.Allow("k") {
			t.Fatalf("request %d of the burst was refused", i)
		}
	}
	if b.Allow("k") || !b.Spent("k") {
		t.Fatal("a fourth request at once went through")
	}
	if !b.Allow("other") {
		t.Fatal("another key shares the bucket")
	}
	c.now = c.now.Add(500 * time.Millisecond)
	if !b.Allow("k") || b.Allow("k") {
		t.Fatal("half a second at 2/s is one request")
	}
	if b.RetryAfter() != 1 {
		t.Fatalf("retry after %d", b.RetryAfter())
	}
}

func TestSpentTakesNothing(t *testing.T) {
	b := New(Rate{PerSecond: 1, Burst: 1}, nil)
	for range 5 {
		if b.Spent("k") {
			t.Fatal("checking took the token")
		}
	}
	if !b.Allow("k") || !b.Spent("k") {
		t.Fatal("the one token")
	}
}

func TestTheZeroRateAllowsEverything(t *testing.T) {
	var unset *Buckets
	b := New(Rate{}, nil)
	for range 100 {
		if !b.Allow("k") || !unset.Allow("k") {
			t.Fatal("refused without a rate")
		}
	}
}

func TestIdleBucketsAreForgotten(t *testing.T) {
	c := &clock{now: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)}
	b := New(Rate{PerSecond: 1, Burst: 1}, c.Now)
	for i := range maxBuckets {
		b.Allow(strconv.Itoa(i))
	}
	c.now = c.now.Add(2 * idleBucket)
	b.Allow("new")
	if n := len(b.buckets); n > 1 {
		t.Fatalf("%d buckets kept", n)
	}
}

func TestTheClientBehindProxies(t *testing.T) {
	trusted, err := ParseTrusted("10.0.0.0/8, 192.0.2.7")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		remote, forwarded string
		trusted           bool
		want              string
	}{
		{"203.0.113.9:4000", "", true, "203.0.113.9"},
		{"203.0.113.9:4000", "198.51.100.1", true, "203.0.113.9"},
		{"10.1.2.3:4000", "198.51.100.1, 203.0.113.50", true, "203.0.113.50"},
		{"10.1.2.3:4000", "198.51.100.1, 203.0.113.50, 192.0.2.7", true, "203.0.113.50"},
		{"10.1.2.3:4000", "198.51.100.1", false, "10.1.2.3"},
		{"[2001:db8:1:2:3:4:5:6]:4000", "", true, "2001:db8:1:2::/64"},
		{"[::ffff:203.0.113.9]:4000", "", true, "203.0.113.9"},
	} {
		r := httptest.NewRequestWithContext(t.Context(), "GET", "/", nil)
		r.RemoteAddr = tc.remote
		if tc.forwarded != "" {
			r.Header.Set("X-Forwarded-For", tc.forwarded)
		}
		c := Clients{}
		if tc.trusted {
			c.Trusted = trusted
		}
		if got := c.Of(r); got != tc.want {
			t.Errorf("%s via %q: %s, want %s", tc.remote, tc.forwarded, got, tc.want)
		}
	}
	if _, err := ParseTrusted("not an address"); err == nil {
		t.Fatal("a bad proxy was accepted")
	}
}
