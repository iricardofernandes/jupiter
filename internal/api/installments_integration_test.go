//go:build integration

package api_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

func (h *harness) latestAttempt(intentID string) payments.Attempt {
	h.t.Helper()
	p, err := h.merchants.Authenticate(h.t.Context(), h.pool, h.keys["sk_test_"])
	if err != nil {
		h.t.Fatal(err)
	}
	parsed, err := payments.IntentPrefix.Parse(intentID)
	if err != nil {
		h.t.Fatal(err)
	}
	a, err := h.payments.LatestAttempt(h.t.Context(), h.pool, payments.Owner{Merchant: p.Merchant, Livemode: p.Livemode}, parsed)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func TestInstallments(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{
		"amount": 60000, "payment_method": payments.TestCardVisa, "confirm": true,
		"installments": map[string]any{"count": 6, "financed_by": "merchant"},
	}, http.StatusOK)
	if it.Installments == nil || it.Installments.Count != 6 || it.Installments.FinancedBy != "merchant" {
		t.Fatalf("installments = %+v", it.Installments)
	}
	if a := h.latestAttempt(it.Id); a.Installments == nil || a.Installments.Count != 6 || a.Installments.FinancedBy != payments.FinancedByMerchant {
		t.Fatalf("the attempt carries %+v", a.Installments)
	}
	updated := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa}, http.StatusOK)
	var got openapi.PaymentIntent
	h.expect(call{method: "POST", path: "/v1/payment_intents/" + updated.Id, body: map[string]any{
		"installments": map[string]any{"count": 3, "financed_by": "issuer"},
	}}, http.StatusOK).decode(t, &got)
	if got.Installments == nil || got.Installments.Count != 3 || got.Installments.FinancedBy != "issuer" {
		t.Fatalf("after update: %+v", got.Installments)
	}

	for name, body := range map[string]map[string]any{
		"one":           {"amount": 1000, "installments": map[string]any{"count": 1, "financed_by": "merchant"}},
		"thirteen":      {"amount": 1000, "installments": map[string]any{"count": 13, "financed_by": "merchant"}},
		"no financing":  {"amount": 1000, "installments": map[string]any{"count": 3}},
		"bad financing": {"amount": 1000, "installments": map[string]any{"count": 3, "financed_by": "bank"}},
		"in dollars":    {"amount": 1000, "currency": "usd", "installments": map[string]any{"count": 3, "financed_by": "issuer"}},
	} {
		if _, ok := body["currency"]; !ok {
			body["currency"] = "brl"
		}
		if resp := h.do(call{method: "POST", path: "/v1/payment_intents", body: body}); resp.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, resp.status, resp.body)
		}
	}
}

func TestAStoredCardPaysOffSession(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	pm := h.createMethod(cardBody("4242424242424242"), nil)

	notSetUp := h.do(call{method: "POST", path: "/v1/payment_intents", body: map[string]any{
		"amount": 1000, "currency": "brl", "payment_method": pm.Id, "confirm": true, "off_session": true,
	}})
	if notSetUp.status != http.StatusBadRequest || !strings.Contains(string(notSetUp.body), "set up for off-session") {
		t.Fatalf("an off-session payment on a card never stored: %d %s", notSetUp.status, notSetUp.body)
	}

	first := h.createIntent(map[string]any{
		"amount": 1000, "payment_method": pm.Id, "confirm": true, "setup_future_usage": "off_session",
	}, http.StatusOK)
	wantStatus(t, first, "succeeded")
	if first.SetupFutureUsage == nil || *first.SetupFutureUsage != "off_session" {
		t.Fatalf("setup_future_usage = %v", first.SetupFutureUsage)
	}
	cit := h.latestAttempt(first.Id)
	if cit.NetworkTransactionID == "" || cit.Initiator != "customer" {
		t.Fatalf("the storing payment: %+v", cit)
	}

	before := h.clock.Now()
	later := h.createIntent(map[string]any{
		"amount": 2500, "payment_method": pm.Id, "confirm": true, "off_session": true, "capture_method": "manual",
	}, http.StatusOK)
	wantStatus(t, later, "requires_capture")
	mit := h.latestAttempt(later.Id)
	if mit.Initiator != "merchant" {
		t.Fatalf("the off-session payment: %+v", mit)
	}
	// A merchant-initiated authorization lasts five days under Visa's rules, not ten.
	if valid := mit.AuthorizationExpiresAt.Sub(before); valid < 5*24*time.Hour || valid > 5*24*time.Hour+time.Minute {
		t.Fatalf("a merchant-initiated authorization is valid for %v, want five days", valid)
	}

	for name, body := range map[string]map[string]any{
		"off_session without confirm": {"amount": 1000, "payment_method": pm.Id, "off_session": true},
		"storing and off_session":     {"amount": 1000, "payment_method": pm.Id, "confirm": true, "off_session": true, "setup_future_usage": "off_session"},
		"off_session on a test card":  {"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true, "off_session": true},
		"storing a test card":         {"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true, "setup_future_usage": "off_session"},
	} {
		body["currency"] = "brl"
		if resp := h.do(call{method: "POST", path: "/v1/payment_intents", body: body}); resp.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, resp.status, resp.body)
		}
	}
	h.consistent()
}
