//go:build integration

package cardrail_test

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// A payment in six installments, authorized over ISO 8583, captured, partly refunded,
// and cleared by the network's file for the day.
func TestALivePaymentFromAuthorizationToClearing(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4242424242424242")
	_, it := h.pay(map[string]any{
		"amount": 60000, "payment_method": pm, "capture_method": "manual",
		"installments": map[string]any{"count": 6, "financed_by": "merchant"},
	})
	if it["status"] != "requires_capture" {
		t.Fatalf("after authorization: %v", it)
	}
	id := str(it, "id")
	a := h.attempt(id)
	if len(a.NetworkTransactionID) != 15 {
		t.Fatalf("network transaction id = %q", a.NetworkTransactionID)
	}
	if holds := h.network.Holds(); len(holds) != 1 || holds[0].Amount != 60000 {
		t.Fatalf("the issuer holds %+v", holds)
	}
	if r := h.post("/v1/payment_intents/"+id+"/capture", map[string]any{}); r.status != http.StatusOK || r.body["status"] != "succeeded" {
		t.Fatalf("capture: %d %s", r.status, r.raw)
	}
	refund := h.post("/v1/refunds", map[string]any{"payment_intent": id, "amount": 10000})
	if refund.status != http.StatusOK || refund.body["status"] != "succeeded" {
		t.Fatalf("refund: %d %s", refund.status, refund.raw)
	}
	h.consistent()

	today := h.clock.Now().UTC()
	if err := h.network.CloseDay(today); err != nil {
		t.Fatal(err)
	}
	report, err := h.connector.ImportClearing(t.Context(), today, h.payments)
	if err != nil || report.Records != 2 || report.Cleared != 2 || report.Exceptions != 0 {
		t.Fatalf("ImportClearing = %+v, %v", report, err)
	}
	a = h.attempt(id)
	if a.AmountCleared != 60000 || a.ClearedOn.IsZero() || a.Installments == nil || a.Installments.Count != 6 {
		t.Fatalf("after clearing: %+v", a)
	}
	var refundCleared bool
	if err := h.pool.QueryRow(t.Context(), "SELECT cleared_on IS NOT NULL FROM payments.refunds WHERE id = $1", refund.body["id"]).Scan(&refundCleared); err != nil || !refundCleared {
		t.Fatalf("the refund was not cleared: %v", err)
	}
	again, err := h.connector.ImportClearing(t.Context(), today, h.payments)
	if err != nil || !again.AlreadyImported {
		t.Fatalf("a second import = %+v, %v", again, err)
	}
	if _, err := h.connector.ImportClearing(t.Context(), today.AddDate(0, 0, 1), h.payments); !errors.Is(err, acquirer.ErrClearingNotReady) {
		t.Fatalf("an open day: %v", err)
	}
}

func TestIssuerDeclines(t *testing.T) {
	h := newHarness(t)
	for number, code := range map[string]string{
		"4000000000000002": "do_not_honor",
		"4000000000009995": "insufficient_funds",
		"4000000000000069": "expired_card",
	} {
		status, it := h.pay(map[string]any{"amount": 1000, "payment_method": h.saveCard(number)})
		if status != http.StatusPaymentRequired || it["status"] != "requires_payment_method" || declineCodeOf(it) != code {
			t.Errorf("%s: %d %v, want %s", number[12:], status, it, code)
		}
	}
	h.consistent()
}

// The issuer approves, the answer is lost: Jupiter reverses at once and declines, and
// the issuer's hold is released.
func TestALostAnswerIsReversed(t *testing.T) {
	h := newHarness(t)
	status, it := h.pay(map[string]any{"amount": 7000, "payment_method": h.saveCard("4000000000000119")})
	if status != http.StatusPaymentRequired || declineCodeOf(it) != "issuer_timeout" {
		t.Fatalf("%d %v", status, it)
	}
	if state, _ := h.exchange(str(it, "latest_attempt")); state != "reversed" {
		t.Fatalf("the authorization is %s, want reversed", state)
	}
	h.consistent()
}

