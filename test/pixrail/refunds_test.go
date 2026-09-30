//go:build integration

package pixrail_test

import (
	"net/http"
	"testing"
)

// Refunds of a Pix payment are returns (devoluções) of the Pix that paid it, partial or
// whole, confirmed by the bank's notification.
func TestRefundsAreReturns(t *testing.T) {
	h := newHarness(t)
	it := h.charge(10000, nil)
	h.payQR(it)
	h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")

	partial := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id"), "amount": 3000})
	if partial.status != http.StatusOK {
		t.Fatalf("a partial refund: %d %s", partial.status, partial.raw)
	}
	if got := h.waitFor("/v1/refunds/"+str(partial.body, "id"), "succeeded"); str(got, "status") != "succeeded" {
		t.Fatalf("after the bank returned it: %v", got)
	}
	if over := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id"), "amount": 7001}); over.status != http.StatusBadRequest {
		t.Fatalf("refunding more than is left: %d %s", over.status, over.raw)
	}
	rest := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id")})
	h.waitFor("/v1/refunds/"+str(rest.body, "id"), "succeeded")
	if got := h.intent(str(it, "id")); num(got, "amount_refunded") != 10000 {
		t.Fatalf("refunded: %v", got)
	}
	h.consistent()
}

func TestAReturnTheBankCannotMake(t *testing.T) {
	h := newHarness(t)
	it := h.charge(5000, nil)
	h.payQR(it)
	h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")
	h.bank.ClosePayer(payerTaxID)
	refund := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id")})
	got := h.waitFor("/v1/refunds/"+str(refund.body, "id"), "failed")
	if str(got, "status") != "failed" || str(got, "failure_reason") != "pix_return_failed" {
		t.Fatalf("a return to a closed account: %v", got)
	}
	if got := h.intent(str(it, "id")); num(got, "amount_refunded") != 0 {
		t.Fatalf("refunded: %v", got)
	}
	h.consistent()
}
