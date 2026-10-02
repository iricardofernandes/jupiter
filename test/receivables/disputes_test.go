//go:build integration

package receivables_test

import (
	"net/http"
	"testing"
	"time"

	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

const day = 24 * time.Hour

func (h *harness) dispute(intent, reasonCode string, amount int64) map[string]any {
	h.t.Helper()
	body := map[string]any{"payment_intent": intent, "reason_code": reasonCode}
	if amount > 0 {
		body["amount"] = amount
	}
	return h.ok(http.MethodPost, "/v1/test_helpers/disputes", body)
}

func (h *harness) getDispute(id string) map[string]any {
	h.t.Helper()
	return h.ok(http.MethodGet, "/v1/disputes/"+id, nil)
}

// advance moves the clock on and lets the worker's dispute pass run.
func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clock.Advance(d)
	if _, err := h.disputes.Advance(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) recover() int64 {
	h.t.Helper()
	n, err := h.receivables.Recover(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

// A seller sold R$ 600.00 in six installments, sold Jupiter
// every unit and was paid out, so a chargeback finds nothing in her balance and nothing
// of hers on her units. A bank then took a lien on her agenda. Her next sale's units go
// first to the bank's lien, and the rest is free; the chargeback is recovered from them
// in the Convenção's order: on each unit, what is free first, then the latest contract
// back, the bank's, and never Jupiter's own. Won in the end, the dispute gives it all
// back. The ledger, payments, receivables and disputes agree at every step.
func TestADisputeAfterEverythingWasAnticipated(t *testing.T) {
	h := newHarness(t)
	seller := h.bankSeller("123456", nil)
	sale := h.payWithSplit(seller)
	h.register()
	h.anticipate(seller, h.sellerUnits(seller)...)
	price := num(h.balance(seller), "available")
	po := h.ok(http.MethodPost, "/v1/payouts", map[string]any{"amount": price, "currency": "brl", "recipient": seller})
	h.resolve()
	if p := h.payout(str(po, "id")); str(p, "status") != "paid" {
		t.Fatalf("the payout: %v", p)
	}
	if b := h.balance(seller); num(b, "available") != 0 || num(b, "pending") != 0 {
		t.Fatalf("after anticipating everything and the payout: %v", b)
	}
	h.consistent()

	if err := h.registry.Accept(bankCNPJ, registryapi.Contract{
		ID: "trava-1", Holder: sellerCNPJ, Effect: string(registrysim.Lien), Rule: string(registrysim.Fixed), Amount: 20000,
		Domicile: registryapi.Domicile{ISPB: "33000167", Account: "loans"},
	}); err != nil {
		t.Fatal(err)
	}

	d := h.dispute(sale, "13.1", 0)
	if str(d, "status") != "needs_response" || str(d, "funds") != "withdrawn" || num(d, "amount") != 60000 || str(d, "reason") != "product_not_received" {
		t.Fatalf("the dispute: %v", d)
	}
	if b := h.balance(seller); num(b, "available") != -60000 {
		t.Fatalf("the liable seller bears it all: %v", b)
	}
	if own := h.balance("me"); num(own, "available") != 0 {
		t.Fatalf("the marketplace bears none of it: %v", own)
	}
	if got := h.recover(); got != 0 {
		t.Fatalf("recovered %d from units Jupiter bought", got)
	}
	h.consistent()

	// Her next sale: the same six dates, R$ 90.00 more on each unit. The bank's lien takes
	// R$ 200.00 of it, earliest first.
	h.payWithSplit(seller)
	h.register()
	if got := h.recover(); got != 54000 {
		t.Fatalf("recovered %d, want all R$ 540.00 of the new sale", got)
	}
	recoveries, err := h.receivables.Recoveries(t.Context(), h.pool, seller)
	if err != nil || len(recoveries) != 6 {
		t.Fatalf("recoveries: %+v, %v", recoveries, err)
	}
	want := []struct{ free, bank int64 }{{0, 9000}, {0, 9000}, {7000, 2000}, {9000, 0}, {9000, 0}, {9000, 0}}
	for i, r := range recoveries {
		var bank int64
		for _, x := range r.FromContracts {
			bank += x
		}
		if r.FromFree != want[i].free || bank != want[i].bank || r.Amount != 9000 {
			t.Fatalf("recovery %d: %+v, want %+v", i, r, want[i])
		}
	}
	h.register()
	positions, err := h.registry.Positions(registrysim.Participant{TaxID: jupiterCNPJ, Role: registrysim.Accreditor}, sellerCNPJ, "", "", nil)
	if err != nil || len(positions) != 6 {
		t.Fatalf("the registry: %+v, %v", positions, err)
	}
	for _, p := range positions {
		if p.Value != 9000 || p.Free != 0 || len(p.Committed) != 1 || p.Committed[0].Beneficiary != jupiterCNPJ || p.Committed[0].Amount != 9000 {
			t.Fatalf("a unit after the recovery: %+v", p)
		}
	}
	if b := h.balance(seller); num(b, "available") != -6000 || num(b, "pending") != 0 {
		t.Fatalf("what she still owes: %v", b)
	}
	if r := h.reconcile("daily"); len(r.Divergences) != 0 {
		t.Fatalf("daily reconciliation: %+v", r.Divergences)
	}
	h.consistent()

	d = h.ok(http.MethodPost, "/v1/disputes/"+str(d, "id"), map[string]any{
		"evidence": map[string]any{"shipping_tracking_number": "BR123", "uncategorized_text": "winning_evidence"}, "submit": true,
	})
	if str(d, "status") != "won" || str(d, "funds") != "reinstated" {
		t.Fatalf("the representment: %v", d)
	}
	if b := h.balance(seller); num(b, "available") != 54000 {
		t.Fatalf("won back: %v", b)
	}
	h.consistent()
}

// Deadlines move disputes on with no one acting, in accelerated time: a merchant that
// does not answer loses; evidence saved but not submitted is submitted at the deadline,
// and an issuer that does not answer it loses; a pre-arbitration left unanswered is lost;
// one escalated is ruled on by the network when its time is up.
func TestDeadlinesMoveDisputesOn(t *testing.T) {
	h := newHarness(t)
	silent := h.dispute(h.pay(10000, 1), "13.1", 0)
	saved := h.dispute(h.pay(20000, 1), "13.3", 0)
	h.ok(http.MethodPost, "/v1/disputes/"+str(saved, "id"), map[string]any{"evidence": map[string]any{"product_description": "As described"}})
	preArb := h.dispute(h.pay(30000, 1), "13.1", 0)
	h.ok(http.MethodPost, "/v1/disputes/"+str(preArb, "id"), map[string]any{"evidence": map[string]any{"uncategorized_text": "losing_evidence"}, "submit": true})
	escalated := h.dispute(h.pay(40000, 1), "13.1", 0)
	h.ok(http.MethodPost, "/v1/disputes/"+str(escalated, "id"), map[string]any{"evidence": map[string]any{"uncategorized_text": "losing_evidence"}, "submit": true})
	if d := h.getDispute(str(preArb, "id")); str(d, "stage") != "pre_arbitration" || str(d, "status") != "needs_response" {
		t.Fatalf("after losing evidence: %v", d)
	}
	h.ok(http.MethodPost, "/v1/disputes/"+str(escalated, "id"), map[string]any{"evidence": map[string]any{"uncategorized_text": "winning_evidence"}, "submit": true})
	if d := h.getDispute(str(escalated, "id")); str(d, "stage") != "arbitration" || str(d, "status") != "under_review" {
		t.Fatalf("escalated: %v", d)
	}
	h.register()
	due := num(silent, "due_by")
	if time.Unix(due, 0).Sub(h.clock.Now()).Round(time.Hour) != 28*day {
		t.Fatalf("the merchant has 28 days, two before Visa's 30: due %s", time.Unix(due, 0))
	}
	h.consistent()

	h.advance(28*day + time.Hour)
	for _, c := range []struct {
		id, status, stage, outcome string
	}{
		{str(silent, "id"), "lost", "chargeback", "deadline"},
		{str(saved, "id"), "under_review", "chargeback", ""},
		{str(preArb, "id"), "lost", "pre_arbitration", "pre_arbitration_deadline"},
		{str(escalated, "id"), "under_review", "arbitration", ""},
	} {
		if d := h.getDispute(c.id); str(d, "status") != c.status || str(d, "stage") != c.stage || str(d, "outcome") != c.outcome {
			t.Fatalf("28 days on, %s: %v", c.id, d)
		}
	}
	h.consistent()

	h.advance(31 * day)
	if d := h.getDispute(str(saved, "id")); str(d, "status") != "won" || str(d, "funds") != "reinstated" || str(d, "outcome") != "acquirer_won" {
		t.Fatalf("the issuer did not answer the representment: %v", d)
	}
	if d := h.getDispute(str(escalated, "id")); str(d, "status") != "won" {
		t.Fatalf("arbitration: %v", d)
	}
	h.consistent()
}

// A dispute opened more than 180 days after authorization is the scheme's: Jupiter takes
// nothing from the merchant and represents citing the cap, which wins.
func TestTheLiabilityCap(t *testing.T) {
	h := newHarness(t)
	intent := h.pay(10000, 1)
	h.register()
	h.clock.Advance(181 * day)
	d := h.dispute(intent, "10.4", 0)
	if str(d, "liability") != "scheme" || str(d, "funds") != "none" || str(d, "status") != "won" || str(d, "outcome") != "liability_cap" {
		t.Fatalf("a dispute past the cap: %v", d)
	}
	h.consistent()
}

// A disputed amount cannot be refunded; fraud reports count apart from disputes, and both
// make the month's ratio.
func TestRefundsFraudReportsAndTheRatio(t *testing.T) {
	h := newHarness(t)
	disputed := h.pay(10000, 1)
	reported := h.pay(10000, 1)
	h.pay(10000, 1)
	h.dispute(disputed, "13.1", 4000)
	if r := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": disputed, "amount": 7000}); r.status != http.StatusBadRequest {
		t.Fatalf("refunding the disputed amount: %d %s", r.status, r.raw)
	}
	h.ok(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": disputed, "amount": 6000})
	report := h.ok(http.MethodPost, "/v1/test_helpers/fraud_reports", map[string]any{"payment_intent": reported, "fraud_type": "card_not_present"})
	if str(report, "object") != "fraud_report" || num(report, "amount") != 10000 {
		t.Fatalf("the report: %v", report)
	}
	if r := h.call(http.MethodPost, "/v1/test_helpers/fraud_reports", map[string]any{"payment_intent": reported, "fraud_type": "stolen"}); r.status != http.StatusBadRequest {
		t.Fatalf("a second report: %d %s", r.status, r.raw)
	}
	m := h.ok(http.MethodGet, "/v1/dispute_monitoring", nil)
	if num(m, "transactions") != 3 || num(m, "disputes") != 1 || num(m, "fraud_reports") != 1 || num(m, "ratio_bps") != 6666 || m["excessive"] != false {
		t.Fatalf("the ratio: %v", m)
	}
	h.consistent()
}
