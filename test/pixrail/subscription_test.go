//go:build integration

package pixrail_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
)

const (
	payerA = "12345678909"
	payerB = "11144477735"
)

func (h *harness) subscribe(extra map[string]any) map[string]any {
	h.t.Helper()
	body := map[string]any{
		"amount": 4990, "currency": "brl", "interval": "month", "description": "Plano mensal",
		"start_date": h.clock.Now().In(time.FixedZone("BRT", -3*3600)).AddDate(0, 0, 5).Format(time.DateOnly),
	}
	for k, v := range extra {
		body[k] = v
	}
	r := h.call(http.MethodPost, "/v1/subscriptions", body)
	if r.status != http.StatusOK || str(r.body, "status") != "incomplete" {
		h.t.Fatalf("subscribing: %d %s", r.status, r.raw)
	}
	return r.body
}

func (h *harness) subscription(id string) map[string]any {
	h.t.Helper()
	r := h.call(http.MethodGet, "/v1/subscriptions/"+id, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", id, r.status, r.raw)
	}
	return r.body
}

func cyclesOf(sub map[string]any) []map[string]any {
	raw, _ := sub["cycles"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, c := range raw {
		m, _ := c.(map[string]any)
		out = append(out, m)
	}
	return out
}

// charge reads a cycle's charge from the bank, through its payment intent's attempt.
func (h *harness) bankCharge(cycle map[string]any) payments.RecurringCharge {
	h.t.Helper()
	it := h.intent(str(cycle, "payment_intent"))
	c, err := h.connector.RecurringCharge(h.t.Context(), payments.PixTxID(str(it, "latest_attempt")))
	if err != nil {
		h.t.Fatalf("reading the charge of cycle %v: %v", cycle["number"], err)
	}
	return c
}

// day lets a day pass: the bank schedules, debits and expires, notifying Jupiter; the
// worker's jobs run.
func (h *harness) day() {
	h.t.Helper()
	h.clock.Advance(24 * time.Hour)
	h.bank.Tick(context.Background())
	if _, err := h.subs.Advance(h.t.Context(), h.pool); err != nil {
		h.t.Fatalf("advancing subscriptions: %v", err)
	}
	if _, err := h.payments.Resolve(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

// agree checks that every cycle's status, and its payment intent's, is what the bank
// says of its charge.
func (h *harness) agree(sub map[string]any) {
	h.t.Helper()
	for _, c := range cyclesOf(sub) {
		if str(c, "payment_intent") == "" {
			continue
		}
		charge := h.bankCharge(c)
		intent := str(h.intent(str(c, "payment_intent")), "status")
		want := map[string][2]string{
			payments.RecurringCreated: {"pending", "processing"}, payments.RecurringActive: {"pending", "processing"},
			payments.RecurringCompleted: {"paid", "succeeded"}, payments.RecurringExpired: {"failed", "requires_payment_method"},
			payments.RecurringRejected: {"failed", "requires_payment_method"}, payments.RecurringCanceled: {"canceled", "canceled"},
		}[charge.Status]
		if str(c, "status") != want[0] || intent != want[1] {
			h.t.Fatalf("%s cycle %v: the bank says %s, Jupiter %s with the payment intent %s", str(sub, "id"), c["number"],
				charge.Status, str(c, "status"), intent)
		}
	}
}

// A year of monthly Pix Automático charges in accelerated time,
// with a payer without funds who pays on a retry, one who never does, a cancellation by
// the payer and one by the receiver, and the ledger and every status matching the
// specification each day.
func TestAYearOfMonthlyCharges(t *testing.T) {
	h := newHarness(t)

	// A: authorized by a request to the customer's bank (journey 1), with retries.
	a := h.subscribe(map[string]any{
		"customer": map[string]any{"name": "Maria", "tax_id": payerA}, "retries": "three_in_seven",
		"authorization": map[string]any{"method": "payer_request", "payer_bank": map[string]any{"ispb": pixsim.PayerISPB, "account": "12345"}},
	})
	if str(obj(a, "next_action"), "type") != "awaiting_payer_authorization" {
		t.Fatalf("A: %v", a)
	}
	requests := h.bank.PendingRequests(payerA)
	if len(requests) != 1 {
		t.Fatalf("the payer's bank shows %v", requests)
	}
	if err := h.bank.DecideRequest(t.Context(), requests[0], true); err != nil {
		t.Fatal(err)
	}
	if got := h.subscription(str(a, "id")); str(got, "status") != "active" || len(cyclesOf(got)) != 1 {
		t.Fatalf("A once authorized: %v", got)
	}

	// B: authorized by reading its QR code (journey 2).
	b := h.subscribe(map[string]any{
		"customer": map[string]any{"name": "João", "tax_id": payerB}, "retries": "none", "authorization": map[string]any{"method": "qr_code"},
	})
	qr := str(obj(obj(b, "next_action"), "pix_display_qr_code"), "data")
	if res, err := h.bank.Pay(t.Context(), pixsim.Payment{BRCode: qr, PayerTaxID: payerB}); err != nil || res.Authorized == "" {
		t.Fatalf("B's customer reading the code: %+v, %v", res, err)
	}
	recB := h.bank.ApprovedRecurrences(payerB)
	if got := h.subscription(str(b, "id")); str(got, "status") != "active" || len(recB) != 1 {
		t.Fatalf("B once authorized: %v", got)
	}

	var sawPastDue, backToActive, canceledA, canceledB, fundedAgain2, fundedAgain4 bool
	for day := 1; day <= 420; day++ {
		subA, subB := h.subscription(str(a, "id")), h.subscription(str(b, "id"))
		cyclesA, cyclesB := cyclesOf(subA), cyclesOf(subB)
		// The third charge: no funds until a retry has been asked for.
		if len(cyclesA) == 3 && !fundedAgain2 {
			h.bank.SetPayerFunds(payerA, 0)
			if h.bankCharge(cyclesA[2]).Retries() >= 1 {
				h.bank.SetPayerFunds(payerA, -1)
				fundedAgain2 = true
			}
		}
		// The fifth: no funds through every retry.
		if len(cyclesA) == 5 && !fundedAgain4 {
			h.bank.SetPayerFunds(payerA, 0)
			if h.bankCharge(cyclesA[4]).Status == payments.RecurringExpired {
				h.bank.SetPayerFunds(payerA, -1)
				fundedAgain4 = true
			}
		}
		// B's customer cancels in their bank's app once the fourth charge is sent.
		if len(cyclesB) == 4 && !canceledB {
			if err := h.bank.CancelAsPayer(t.Context(), recB[0]); err != nil {
				t.Fatal(err)
			}
			canceledB = true
		}
		// The merchant cancels A once its tenth charge is sent.
		if len(cyclesA) == 10 && !canceledA {
			if r := h.call(http.MethodPost, "/v1/subscriptions/"+str(a, "id")+"/cancel", map[string]any{}); r.status != http.StatusOK ||
				str(r.body, "status") != "canceled" || str(r.body, "canceled_by") != "merchant" {
				t.Fatalf("canceling A: %d %s", r.status, r.raw)
			}
			canceledA = true
		}
		h.day()
		subA = h.subscription(str(a, "id"))
		switch str(subA, "status") {
		case "past_due":
			sawPastDue = true
		case "active":
			backToActive = backToActive || sawPastDue
		}
		h.agree(subA)
		h.agree(h.subscription(str(b, "id")))
		h.consistent()
		if t.Failed() {
			t.Fatalf("on day %d", day)
		}
	}

	subA, subB := h.subscription(str(a, "id")), h.subscription(str(b, "id"))
	statuses := func(sub map[string]any) string {
		var out []string
		for _, c := range cyclesOf(sub) {
			out = append(out, str(c, "status"))
		}
		return strings.Join(out, " ")
	}
	if got := statuses(subA); got != "paid paid paid paid failed paid paid paid paid canceled" {
		t.Errorf("A's cycles: %s", got)
	}
	if got := statuses(subB); got != "paid paid paid canceled" {
		t.Errorf("B's cycles: %s", got)
	}
	if str(subA, "status") != "canceled" || str(subA, "canceled_by") != "merchant" {
		t.Errorf("A: %s by %s", str(subA, "status"), str(subA, "canceled_by"))
	}
	if str(subB, "status") != "canceled" || str(subB, "canceled_by") != "customer" {
		t.Errorf("B: %s by %s", str(subB, "status"), str(subB, "canceled_by"))
	}
	if !sawPastDue || !backToActive {
		t.Errorf("A was past due %t, and active again %t", sawPastDue, backToActive)
	}
	if c := h.bankCharge(cyclesOf(subA)[2]); c.Retries() != 1 || c.Status != payments.RecurringCompleted {
		t.Errorf("the third charge: %d retries, %s", c.Retries(), c.Status)
	}
	if c := h.bankCharge(cyclesOf(subA)[4]); c.Retries() != 3 || c.Status != payments.RecurringExpired {
		t.Errorf("the fifth charge: %d retries, %s", c.Retries(), c.Status)
	}
	seen := map[string]bool{}
	for _, att := range h.bankCharge(cyclesOf(subA)[4]).Attempts {
		if seen[att.E2EID] {
			t.Errorf("two attempts with the endToEndId %s", att.E2EID)
		}
		seen[att.E2EID] = true
	}
}

func TestARejectedSubscription(t *testing.T) {
	h := newHarness(t)
	sub := h.subscribe(map[string]any{
		"customer":      map[string]any{"name": "Maria", "tax_id": payerA},
		"authorization": map[string]any{"method": "payer_request", "payer_bank": map[string]any{"ispb": pixsim.PayerISPB, "account": "12345"}},
	})
	if err := h.bank.DecideRequest(t.Context(), h.bank.PendingRequests(payerA)[0], false); err != nil {
		t.Fatal(err)
	}
	if got := h.subscription(str(sub, "id")); str(got, "status") != "rejected" || len(cyclesOf(got)) != 0 {
		t.Fatalf("after the customer refused: %v", got)
	}
}

func TestInvalidSubscriptions(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]map[string]any{
		"past start":      {"start_date": "2020-01-01"},
		"bad interval":    {"interval": "daily"},
		"long object":     {"description": "a description much longer than thirty-five characters"},
		"bad tax id":      {"customer": map[string]any{"name": "X", "tax_id": "12345678900"}},
		"request no bank": {"authorization": map[string]any{"method": "payer_request"}},
		"qr with a bank":  {"authorization": map[string]any{"method": "qr_code", "payer_bank": map[string]any{"ispb": "30000002", "account": "1"}}},
	} {
		req := map[string]any{
			"amount": 4990, "currency": "brl", "interval": "month", "description": "Plano",
			"start_date":    h.clock.Now().AddDate(0, 0, 5).Format(time.DateOnly),
			"customer":      map[string]any{"name": "Maria", "tax_id": payerA},
			"authorization": map[string]any{"method": "qr_code"},
		}
		for k, v := range body {
			req[k] = v
		}
		if r := h.call(http.MethodPost, "/v1/subscriptions", req); r.status != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, r.status, r.raw)
		}
	}
}

