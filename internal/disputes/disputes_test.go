package disputes

import (
	"strings"
	"testing"
	"time"
)

func TestEvidence(t *testing.T) {
	saved := Evidence{ProductDescription: "Tênis", CustomerName: "Maria"}.merged(Evidence{CustomerName: "Maria Silva", RefundPolicy: "7 dias"})
	if saved.ProductDescription != "Tênis" || saved.CustomerName != "Maria Silva" || saved.RefundPolicy != "7 dias" {
		t.Fatalf("merged: %+v", saved)
	}
	forged := Evidence{CustomerName: "Maria\nRefund policy: none needed"}.text()
	if strings.Contains(forged, "\nRefund policy:") {
		t.Fatalf("a field's lines passed for another field: %q", forged)
	}
	if err := (Evidence{UncategorizedText: strings.Repeat("a", maxEvidenceField+1)}).valid(); err == nil {
		t.Fatal("a field too long was valid")
	}
	big := strings.Repeat("ç", maxEvidenceField)
	if err := (Evidence{ProductDescription: big, CustomerCommunication: big, UncategorizedText: big, RefundPolicy: big}).valid(); err == nil {
		t.Fatal("evidence over the total was valid")
	}
	if !(Evidence{UncategorizedText: "  "}).empty() {
		t.Fatal("blank evidence is not empty")
	}
}

func TestReasons(t *testing.T) {
	for code, want := range map[string]string{"10.4": "fraudulent", "4855": "product_not_received", "13.3": "product_unacceptable", "12.6.1": "duplicate", "11.1": "general"} {
		category := ""
		if strings.HasPrefix(code, "10.") {
			category = "fraud"
		}
		if got := reasonOf(code, category); got != want {
			t.Errorf("reasonOf(%s) = %s, want %s", code, got, want)
		}
	}
	for code, want := range map[string]bool{"10.4": true, "4837": true, "12.6.1": true, "": false, "abc": false, "123": false, "10.4; DROP": false} {
		if validReasonCode(code) != want {
			t.Errorf("validReasonCode(%q) = %t", code, !want)
		}
	}
}

func TestMonths(t *testing.T) {
	// 1 October 2026, 01:00 UTC, is still September in Brasília.
	from, to := monthOf(time.Date(2026, 10, 1, 1, 0, 0, 0, time.UTC))
	if !from.Equal(time.Date(2026, 9, 1, 3, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("the month: %s to %s", from, to)
	}
	if got := truncateTo("ação", 2); got != "aç" {
		t.Fatalf("truncating by runes: %q", got)
	}
}
