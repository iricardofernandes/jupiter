//go:build integration

package pixrail_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
)

func (h *harness) fund(amount int64) {
	h.t.Helper()
	it := h.charge(amount, nil)
	h.payQR(it)
	h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")
}

func payout(amount int64, key string) map[string]any {
	return map[string]any{"amount": amount, "currency": "brl", "destination": map[string]any{"type": "pix", "pix_key": key}}
}

func TestPayouts(t *testing.T) {
	h := newHarness(t)
	h.fund(10000)
	r := h.call(http.MethodPost, "/v1/payouts", payout(6000, sellerKey))
	if r.status != http.StatusOK || str(r.body, "status") != "paid" || str(obj(r.body, "destination"), "recipient_name") != "Vendedor da Silva" ||
		str(r.body, "end_to_end_id") == "" {
		t.Fatalf("a payout: %d %s", r.status, r.raw)
	}
	var sent bool
	for _, tr := range h.bank.Transfers() {
		sent = sent || (tr.Chave == sellerKey && tr.Valor == "60.00" && tr.Status == "REALIZADO")
	}
	if !sent {
		t.Fatalf("the bank sent %+v", h.bank.Transfers())
	}
	if over := h.call(http.MethodPost, "/v1/payouts", payout(4001, sellerKey)); over.status != http.StatusBadRequest ||
		str(obj(over.body, "error"), "code") != "balance_insufficient" {
		t.Fatalf("paying out more than the balance: %d %s", over.status, over.raw)
	}
	// The balance is paid out only to the merchant's own recipient's destination.
	for _, key := range []string{"ninguem@example.com", "not a key"} {
		if other := h.call(http.MethodPost, "/v1/payouts", payout(1000, key)); other.status != http.StatusBadRequest ||
			str(obj(other.body, "error"), "code") != "parameter_invalid" {
			t.Fatalf("to %s: %d %s", key, other.status, other.raw)
		}
	}

	// A new destination holds the balance's payouts until an operator lets them go.
	h.call(http.MethodPost, "/v1/recipients/me", map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": "ninguem@example.com"}})
	held := h.call(http.MethodPost, "/v1/payouts", map[string]any{"amount": 1000, "currency": "brl"})
	if str(held.body, "status") != "held" {
		t.Fatalf("after a new destination: %d %s", held.status, held.raw)
	}
	h.release()
	failed := h.call(http.MethodGet, "/v1/payouts/"+str(held.body, "id"), nil)
	if str(failed.body, "status") != "failed" || str(failed.body, "failure_code") != "pix_transfer_failed" {
		t.Fatalf("to a key nobody has: %d %s", failed.status, failed.raw)
	}
	// The failed payout gave its amount back.
	h.call(http.MethodPost, "/v1/recipients/me", map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": sellerKey}})
	again := h.call(http.MethodPost, "/v1/payouts", payout(4000, sellerKey))
	h.release()
	if got := h.call(http.MethodGet, "/v1/payouts/"+str(again.body, "id"), nil); str(got.body, "status") != "paid" {
		t.Fatalf("paying out the rest: %d %s", got.status, got.raw)
	}
	list := h.call(http.MethodGet, "/v1/payouts", nil)
	if data, _ := list.body["data"].([]any); len(data) != 3 {
		t.Fatalf("listing: %s", list.raw)
	}
	h.consistent()
}

// release is an operator lifting the hold on the merchant's own recipient's payouts; the
// resolver then sends them.
func (h *harness) release() {
	h.t.Helper()
	err := postgres.InTx(h.t.Context(), h.pool, func(tx pgx.Tx) error {
		me, err := h.recips.DefaultOf(h.t.Context(), tx, h.owner)
		if err != nil {
			return err
		}
		if err := h.recips.HoldPayouts(h.t.Context(), tx, me.ID.String(), false); err != nil {
			return err
		}
		_, _, err = h.payments.ReleaseHeldPayouts(h.t.Context(), tx, payments.Owner{Merchant: h.owner.Merchant, Livemode: true}, me.ID.String(), true, me.PayoutDestination())
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.payments.ResolvePayouts(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

// A transfer whose answer is lost is asked about, never sent twice.
func TestAPayoutWhoseAnswerIsLost(t *testing.T) {
	h := newHarness(t)
	h.fund(5000)
	h.setFaults(func(e pixsim.Event) pixsim.Fault { return pixsim.Fault{LoseResponse: e.Kind == "transfer"} })
	r := h.call(http.MethodPost, "/v1/payouts", payout(5000, sellerKey))
	if r.status != http.StatusOK || str(r.body, "status") != "pending" {
		t.Fatalf("without an answer: %d %s", r.status, r.raw)
	}
	h.setFaults(nil)
	h.clock.Advance(2 * 60e9)
	if n, err := h.payments.ResolvePayouts(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("ResolvePayouts = %d, %v", n, err)
	}
	if got := h.call(http.MethodGet, "/v1/payouts/"+str(r.body, "id"), nil); str(got.body, "status") != "paid" {
		t.Fatalf("after resolving: %s", got.raw)
	}
	if n := len(h.bank.Transfers()); n != 1 {
		t.Fatalf("the bank made %d transfers", n)
	}
	h.consistent()
}

// A refund not yet confirmed will take its amount from the balance: it cannot be paid
// out meanwhile.
func TestAPayoutWaitsForRefundsInFlight(t *testing.T) {
	h := newHarness(t)
	h.fund(5000)
	h.setFaults(func(e pixsim.Event) pixsim.Fault {
		return pixsim.Fault{Pending: e.Kind == "return", PendingFor: time.Hour}
	})
	it := h.call(http.MethodGet, "/v1/payment_intents?limit=1", nil)
	data, _ := it.body["data"].([]any)
	first, _ := data[0].(map[string]any)
	if r := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(first, "id"), "amount": 3000}); str(r.body, "status") != "pending" {
		t.Fatalf("the refund: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(2001, sellerKey)); r.status != http.StatusBadRequest {
		t.Fatalf("paying out what a refund will take: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(2000, sellerKey)); str(r.body, "status") != "paid" {
		t.Fatalf("paying out the rest: %d %s", r.status, r.raw)
	}
	h.consistent()
}

// A Pix's money reaches the balance at once and may be paid out; a refund after that
// has nothing to come from, and is refused rather than leave the balance below zero.
// A refund and a payout racing for the same money go one at a time.
func TestARefundNeedsTheBalance(t *testing.T) {
	h := newHarness(t)
	h.fund(5000)
	first := h.call(http.MethodGet, "/v1/payment_intents?limit=1", nil)
	data, _ := first.body["data"].([]any)
	paid, _ := data[0].(map[string]any)
	if r := h.call(http.MethodPost, "/v1/payouts", payout(5000, sellerKey)); str(r.body, "status") != "paid" {
		t.Fatalf("paying out everything: %d %s", r.status, r.raw)
	}
	r := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(paid, "id")})
	if r.status != http.StatusBadRequest || str(obj(r.body, "error"), "code") != "balance_insufficient" {
		t.Fatalf("refunding what was paid out: %d %s", r.status, r.raw)
	}

	h.fund(5000)
	again := h.call(http.MethodGet, "/v1/payment_intents?limit=1", nil)
	data, _ = again.body["data"].([]any)
	latest, _ := data[0].(map[string]any)
	var wg sync.WaitGroup
	var refund, out reply
	wg.Go(func() {
		refund = h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(latest, "id")})
	})
	wg.Go(func() { out = h.call(http.MethodPost, "/v1/payouts", payout(5000, sellerKey)) })
	wg.Wait()
	if (refund.status == http.StatusOK) == (out.status == http.StatusOK) {
		t.Fatalf("a refund and a payout of the same 5000: %d %s / %d %s", refund.status, refund.raw, out.status, out.raw)
	}
	h.consistent()
}

// What payouts asked for through the API take from a balance in a day is limited, and
// monitoring sees a balance near the limit and payouts held for an operator.
func TestTheDailyPayoutLimit(t *testing.T) {
	h := newHarness(t, func(c *payments.Config) { c.DailyPayoutLimit = 5000 })
	h.fund(10000)
	if r := h.call(http.MethodPost, "/v1/payouts", payout(3000, sellerKey)); str(r.body, "status") != "paid" {
		t.Fatalf("within the limit: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(2001, sellerKey)); r.status != http.StatusBadRequest ||
		str(obj(r.body, "error"), "code") != "payout_limit_reached" {
		t.Fatalf("past the limit: %d %s", r.status, r.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(2000, sellerKey)); str(r.body, "status") != "paid" {
		t.Fatalf("up to the limit: %d %s", r.status, r.raw)
	}
	h.call(http.MethodPost, "/v1/recipients/me", map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": "outra@example.com"}})
	h.clock.Advance(24 * time.Hour)
	if r := h.call(http.MethodPost, "/v1/payouts", map[string]any{"amount": 1000, "currency": "brl"}); str(r.body, "status") != "held" {
		t.Fatalf("the next day, to a new destination: %d %s", r.status, r.raw)
	}
	readings, err := h.payments.Health(t.Context(), h.pool)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range readings {
		got[r.Gauge] = r.Value
	}
	if got["jupiter.payouts.held"] != 1 || got["jupiter.payouts.balances_near_daily_limit"] != 0 {
		t.Fatalf("gauges the next day: %v", got)
	}
	h.clock.Advance(-24 * time.Hour)
	if readings, err = h.payments.Health(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	for _, r := range readings {
		if r.Gauge == "jupiter.payouts.balances_near_daily_limit" && r.Value != 1 {
			t.Fatalf("a balance at its limit: %d", r.Value)
		}
	}
}

// A payout held for a destination the merchant changed again before an operator
// confirmed it fails when the hold is lifted, its amount back in the balance: what a
// stolen key set never gets the money.
func TestAHeldPayoutToAnotherDestinationFails(t *testing.T) {
	h := newHarness(t)
	h.fund(5000)
	h.call(http.MethodPost, "/v1/recipients/me", map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": "ladrao@example.com"}})
	stolen := h.call(http.MethodPost, "/v1/payouts", map[string]any{"amount": 5000, "currency": "brl"})
	if str(stolen.body, "status") != "held" {
		t.Fatalf("to the new destination: %d %s", stolen.status, stolen.raw)
	}
	h.call(http.MethodPost, "/v1/recipients/me", map[string]any{"payout_destination": map[string]any{"type": "pix", "pix_key": sellerKey}})
	h.release()
	got := h.call(http.MethodGet, "/v1/payouts/"+str(stolen.body, "id"), nil)
	if str(got.body, "status") != "failed" || str(got.body, "failure_code") != "destination_changed" {
		t.Fatalf("after the hold was lifted: %s", got.raw)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(5000, sellerKey)); str(r.body, "status") != "paid" {
		t.Fatalf("the amount back in the balance: %d %s", r.status, r.raw)
	}
	for _, tr := range h.bank.Transfers() {
		if tr.Chave == "ladrao@example.com" {
			t.Fatalf("the bank paid the changed destination: %+v", tr)
		}
	}
	h.consistent()
}