// A cycle that failed is the merchant's to collect another way, never by charging the
// customer's recurrence again.
func TestAFailedCycleIsNotChargedAgainByPix(t *testing.T) {
	h := newHarness(t)
	sub := h.subscribe(map[string]any{
		"customer": map[string]any{"name": "João", "tax_id": payerB}, "retries": "none", "authorization": map[string]any{"method": "qr_code"},
	})
	qr := str(obj(obj(sub, "next_action"), "pix_display_qr_code"), "data")
	if _, err := h.bank.Pay(t.Context(), pixsim.Payment{BRCode: qr, PayerTaxID: payerB}); err != nil {
		t.Fatal(err)
	}
	h.bank.SetPayerFunds(payerB, 0)
	var cycle map[string]any
	for range 10 {
		h.day()
		if cycles := cyclesOf(h.subscription(str(sub, "id"))); len(cycles) > 0 && str(cycles[0], "status") == "failed" {
			cycle = cycles[0]
			break
		}
	}
	if cycle == nil {
		t.Fatal("the first cycle did not fail")
	}
	// Updated to a plain Pix payment, it is an ordinary charge, of the amount asked, the
	// customer may choose to pay.
	intentID := str(cycle, "payment_intent")
	if r := h.call(http.MethodPost, "/v1/payment_intents/"+intentID, map[string]any{"payment_method": "pix"}); r.status != http.StatusOK {
		t.Fatalf("updating the failed cycle: %d %s", r.status, r.raw)
	}
	if c := h.call(http.MethodPost, "/v1/payment_intents/"+intentID+"/confirm", map[string]any{}); c.status != http.StatusOK ||
		obj(obj(c.body, "next_action"), "pix_display_qr_code") == nil {
		t.Fatalf("confirming it: %d %s", c.status, c.raw)
	}
	if recs := h.bank.ApprovedRecurrences(payerB); len(recs) != 1 || len(h.bank.RecurringCharges(recs[0])) != 1 {
		t.Fatal("a second charge was made under the recurrence")
	}
}

