//go:build integration

package pixrail_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
)

func (h *harness) openBreaks() []string {
	h.t.Helper()
	found, _, err := h.recon.Breaks(h.t.Context(), h.pool, true, "", "open", page.Request{Limit: 100})
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

// The Pix bank's SPI statement loses a Pix received, lists another twice and puts a
// payout on the next day: reconciliation finds each, and nothing else, and the late line's
// break resolves the next day. A refund matches its return.
func TestTheSPIStatementReconciles(t *testing.T) {
	h := newHarness(t)
	// Noon in Brasília, so the day does not turn while the test runs.
	now := h.clock.Now()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 15, 0, 0, 0, time.UTC)
	if noon.Before(now) {
		noon = noon.AddDate(0, 0, 1)
	}
	h.clock.Advance(noon.Sub(now))
	var mu sync.Mutex
	credits := 0
	h.setFaults(func(e pixsim.Event) pixsim.Fault {
		if e.Kind != "statement" {
			return pixsim.Fault{}
		}
		if strings.HasPrefix(e.ID, "po") {
			return pixsim.Fault{Delay: true}
		}
		if !strings.HasPrefix(e.ID, "E") {
			return pixsim.Fault{}
		}
		mu.Lock()
		defer mu.Unlock()
		credits++
		return pixsim.Fault{Drop: credits == 1, Duplicate: credits == 2}
	})
	_, lost := h.paid(1000)
	_, twice := h.paid(2000)
	refunded, _ := h.paid(3000)
	refund := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": refunded, "amount": 500})
	h.waitFor("/v1/refunds/"+str(refund.body, "id"), "succeeded")
	po := h.call(http.MethodPost, "/v1/payouts", payout(1500, sellerKey))
	if str(po.body, "status") != "paid" {
		t.Fatalf("the payout: %d %s", po.status, po.raw)
	}
	payoutKey := strings.ReplaceAll(str(po.body, "id"), "_", "")
	today := reconciliation.Day(h.clock.Now())
	if _, err := h.recon.Reconcile(t.Context(), true, today); err != nil {
		t.Fatal(err)
	}
	want := []string{"missing_at_counterparty " + lost, "duplicate " + twice, "missing_at_counterparty " + payoutKey}
	slices.Sort(want)
	if got := h.openBreaks(); !slices.Equal(got, want) {
		t.Fatalf("open breaks %v, want %v", got, want)
	}
	h.clock.Advance(24 * time.Hour)
	if _, err := h.recon.Reconcile(t.Context(), true, today.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	want = []string{"missing_at_counterparty " + lost, "duplicate " + twice}
	slices.Sort(want)
	if got := h.openBreaks(); !slices.Equal(got, want) {
		t.Fatalf("the next day, open breaks %v, want %v", got, want)
	}
	h.consistent()
}

// A record a counterparty lists that cannot be matched opens a break of its own, once,
// and the reconciliation goes on: the next day is reconciled as any other.
func TestAnUnreadableRecordDoesNotStopTheReconciliation(t *testing.T) {
	h := newHarness(t)
	theirs := func(_ context.Context, _ *pgxpool.Pool, _ bool, day time.Time) ([]reconciliation.Record, error) {
		return []reconciliation.Record{{Identity: day.Format(time.DateOnly) + "/fee", Key: "", Direction: reconciliation.Out, Amount: 350, Date: day}}, nil
	}
	recon := reconciliation.New(reconciliation.Config{Pool: h.pool, Now: h.clock.Now, Live: reconciliation.Mode{Streams: []reconciliation.Stream{{
		Counterparty: "pix_bank", Name: "fees", Theirs: theirs,
	}}}})
	today := reconciliation.Day(h.clock.Now())
	first, err := recon.Reconcile(t.Context(), true, today.AddDate(0, 0, -1))
	if err != nil {
		t.Fatalf("a run with a record that cannot be matched: %v", err)
	}
	second, err := recon.Reconcile(t.Context(), true, today)
	if err != nil || !second.Through.After(first.Through) {
		t.Fatalf("the next run: %+v, %v; want it a day further", second, err)
	}
	breaks, _, err := recon.Breaks(t.Context(), h.pool, true, "", "open", page.Request{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	unreadable := 0
	for _, b := range breaks {
		if b.Kind == reconciliation.KindUnreadable {
			unreadable++
		}
	}
	// The first run read the week before its day, the second the days again and one
	// more: one break for each day's record, none twice.
	if unreadable != 9 {
		t.Fatalf("%d unreadable breaks, want one per day's record", unreadable)
	}
}