// Phase 5 exit criterion: a late 0110 that arrives after the reversal was sent changes
// nothing: the payment stays declined and the issuer holds nothing.
func TestALateApprovalAfterTheReversal(t *testing.T) {
	h := newHarness(t)
	status, it := h.pay(map[string]any{"amount": 8000, "payment_method": h.saveCard("4000000000000127")})
	if status != http.StatusPaymentRequired || declineCodeOf(it) != "issuer_timeout" {
		t.Fatalf("%d %v", status, it)
	}
	key := str(it, "latest_attempt")
	deadline := time.Now().Add(5 * timeout)
	for {
		state, late := h.exchange(key)
		if late == cardnet.Approved {
			if state != "reversed" {
				t.Fatalf("after the late approval the authorization is %s", state)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the late approval never arrived")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.consistent()
}

func TestAnAnswerSentTwiceIsTakenOnce(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 3000, "payment_method": h.saveCard("4000000000000135")})
	if it["status"] != "succeeded" {
		t.Fatalf("%v", it)
	}
	time.Sleep(3 * timeout)
	if c := h.network.Completions(); len(c) != 1 {
		t.Fatalf("completions = %+v", c)
	}
	h.consistent()
}

func TestStandIn(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4000000000000150")
	if _, it := h.pay(map[string]any{"amount": 50000, "payment_method": pm}); it["status"] != "succeeded" {
		t.Fatalf("within the stand-in limit: %v", it)
	}
	if status, it := h.pay(map[string]any{"amount": 50001, "payment_method": pm}); status != http.StatusPaymentRequired || declineCodeOf(it) != "issuer_not_available" {
		t.Fatalf("over it: %d %v", status, it)
	}
}

// A capture advice whose acknowledgement is lost is repeated until the network
// acknowledges it, and the capture counts once.
func TestALostCaptureAcknowledgementIsRepeated(t *testing.T) {
	h := newHarness(t)
	var lost atomic.Bool
	h.setFaults(func(m cardnet.Message) cardnetwork.Fault {
		if m.MTI == cardnet.CompletionAdvice && lost.CompareAndSwap(false, true) {
			return cardnetwork.LoseAnswer
		}
		return cardnetwork.NoFault
	})
	_, it := h.pay(map[string]any{"amount": 9000, "payment_method": h.saveCard("4242424242424242")})
	if it["status"] != "processing" {
		t.Fatalf("with the capture unanswered: %v", it)
	}
	h.resolve()
	h.resolve()
	if a := h.attempt(str(it, "id")); a.Status != "captured" {
		t.Fatalf("the attempt is %s", a.Status)
	}
	if c := h.network.Completions(); len(c) != 1 || c[0].Completed != 9000 {
		t.Fatalf("completions = %+v", c)
	}
	h.consistent()
}

func TestCancelingReversesTheHold(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 4000, "payment_method": h.saveCard("5555555555554444"), "capture_method": "manual"})
	if r := h.post("/v1/payment_intents/"+str(it, "id")+"/cancel", map[string]any{}); r.status != http.StatusOK || r.body["status"] != "canceled" {
		t.Fatalf("cancel: %d %s", r.status, r.raw)
	}
	if holds := h.network.Holds(); len(holds) != 0 {
		t.Fatalf("holds after canceling: %+v", holds)
	}
	h.consistent()
}

func TestAMerchantInitiatedPaymentQuotesTheFirst(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4242424242424242")
	if _, it := h.pay(map[string]any{"amount": 1000, "payment_method": pm, "setup_future_usage": "off_session"}); it["status"] != "succeeded" {
		t.Fatalf("storing: %v", it)
	}
	_, it := h.pay(map[string]any{"amount": 2000, "payment_method": pm, "off_session": true})
	if it["status"] != "succeeded" {
		t.Fatalf("off session: %v", it)
	}
	if a := h.attempt(str(it, "id")); a.Initiator != "merchant" || a.NetworkTransactionID == "" {
		t.Fatalf("the merchant-initiated attempt: %+v", a)
	}
}

// An exchange left sending by a process that died is taken over as a timeout: the
// rail's query reverses it.
func TestAnAbandonedAuthorizationIsReversedOnQuery(t *testing.T) {
	h := newHarness(t)
	_, err := h.pool.Exec(t.Context(), `INSERT INTO acquirer.exchanges (key, kind, mti, stan, rrn, transmitted_at, amount,
		merchant_code, state, created_at, updated_at) VALUES ('pa_dead', 'authorize', '0100', '999999', '627499999999',
		'1001120000', 1500, 'M00000000000000', 'sending', now() - interval '1 hour', now() - interval '1 hour')`)
	if err != nil {
		t.Fatal(err)
	}
	if res := h.connector.Query(t.Context(), "pa_dead"); res.Outcome != "declined" || res.DeclineCode != "issuer_timeout" {
		t.Fatalf("Query = %+v", res)
	}
	if state, _ := h.exchange("pa_dead"); state != "reversed" {
		t.Fatalf("the abandoned authorization is %s", state)
	}
	if res := h.connector.Query(t.Context(), "pa_nothing"); res.Outcome != "not_found" {
		t.Fatalf("Query of an unknown key = %+v", res)
	}
}

func TestTheNetworkDown(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4242424242424242")
	h.network.Close()
	time.Sleep(100 * time.Millisecond)
	status, it := h.pay(map[string]any{"amount": 1000, "payment_method": pm})
	if status != http.StatusPaymentRequired {
		t.Fatalf("%d %v", status, it)
	}
	if code := declineCodeOf(it); code != "network_unavailable" && code != "issuer_timeout" {
		t.Fatalf("decline code %s", code)
	}
}
