//go:build integration

package cardrail_test

import (
	"slices"
	"testing"
	"time"

	pagination "github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// closeAndImport has the network close a day and Jupiter import its clearing file.
func (h *harness) closeAndImport(day time.Time) {
	h.t.Helper()
	if err := h.network.CloseDay(day); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.connector.ImportClearing(h.t.Context(), day, h.payments); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) clearingBreaks() []string {
	h.t.Helper()
	found, _, err := h.recon.Breaks(h.t.Context(), h.pool, true, "", "open", pagination.Request{Limit: 100})
	if err != nil {
		h.t.Fatal(err)
	}
	out := make([]string, 0, len(found))
	for _, b := range found {
		out = append(out, b.Kind+" "+b.Key)
	}
	slices.Sort(out)
	return out
}

// The network's clearing file loses a capture, lists another twice and holds a third
// for the next day's file: reconciliation finds each, and nothing else, and the held
// capture's break resolves the day its file lists it.
func TestTheClearingReconciles(t *testing.T) {
	h := newHarness(t)
	now := h.clock.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	if noon.Before(now) {
		noon = noon.AddDate(0, 0, 1)
	}
	h.clock.Advance(noon.Sub(now))
	h.mu.Lock()
	h.clearingFaults = func(r cardnet.ClearingRecord) cardnetwork.RecordFault {
		return cardnetwork.RecordFault{Drop: r.Amount == 1100, Duplicate: r.Amount == 1200, Delay: r.Amount == 1300}
	}
	h.mu.Unlock()
	card := h.saveCard("4242424242424242")
	rrns := map[int64]string{}
	for _, amount := range []int64{1000, 1100, 1200, 1300} {
		_, it := h.pay(map[string]any{"amount": amount, "payment_method": card})
		var rrn string
		if err := h.pool.QueryRow(t.Context(), "SELECT rrn FROM acquirer.exchanges WHERE kind = 'capture' AND authorization_key = $1",
			h.attempt(str(it, "id")).ID.String()).Scan(&rrn); err != nil {
			t.Fatal(err)
		}
		rrns[amount] = rrn
	}
	today := reconciliation.UTCDay(h.clock.Now())
	h.closeAndImport(today)
	if _, err := h.recon.Reconcile(t.Context(), true, today); err != nil {
		t.Fatal(err)
	}
	want := []string{"missing_at_counterparty " + rrns[1100], "duplicate " + rrns[1200], "missing_at_counterparty " + rrns[1300]}
	slices.Sort(want)
	if got := h.clearingBreaks(); !slices.Equal(got, want) {
		t.Fatalf("open breaks %v, want %v", got, want)
	}
	h.clock.Advance(24 * time.Hour)
	tomorrow := today.AddDate(0, 0, 1)
	h.closeAndImport(tomorrow)
	if _, err := h.recon.Reconcile(t.Context(), true, tomorrow); err != nil {
		t.Fatal(err)
	}
	want = []string{"missing_at_counterparty " + rrns[1100], "duplicate " + rrns[1200]}
	slices.Sort(want)
	if got := h.clearingBreaks(); !slices.Equal(got, want) {
		t.Fatalf("the next day, open breaks %v, want %v", got, want)
	}
}
