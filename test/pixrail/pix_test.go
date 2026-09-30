//go:build integration

package pixrail_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/brcode"
)

func TestAPixPaymentSucceeds(t *testing.T) {
	h := newHarness(t)
	it := h.charge(12345, map[string]any{"description": "Pedido 42"})
	code, err := brcode.Parse(qrCode(it))
	if err != nil || code.Kind() != brcode.Dynamic {
		t.Fatalf("the QR code %q: %+v, %v", qrCode(it), code, err)
	}
	if expires := num(obj(obj(it, "next_action"), "pix_display_qr_code"), "expires_at"); expires < time.Now().Add(23*time.Hour).Unix() {
		t.Fatalf("expires at %d, want a day from now", expires)
	}
	res := h.payQR(it)
	if res.Refused != "" {
		t.Fatalf("paying: %+v", res)
	}
	paid := h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")
	if str(paid, "status") != "succeeded" || num(paid, "amount_received") != 12345 || paid["next_action"] != nil {
		t.Fatalf("after the payment: %v", paid)
	}
	var e2e string
	_ = h.pool.QueryRow(t.Context(), "SELECT network_transaction_id FROM payments.attempts WHERE id = $1", str(it, "latest_attempt")).Scan(&e2e)
	if e2e != res.EndToEndID {
		t.Fatalf("the attempt names Pix %q, the bank %q", e2e, res.EndToEndID)
	}
	h.consistent()
}

// The unknown-outcome path: a notification that never arrives. Reconciliation reads the
// Pix from the bank; a charge found paid at expiry is settled from GET /pix/{e2eid}.
func TestALostNotification(t *testing.T) {
	h := newHarness(t)
	h.setFaults(func(e pixsim.Event) pixsim.Fault { return pixsim.Fault{Drop: e.Kind == "webhook"} })
	first := h.charge(1000, nil)
	h.payQR(first)
	if got := h.intent(str(first, "id")); str(got, "status") != "requires_action" {
		t.Fatalf("without a notification: %v", got)
	}
	if _, err := h.connector.Reconcile(t.Context(), h.payments); err != nil {
		t.Fatal(err)
	}
	if got := h.intent(str(first, "id")); str(got, "status") != "succeeded" {
		t.Fatalf("after reconciling: %v", got)
	}

	second := h.charge(2000, map[string]any{"pix": map[string]any{"expires_after_seconds": 600}})
	h.payQR(second)
	h.clock.Advance(20 * time.Minute)
	if n, err := h.payments.ExpirePixCharges(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("ExpirePixCharges = %d, %v", n, err)
	}
	if got := h.intent(str(second, "id")); str(got, "status") != "succeeded" || num(got, "amount_received") != 2000 {
		t.Fatalf("a paid charge found at expiry: %v", got)
	}
	h.consistent()
}

func TestAnUnpaidChargeExpires(t *testing.T) {
	h := newHarness(t)
	it := h.charge(1000, map[string]any{"pix": map[string]any{"expires_after_seconds": 60}})
	h.clock.Advance(3 * time.Minute)
	if n, _ := h.payments.ExpirePixCharges(t.Context(), h.pool); n != 0 {
		t.Fatal("expired within the grace period")
	}
	h.clock.Advance(5 * time.Minute)
	if n, err := h.payments.ExpirePixCharges(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("ExpirePixCharges = %d, %v", n, err)
	}
	got := h.intent(str(it, "id"))
	if str(got, "status") != "requires_payment_method" || str(obj(got, "last_payment_error"), "code") != "payment_intent_payment_attempt_expired" ||
		got["next_action"] != nil {
		t.Fatalf("after expiry: %v", got)
	}
	if res := h.payQR(it); res.Refused == "" {
		t.Fatalf("an expired charge was paid: %+v", res)
	}
	// The customer tries again: a new attempt, a new charge.
	again := h.call(http.MethodPost, "/v1/payment_intents/"+str(it, "id")+"/confirm", map[string]any{})
	if str(again.body, "status") != "requires_action" || qrCode(again.body) == qrCode(it) {
		t.Fatalf("confirming again: %d %s", again.status, again.raw)
	}
	h.payQR(again.body)
	if got := h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded"); str(got, "status") != "succeeded" {
		t.Fatalf("paying the new charge: %v", got)
	}
	h.consistent()
}

func TestCancelingRemovesTheCharge(t *testing.T) {
	h := newHarness(t)
	it := h.charge(1000, nil)
	r := h.call(http.MethodPost, "/v1/payment_intents/"+str(it, "id")+"/cancel", map[string]any{})
	if str(r.body, "status") != "canceled" || r.body["next_action"] != nil {
		t.Fatalf("canceling: %d %s", r.status, r.raw)
	}
	if res := h.payQR(it); res.Refused == "" || !strings.Contains(res.Refused, "REMOVIDA") && !strings.Contains(res.Refused, "410") {
		t.Fatalf("a removed charge was paid: %+v", res)
	}
	h.consistent()
}

