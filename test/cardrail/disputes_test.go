//go:build integration

package cardrail_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

func (h *harness) read(path string) map[string]any {
	h.t.Helper()
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodGet, h.api.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+h.liveKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &out) != nil {
		h.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, raw)
	}
	return out
}

func (h *harness) disputeOf(intent string) map[string]any {
	h.t.Helper()
	list := h.read("/v1/disputes?payment_intent=" + intent)
	data, _ := list["data"].([]any)
	if len(data) != 1 {
		h.t.Fatalf("disputes of %s: %v", intent, list)
	}
	d, _ := data[0].(map[string]any)
	return d
}

func (h *harness) merchantBalance() int64 {
	h.t.Helper()
	b, err := h.payments.MerchantBalance(h.t.Context(), h.pool, h.owner, money.BRL)
	if err != nil {
		h.t.Fatal(err)
	}
	posted, err := b.Posted()
	if err != nil {
		h.t.Fatal(err)
	}
	return posted.Minor()
}

func (h *harness) checkDisputes() {
	h.t.Helper()
	violations, err := h.disputes.Check(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range violations {
		h.t.Errorf("disputes violation: %+v", v)
	}
}

// A chargeback through the network: the issuer opens it, Jupiter takes it from the
// merchant's balance (the merchant has no receivables), represents, the issuer rejects
// into pre-arbitration, Jupiter escalates, and the network rules for the merchant when its
// time is up. Every step reaches Jupiter as a signed event.
func TestAChargebackThroughTheNetwork(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 30000, "payment_method": h.saveCard("4242424242424242")})
	before := h.merchantBalance()
	ntid := h.attempt(str(it, "id")).NetworkTransactionID
	if _, err := h.network.OpenDispute(t.Context(), cardnetwork.DisputeParams{
		NetworkTransactionID: ntid, ReasonCode: "13.1", Issuer: cardnetwork.IssuerPreArbitration, Arbitration: "acquirer",
	}); err != nil {
		t.Fatal(err)
	}
	d := h.disputeOf(str(it, "id"))
	if str(d, "status") != "needs_response" || str(d, "funds") != "withdrawn" || str(d, "network") != "visa" {
		t.Fatalf("the chargeback: %v", d)
	}
	if after := h.merchantBalance(); after != before-30000 {
		t.Fatalf("the merchant's balance: %d, from %d", after, before)
	}
	h.consistent()
	h.checkDisputes()

	r := h.post("/v1/disputes/"+str(d, "id"), map[string]any{"evidence": map[string]any{"shipping_tracking_number": "BR987"}, "submit": true})
	if r.status != http.StatusOK || str(r.body, "stage") != "pre_arbitration" || str(r.body, "status") != "needs_response" {
		t.Fatalf("the representment: %d %s", r.status, r.raw)
	}
	r = h.post("/v1/disputes/"+str(d, "id"), map[string]any{"evidence": map[string]any{"uncategorized_text": "Delivered and signed for"}, "submit": true})
	if r.status != http.StatusOK || str(r.body, "stage") != "arbitration" || str(r.body, "status") != "under_review" {
		t.Fatalf("the escalation: %d %s", r.status, r.raw)
	}
	h.clock.Advance(31 * 24 * time.Hour)
	h.network.AdvanceDisputes(t.Context())
	if d = h.disputeOf(str(it, "id")); str(d, "status") != "won" || str(d, "funds") != "reinstated" {
		t.Fatalf("the ruling: %v", d)
	}
	if after := h.merchantBalance(); after != before {
		t.Fatalf("the merchant's balance after winning: %d, want %d", after, before)
	}
	h.consistent()
	h.checkDisputes()
}

// A chargeback whose event never arrived is found by reconciling with the network, and a
// fraud report arrives apart from any dispute.
func TestALostChargebackEventAndAFraudReport(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 12000, "payment_method": h.saveCard("4242424242424242")})
	ntid := h.attempt(str(it, "id")).NetworkTransactionID
	h.disputeEventsDown.Store(true)
	if _, err := h.network.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: ntid, ReasonCode: "4853", Amount: 5000}); err != nil {
		t.Fatal(err)
	}
	h.disputeEventsDown.Store(false)
	if list := h.read("/v1/disputes"); len(list["data"].([]any)) != 0 { //nolint:forcetypeassert // the API answers an array
		t.Fatalf("a dispute whose event was lost: %v", list)
	}
	if n, err := h.disputes.ReconcileCases(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("reconciling: %d, %v", n, err)
	}
	if d := h.disputeOf(str(it, "id")); num(d, "amount") != 5000 || str(d, "reason") != "product_unacceptable" {
		t.Fatalf("the dispute found: %v", d)
	}
	if _, err := h.network.ReportFraud(t.Context(), ntid, "card_not_present"); err != nil {
		t.Fatal(err)
	}
	reports := h.read("/v1/fraud_reports")
	data, _ := reports["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("fraud reports: %v", reports)
	}
	h.consistent()
	h.checkDisputes()
}

func num(m map[string]any, key string) int64 {
	f, _ := m[key].(float64)
	return int64(f)
}

// A dispute event is only a hint: one signed with the token events' secret is refused,
// and one that says a case closed when the network says otherwise changes nothing.
func TestDisputeEventsAreReadBack(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 8000, "payment_method": h.saveCard("4242424242424242")})
	d, err := h.network.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: h.attempt(str(it, "id")).NetworkTransactionID, ReasonCode: "13.1"})
	if err != nil {
		t.Fatal(err)
	}
	forged := d
	forged.Status, forged.Outcome, forged.Version = cardnet.DisputeClosed, cardnet.AcquirerWon, 1<<20
	body, _ := json.Marshal(cardnet.DisputeEvent{Type: "dispute.updated", Dispute: forged})
	for secret, want := range map[string]int{eventsSecret: http.StatusUnauthorized, disputeEventsSecret: http.StatusOK} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, h.api.URL+acquirer.DisputeEventsPath, bytes.NewReader(body))
		req.Header.Set(cardnet.EventSignatureHeader, cardnet.SignEvent(body, secret, h.clock.Now()))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("an event signed with %q: %d, want %d", secret, resp.StatusCode, want)
		}
	}
	if got := h.disputeOf(str(it, "id")); str(got, "status") != "needs_response" || str(got, "funds") != "withdrawn" {
		t.Fatalf("after a forged closing: %v", got)
	}
	h.consistent()
	h.checkDisputes()
}
