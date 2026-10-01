//go:build integration

package pixrail_test

import (
	"net/http"
	"testing"
	"time"

	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/pkg/pixapi"
)

const day = 24 * time.Hour

// paid is a Pix payment that succeeded, and the end-to-end id of the Pix that paid it.
func (h *harness) paid(amount int64) (intent, e2e string) {
	h.t.Helper()
	it := h.charge(amount, nil)
	res := h.payQR(it)
	h.waitFor("/v1/payment_intents/"+str(it, "id"), "succeeded")
	return str(it, "id"), res.EndToEndID
}

// claim is the payer contesting a Pix in its bank's app, which reaches Jupiter's bank and
// then Jupiter.
func (h *harness) claim(e2e string, amount int64, upheld bool) {
	h.t.Helper()
	if _, err := h.bank.ReportInfraction(h.t.Context(), pixsim.InfractionParams{EndToEndID: e2e, Amount: amount, Details: "golpe", Upheld: upheld}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) claimOf(intent string) map[string]any {
	h.t.Helper()
	r := h.call(http.MethodGet, "/v1/disputes?payment_intent="+intent, nil)
	data, _ := r.body["data"].([]any)
	if r.status != http.StatusOK || len(data) != 1 {
		h.t.Fatalf("the claims of %s: %d %s", intent, r.status, r.raw)
	}
	d, _ := data[0].(map[string]any)
	return d
}

func (h *harness) advance(d time.Duration) {
	h.t.Helper()
	h.clock.Advance(d)
	if _, err := h.disputes.Advance(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

// A MED claim for more than the merchant still has: Jupiter holds what it has and traces
// the rest through the payout that took it. The merchant accepts; the bank returns what
// was held to the payer, of nature MED_FRAUDE.
func TestAMEDClaimAccepted(t *testing.T) {
	h := newHarness(t)
	intent, e2e := h.paid(10000)
	po := h.call(http.MethodPost, "/v1/payouts", payout(4000, sellerKey))
	if str(po.body, "status") != "paid" {
		t.Fatalf("the payout: %d %s", po.status, po.raw)
	}
	h.claim(e2e, 0, false)
	d := h.claimOf(intent)
	trace, _ := d["trace"].([]any)
	if str(d, "kind") != "med" || str(d, "status") != "needs_response" || num(d, "held") != 6000 || len(trace) != 1 {
		t.Fatalf("the claim: %v", d)
	}
	if hop, _ := trace[0].(map[string]any); str(hop, "end_to_end_id") != str(po.body, "end_to_end_id") || num(hop, "amount") != 4000 {
		t.Fatalf("the trace: %v", trace)
	}
	if r := h.call(http.MethodPost, "/v1/payouts", payout(100, sellerKey)); r.status != http.StatusBadRequest {
		t.Fatalf("paying out held money: %d %s", r.status, r.raw)
	}
	h.consistent()

	closed := h.call(http.MethodPost, "/v1/disputes/"+str(d, "id")+"/close", nil)
	if closed.status != http.StatusOK || str(closed.body, "status") != "lost" || str(closed.body, "funds") != "withdrawn" {
		t.Fatalf("accepting the claim: %d %s", closed.status, closed.raw)
	}
	reports := h.bank.Infractions()
	if len(reports) != 1 || reports[0].AnalysisResult != pixapi.AnalysisAgreed || reports[0].RefundStatus != "DEVOLVIDO" ||
		reports[0].RefundValor != "60.00" || len(reports[0].FundsTrace) != 1 || h.bank.Blocked(clientID) != 0 {
		t.Fatalf("the bank: %+v, blocked %d", reports, h.bank.Blocked(clientID))
	}
	h.consistent()
}

// Deadlines move claims on: one the merchant does not answer is agreed with after three
// days; one it answered, that no operator decides, is disagreed with when its block ends
// after eleven.
func TestMEDDeadlines(t *testing.T) {
	h := newHarness(t)
	quiet, quietE2E := h.paid(3000)
	answered, answeredE2E := h.paid(5000)
	h.claim(quietE2E, 0, false)
	h.claim(answeredE2E, 0, false)
	d := h.claimOf(answered)
	if r := h.call(http.MethodPost, "/v1/disputes/"+str(d, "id"), map[string]any{
		"evidence": map[string]any{"uncategorized_text": "O cliente recebeu o produto"}, "submit": true,
	}); r.status != http.StatusOK || str(r.body, "status") != "under_review" {
		t.Fatalf("answering: %d %s", r.status, r.raw)
	}
	h.advance(3*day + time.Hour)
	if d := h.claimOf(quiet); str(d, "status") != "lost" || str(d, "outcome") != "deadline" {
		t.Fatalf("unanswered: %v", d)
	}
	if d := h.claimOf(answered); str(d, "status") != "under_review" {
		t.Fatalf("answered, before the block ends: %v", d)
	}
	h.advance(8 * day)
	if d := h.claimOf(answered); str(d, "status") != "won" || str(d, "funds") != "released" || str(d, "outcome") != "block_lapsed" {
		t.Fatalf("when the block ends: %v", d)
	}
	h.consistent()
}

// A merchant whose money a MED claim returned contests the return; the payer's bank
// upholds it, and the money comes back.
func TestAContestedMEDReturn(t *testing.T) {
	h := newHarness(t)
	intent, e2e := h.paid(7000)
	h.claim(e2e, 0, true)
	d := h.claimOf(intent)
	h.call(http.MethodPost, "/v1/disputes/"+str(d, "id")+"/close", nil)
	r := h.call(http.MethodPost, "/v1/disputes/"+str(d, "id"), map[string]any{
		"evidence": map[string]any{"uncategorized_text": "Venda legítima, entregue"}, "submit": true,
	})
	if r.status != http.StatusOK || str(r.body, "stage") != "med_contestation" || str(r.body, "status") != "won" || str(r.body, "funds") != "reinstated" {
		t.Fatalf("contesting: %d %s", r.status, r.raw)
	}
	if again := h.call(http.MethodPost, "/v1/disputes/"+str(d, "id"), map[string]any{"submit": true, "evidence": map[string]any{"uncategorized_text": "de novo"}}); again.status != http.StatusBadRequest {
		t.Fatalf("contesting twice: %d %s", again.status, again.raw)
	}
	h.consistent()
}

// A claim whose notification was lost is found when Jupiter reads the bank's claims.
func TestALostMEDNotification(t *testing.T) {
	h := newHarness(t)
	intent, e2e := h.paid(2000)
	h.setFaults(func(e pixsim.Event) pixsim.Fault { return pixsim.Fault{Drop: e.Kind == "infraction"} })
	h.claim(e2e, 1500, false)
	if r := h.call(http.MethodGet, "/v1/disputes", nil); len(r.body["data"].([]any)) != 0 { //nolint:forcetypeassert // the API answers an array
		t.Fatalf("a claim whose notification was lost: %s", r.raw)
	}
	if n, err := h.disputes.ReconcileMED(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("reconciling: %d, %v", n, err)
	}
	if d := h.claimOf(intent); num(d, "held") != 1500 || num(d, "amount") != 1500 {
		t.Fatalf("the claim found: %v", d)
	}
	h.consistent()
}

// A claim Jupiter agreed with whose return is still being made when the block ends is not
// disagreed with: it is lost once the return is made.
func TestAnAgreedClaimWaitsForItsReturn(t *testing.T) {
	h := newHarness(t)
	intent, e2e := h.paid(4000)
	h.claim(e2e, 0, false)
	h.setFaults(func(e pixsim.Event) pixsim.Fault {
		return pixsim.Fault{Pending: e.Kind == "return", PendingFor: 12 * day}
	})
	d := h.claimOf(intent)
	if r := h.call(http.MethodPost, "/v1/disputes/"+str(d, "id")+"/close", nil); str(r.body, "status") != "under_review" {
		t.Fatalf("agreeing, the return pending: %d %s", r.status, r.raw)
	}
	h.advance(11*day + time.Hour)
	if d := h.claimOf(intent); str(d, "status") != "under_review" || str(d, "funds") != "held" {
		t.Fatalf("when the block ends, the return still pending: %v", d)
	}
	h.advance(2 * day)
	if d := h.claimOf(intent); str(d, "status") != "lost" || str(d, "funds") != "withdrawn" {
		t.Fatalf("once the return is made: %v", d)
	}
	h.consistent()
}