// A Pix that pays nothing Jupiter wants, a transfer to its key without a charge, is held
// apart and returned to the payer.
func TestAPixThatPaysNothingIsReturned(t *testing.T) {
	h := newHarness(t)
	res, err := h.bank.Pay(t.Context(), pixsim.Payment{Key: jupiterKey, Amount: "5.00", PayerTaxID: payerTaxID})
	if err != nil || res.Refused != "" {
		t.Fatalf("paying the key: %+v, %v", res, err)
	}
	if _, err := h.connector.Reconcile(t.Context(), h.payments); err != nil {
		t.Fatal(err)
	}
	var status string
	_ = h.pool.QueryRow(t.Context(), "SELECT status FROM payments.pix_received WHERE e2e_id = $1", res.EndToEndID).Scan(&status)
	if status != "unmatched" {
		t.Fatalf("the Pix is %q", status)
	}
	h.consistent()
	h.clock.Advance(2 * time.Minute)
	if _, err := h.payments.ReturnUnmatchedPix(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for status != "returned" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		_ = h.pool.QueryRow(t.Context(), "SELECT status FROM payments.pix_received WHERE e2e_id = $1", res.EndToEndID).Scan(&status)
	}
	if status != "returned" {
		t.Fatalf("the Pix is %q, want returned", status)
	}
	h.consistent()
}

func TestADueDateCharge(t *testing.T) {
	h := newHarness(t)
	due := h.clock.Now().In(time.FixedZone("BRT", -3*3600)).AddDate(0, 0, 5).Format(time.DateOnly)
	it := h.charge(10000, map[string]any{"pix": map[string]any{
		"due_date": due, "payer": map[string]any{"name": "Maria da Silva", "tax_id": payerTaxID},
		"fine": map[string]any{"percent": "2.00"}, "interest": map[string]any{"monthly_percent": "3.00"},
	}})
	if obj(it, "pix")["due_date"] != due {
		t.Fatalf("the options: %v", it["pix"])
	}
	h.clock.Advance(15 * 24 * time.Hour) // ten days late
	res := h.payQR(it)
	// 100.00 + 2% fine + 3% a month for 10 days (1.00).
	if res.Refused != "" || res.Amount != "103.00" {
		t.Fatalf("paying late: %+v", res)
	}
	paid := h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")
	if num(paid, "amount_received") != 10300 {
		t.Fatalf("received: %v", paid)
	}
	refund := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id")})
	if refund.status != http.StatusOK || num(refund.body, "amount") != 10300 {
		t.Fatalf("refunding everything received: %d %s", refund.status, refund.raw)
	}
	h.waitFor("/v1/refunds/"+str(refund.body, "id"), "succeeded")
	h.consistent()
}

func TestInvalidPixOptions(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]map[string]any{
		"usd":              {"currency": "usd"},
		"manual capture":   {"capture_method": "manual"},
		"installments":     {"installments": map[string]any{"count": 2, "financed_by": "merchant"}},
		"no payer":         {"pix": map[string]any{"due_date": time.Now().AddDate(0, 0, 3).Format(time.DateOnly)}},
		"bad tax id":       {"pix": map[string]any{"due_date": time.Now().AddDate(0, 0, 3).Format(time.DateOnly), "payer": map[string]any{"name": "A", "tax_id": "12345678900"}}},
		"fine alone":       {"pix": map[string]any{"fine": map[string]any{"percent": "2.00"}}},
		"short expiry":     {"pix": map[string]any{"expires_after_seconds": 10}},
		"options for card": {"payment_method": "pm_card_visa", "pix": map[string]any{"expires_after_seconds": 600}},
	} {
		req := map[string]any{"amount": 1000, "currency": "brl", "payment_method": "pix", "confirm": true}
		for k, v := range body {
			req[k] = v
		}
		if r := h.call(http.MethodPost, "/v1/payment_intents", req); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
}

// A Pix may arrive before Jupiter records its charge: the charge's answer was lost, and
// the customer paid a code they got anyway. It settles the attempt, found by its txid.
func TestAPixBeforeItsChargeIsRecorded(t *testing.T) {
	h := newHarness(t)
	h.setFaults(func(e pixsim.Event) pixsim.Fault { return pixsim.Fault{LoseResponse: e.Kind == "charge"} })
	r := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{"amount": 4200, "currency": "brl", "payment_method": "pix", "confirm": true})
	if str(r.body, "status") != "processing" {
		t.Fatalf("without the charge's answer: %d %s", r.status, r.raw)
	}
	h.setFaults(nil)
	charge, err := h.connector.Charge(t.Context(), payments.PixTxID(str(r.body, "latest_attempt")), false)
	if err != nil {
		t.Fatal(err)
	}
	if res, err := h.bank.Pay(t.Context(), pixsim.Payment{BRCode: charge.CopyPaste}); err != nil || res.Refused != "" {
		t.Fatalf("paying: %+v, %v", res, err)
	}
	if got := h.waitFor("/v1/payment_intents/"+str(r.body, "id"), "succeeded"); num(got, "amount_received") != 4200 {
		t.Fatalf("after the Pix: %v", got)
	}
	h.clock.Advance(2 * time.Minute)
	if _, err := h.payments.Resolve(t.Context(), h.pool); err != nil {
		t.Fatal(err)
	}
	if got := h.intent(str(r.body, "id")); str(got, "status") != "succeeded" {
		t.Fatalf("after resolving: %v", got)
	}
	h.consistent()
}
