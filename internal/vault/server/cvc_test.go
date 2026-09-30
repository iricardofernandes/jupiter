package server

import (
	"testing"
	"time"
)

func TestSecurityCodesAreGivenOnceAndExpire(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	c := newCVCs(time.Minute, 10, func() time.Time { return now })
	c.put("tok_a", "123")
	c.put("tok_b", "456")
	c.put("tok_c", "")
	if got := c.take("tok_a"); got != "123" {
		t.Fatalf("take = %q", got)
	}
	if got := c.take("tok_a"); got != "" {
		t.Fatalf("a code was given twice: %q", got)
	}
	if got := c.take("tok_c"); got != "" {
		t.Fatalf("an empty code was stored: %q", got)
	}
	c.put("tok_b", "789")
	now = now.Add(time.Minute)
	if got := c.take("tok_b"); got != "" {
		t.Fatalf("an expired code was given: %q", got)
	}
	c.put("tok_d", "111")
	now = now.Add(2 * time.Minute)
	if n := c.sweep(); n != 1 || len(c.entries) != 0 {
		t.Fatalf("sweep forgot %d, left %d", n, len(c.entries))
	}
}

func TestSecurityCodesAreBounded(t *testing.T) {
	c := newCVCs(time.Minute, 2, time.Now)
	c.put("tok_a", "111")
	c.put("tok_b", "222")
	c.put("tok_c", "333")
	c.put("tok_a", "444")
	if len(c.entries) != 2 || c.take("tok_c") != "" || c.take("tok_a") != "444" {
		t.Fatalf("entries = %d; a full store must keep what it has and take no more", len(c.entries))
	}
}
