//go:build integration

package pixrail_test

import (
	"net/http"
	"testing"
	"time"

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
	failed := h.call(http.MethodPost, "/v1/payouts", payout(1000, "ninguem@example.com"))
	if str(failed.body, "status") != "failed" || str(failed.body, "failure_code") != "pix_transfer_failed" {
		t.Fatalf("to a key nobody has: %d %s", failed.status, failed.raw)
	}
	// The failed payout gave its amount back.
	if again := h.call(http.MethodPost, "/v1/payouts", payout(4000, sellerKey)); str(again.body, "status") != "paid" {
		t.Fatalf("paying out the rest: %d %s", again.status, again.raw)
	}
	if bad := h.call(http.MethodPost, "/v1/payouts", payout(100, "not a key")); bad.status != http.StatusBadRequest {
		t.Fatalf("an invalid key: %d %s", bad.status, bad.raw)
	}
	list := h.call(http.MethodGet, "/v1/payouts", nil)
	if data, _ := list.body["data"].([]any); len(data) != 3 {
		t.Fatalf("listing: %s", list.raw)
	}
	h.consistent()
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
