package server

import (
	"testing"
	"time"
)

func TestSecurityCodesAreGivenOnceAndExpire(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	c := newCVCs(time.Minute, 10, func() time.Time { return now })
	c.put("tok_a", "123", false)
	c.put("tok_b", "456", false)
	c.put("tok_c", "", false)
	if got := c.take("tok_a"); got != "123" {
		t.Fatalf("take = %q", got)
	}
	if got := c.take("tok_a"); got != "" {
		t.Fatalf("a code was given twice: %q", got)
	}
	if got := c.take("tok_c"); got != "" {
		t.Fatalf("an empty code was stored: %q", got)
	}
	c.put("tok_b", "789", false)
	now = now.Add(time.Minute)
	if got := c.take("tok_b"); got != "" {
		t.Fatalf("an expired code was given: %q", got)
	}
	c.put("tok_d", "111", false)
	now = now.Add(2 * time.Minute)
	if n := c.sweep(); n != 1 || len(c.entries) != 0 {
		t.Fatalf("sweep forgot %d, left %d", n, len(c.entries))
	}
}

func TestSecurityCodesAreBounded(t *testing.T) {
	c := newCVCs(time.Minute, 2, time.Now)
	c.put("tok_a", "111", false)
	c.put("tok_b", "222", false)
	c.put("tok_c", "333", false)
	c.put("tok_a", "444", false)
	if len(c.entries) != 2 || c.take("tok_c") != "" || c.take("tok_a") != "444" {
		t.Fatalf("entries = %d; a full store must keep what it has and take no more", len(c.entries))
	}
}

// Codes from web pages fill at most half the store: what is left is for the merchants'
// servers' cards.
func TestPublicCodesLeaveRoomForTheInternalPath(t *testing.T) {
	c := newCVCs(time.Minute, 4, time.Now)
	for _, token := range []string{"pub_a", "pub_b", "pub_c"} {
		c.put(token, "111", true)
	}
	if c.inUse != 2 {
		t.Fatalf("public codes hold %d places of 4", c.inUse)
	}
	if !c.put("tok_a", "444", false) || !c.put("tok_b", "555", false) || c.put("tok_c", "666", false) {
		t.Fatal("the internal path lacked its room, or overfilled the store")
	}
}

// A full public half lets its oldest code go for a new one, and the internal half is
// untouched.
func TestANewPublicCodeTakesTheOldestOnesPlace(t *testing.T) {
	c := newCVCs(time.Minute, 4, time.Now)
	c.put("tok_a", "900", false)
	for i, token := range []string{"pub_1", "pub_2", "pub_3", "pub_4"} {
		if !c.put(token, "11"+string(rune('0'+i)), true) {
			t.Fatalf("%s was not kept", token)
		}
	}
	if c.take("pub_1") != "" || c.take("pub_2") != "" || c.take("pub_3") != "112" || c.take("pub_4") != "113" {
		t.Fatal("the oldest public codes were not the ones let go")
	}
	if c.take("tok_a") != "900" {
		t.Fatal("a public code took an internal one's place")
	}
	if c.inUse != 0 || len(c.entries) != 0 {
		t.Fatalf("counts after all were taken: %d in use, %d entries", c.inUse, len(c.entries))
	}
}