// A request the customer's bank cannot receive rejects the subscription.
func TestASubscriptionTheBankRefuses(t *testing.T) {
	h := newHarness(t)
	r := h.call(http.MethodPost, "/v1/subscriptions", map[string]any{
		"amount": 4990, "currency": "brl", "interval": "month", "description": "Plano",
		"start_date":    h.clock.Now().AddDate(0, 0, 5).Format(time.DateOnly),
		"customer":      map[string]any{"name": "Maria", "tax_id": payerA},
		"authorization": map[string]any{"method": "payer_request", "payer_bank": map[string]any{"ispb": "99999999", "account": "1"}},
	})
	if r.status != http.StatusOK || str(r.body, "status") != "rejected" {
		t.Fatalf("subscribing with a bank that does not exist: %d %s", r.status, r.raw)
	}
	if again := h.subscribe(map[string]any{
		"customer":      map[string]any{"name": "Maria", "tax_id": payerA},
		"authorization": map[string]any{"method": "qr_code"},
	}); str(again, "status") != "incomplete" {
		t.Fatalf("subscribing again: %v", again)
	}
	if r := h.call(http.MethodPost, "/v1/subscriptions", map[string]any{
		"amount": 4990, "currency": "brl", "interval": "month", "description": "Plano",
		"start_date": h.clock.Now().AddDate(0, 0, 5).Format(time.DateOnly),
		"customer":   map[string]any{"name": "Maria", "tax_id": payerA}, "authorization": map[string]any{"method": "qr_code"},
	}); r.status != http.StatusBadRequest || str(obj(r.body, "error"), "code") != "subscription_unexpected_state" {
		t.Fatalf("a second subscription waiting for the same customer: %d %s", r.status, r.raw)
	}
}
