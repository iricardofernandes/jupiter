package events

import (
	"net/netip"
	"testing"
	"unicode/utf8"
)

func TestTruncateKeepsValidUTF8(t *testing.T) {
	s := "falha: conexão recusada"
	for n := range len(s) + 2 {
		got := truncate(s, n)
		if len(got) > n || !utf8.ValidString(got) {
			t.Fatalf("truncate(%q, %d) = %q", s, n, got)
		}
	}
}

func TestIsPublic(t *testing.T) {
	public := []string{"8.8.8.8", "2606:4700::1111", "1.1.1.1"}
	private := []string{
		"127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.169.254", "0.0.0.0",
		"100.64.0.1", "198.18.0.1", "240.0.0.1", "::1", "fe80::1", "fc00::1",
		"::ffff:127.0.0.1", "64:ff9b::a00:1", "2002:a00:1::1", "224.0.0.1",
	}
	for _, s := range public {
		if !isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s refused", s)
		}
	}
	for _, s := range private {
		if isPublic(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
}

// A merchant's deliveries in flight are bounded; another merchant's are not held by
// them, and a slot is free again once released.
func TestDeliveriesInFlightAreBoundedPerMerchant(t *testing.T) {
	f := &inFlight{running: map[string]int{}}
	for range maxInFlight {
		if !f.take("mch_a") {
			t.Fatal("a slot within the bound was refused")
		}
	}
	if f.take("mch_a") {
		t.Fatal("a delivery past the bound was let through")
	}
	if !f.take("mch_b") {
		t.Fatal("another merchant waited on the first")
	}
	f.release("mch_a")
	if !f.take("mch_a") {
		t.Fatal("a released slot was not free again")
	}
	for range maxInFlight {
		f.release("mch_a")
	}
	f.release("mch_b")
	if len(f.running) != 0 {
		t.Fatalf("merchants left counted: %v", f.running)
	}
}
