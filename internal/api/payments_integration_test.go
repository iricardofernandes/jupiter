//go:build integration

package api_test

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

func (h *harness) intent(resp response) openapi.PaymentIntent {
	h.t.Helper()
	var it openapi.PaymentIntent
	resp.decode(h.t, &it)
	return it
}

func (h *harness) createIntent(body map[string]any, status int) openapi.PaymentIntent {
	h.t.Helper()
	if _, ok := body["currency"]; !ok {
		body["currency"] = "brl"
	}
	resp := h.expect(call{method: "POST", path: "/v1/payment_intents", body: body}, status)
	if status == http.StatusPaymentRequired {
		var e openapi.ErrorResponse
		resp.decode(h.t, &e)
		return *e.Error.PaymentIntent
	}
	conforms(h.t, "PaymentIntent", resp.body)
	return h.intent(resp)
}

func (h *harness) post(path string, body any, status int) openapi.PaymentIntent {
	h.t.Helper()
	resp := h.expect(call{method: "POST", path: path, body: body}, status)
	if status == http.StatusPaymentRequired {
		var e openapi.ErrorResponse
		resp.decode(h.t, &e)
		return *e.Error.PaymentIntent
	}
	if status != http.StatusOK {
		return openapi.PaymentIntent{}
	}
	return h.intent(resp)
}

func (h *harness) getIntent(intentID string) openapi.PaymentIntent {
	h.t.Helper()
	return h.intent(h.expect(call{method: "GET", path: "/v1/payment_intents/" + intentID}, http.StatusOK))
}

func (h *harness) resolve() {
	h.t.Helper()
	h.clock.Advance(2 * time.Minute)
	if _, err := h.payments.Resolve(h.t.Context(), h.pool); err != nil {
		h.t.Fatalf("Resolve: %v", err)
	}
}

// consistent checks both ledger invariants and that payments agree with the ledger.
func (h *harness) consistent() {
	h.t.Helper()
	report, err := h.ledger.Check(h.t.Context(), h.pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: 1000 * time.Hour})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range report.Violations {
		h.t.Errorf("ledger violation: %+v", v)
	}
	violations, err := h.payments.Check(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range violations {
		h.t.Errorf("payments violation: %+v", v)
	}
}

func (h *harness) merchantBalance() (posted, held int64) {
	h.t.Helper()
	p, err := h.merchants.Authenticate(h.t.Context(), h.pool, h.keys["sk_test_"])
	if err != nil {
		h.t.Fatal(err)
	}
	b, err := h.payments.MerchantBalance(h.t.Context(), h.pool, payments.Owner{Merchant: p.Merchant}, money.BRL)
	if err != nil {
		h.t.Fatal(err)
	}
	postedAmount, err := b.Posted()
	if err != nil {
		h.t.Fatal(err)
	}
	return postedAmount.Minor(), b.PendingCredits.Minor()
}

func wantStatus(t *testing.T, it openapi.PaymentIntent, want openapi.PaymentIntentStatus) {
	t.Helper()
	if it.Status != want {
		t.Fatalf("status = %s, want %s: %+v", it.Status, want, it)
	}
}

// Every transition of the state machine is exercised through
// the API, and every transition not in it is refused.
func TestEveryAllowedTransitionThroughTheAPI(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	pi := "/v1/payment_intents/"

	t.Run("requires_payment_method → requires_confirmation → requires_payment_method", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000}, http.StatusOK)
		wantStatus(t, it, "requires_payment_method")
		it = h.post(pi+it.Id, map[string]any{"payment_method": payments.TestCardVisa}, http.StatusOK)
		wantStatus(t, it, "requires_confirmation")
		it = h.post(pi+it.Id, map[string]any{"payment_method": ""}, http.StatusOK)
		wantStatus(t, it, "requires_payment_method")
	})
	t.Run("requires_payment_method → processing → succeeded", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000}, http.StatusOK)
		it = h.post(pi+it.Id+"/confirm", map[string]any{"payment_method": payments.TestCardVisa}, http.StatusOK)
		wantStatus(t, it, "succeeded")
	})
	t.Run("requires_payment_method → canceled", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000}, http.StatusOK)
		wantStatus(t, h.post(pi+it.Id+"/cancel", nil, http.StatusOK), "canceled")
	})
	t.Run("requires_confirmation → canceled", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa}, http.StatusOK)
		wantStatus(t, h.post(pi+it.Id+"/cancel", nil, http.StatusOK), "canceled")
	})
	t.Run("requires_confirmation → processing → requires_capture → processing → succeeded", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual"}, http.StatusOK)
		it = h.post(pi+it.Id+"/confirm", nil, http.StatusOK)
		wantStatus(t, it, "requires_capture")
		if it.AmountCapturable != 1000 {
			t.Fatalf("amount_capturable = %d", it.AmountCapturable)
		}
		wantStatus(t, h.post(pi+it.Id+"/capture", nil, http.StatusOK), "succeeded")
	})
	t.Run("requires_capture → processing → canceled", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
		it = h.post(pi+it.Id+"/cancel", nil, http.StatusOK)
		wantStatus(t, it, "canceled")
		if it.CancellationReason == nil || *it.CancellationReason != "requested_by_customer" {
			t.Fatalf("cancellation_reason = %v", it.CancellationReason)
		}
	})
	t.Run("processing → requires_payment_method on a decline, then processing again", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardDeclined, "confirm": true}, http.StatusPaymentRequired)
		wantStatus(t, it, "requires_payment_method")
		if it.LastPaymentError == nil || it.LastPaymentError.DeclineCode == nil || *it.LastPaymentError.DeclineCode != "generic_decline" {
			t.Fatalf("last_payment_error = %+v", it.LastPaymentError)
		}
		declined := *it.LatestAttempt
		it = h.post(pi+it.Id+"/confirm", map[string]any{"payment_method": payments.TestCardVisa}, http.StatusOK)
		wantStatus(t, it, "succeeded")
		if *it.LatestAttempt == declined || it.LastPaymentError != nil {
			t.Fatalf("a retry must be a new attempt and clear the error: %+v", it)
		}
	})
	t.Run("processing → requires_action → processing → requires_capture", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardAuthenticationRequired, "capture_method": "manual", "confirm": true}, http.StatusOK)
		wantStatus(t, it, "requires_action")
		if it.NextAction == nil || it.NextAction.Type != "use_test_authentication" {
			t.Fatalf("next_action = %+v", it.NextAction)
		}
		it = h.post("/v1/test_helpers/payment_intents/"+it.Id+"/complete_action", map[string]any{"outcome": "succeeded"}, http.StatusOK)
		wantStatus(t, it, "requires_capture")
	})
	t.Run("requires_action → requires_payment_method", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardAuthenticationRequired, "confirm": true}, http.StatusOK)
		it = h.post("/v1/test_helpers/payment_intents/"+it.Id+"/complete_action", map[string]any{"outcome": "failed"}, http.StatusPaymentRequired)
		wantStatus(t, it, "requires_payment_method")
	})
	t.Run("requires_action → canceled", func(t *testing.T) {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardAuthenticationRequired, "confirm": true}, http.StatusOK)
		wantStatus(t, h.post(pi+it.Id+"/cancel", nil, http.StatusOK), "canceled")
	})

	t.Run("forbidden transitions are refused", func(t *testing.T) {
		succeeded := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
		canceled := h.createIntent(map[string]any{"amount": 1000}, http.StatusOK)
		h.post(pi+canceled.Id+"/cancel", nil, http.StatusOK)
		capturable := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
		fresh := h.createIntent(map[string]any{"amount": 1000}, http.StatusOK)
		for _, c := range []struct{ path, body string }{
			{pi + succeeded.Id + "/capture", ""},
			{pi + succeeded.Id + "/cancel", ""},
			{pi + succeeded.Id + "/confirm", ""},
			{pi + succeeded.Id, `{"amount": 5}`},
			{pi + canceled.Id + "/confirm", `{"payment_method": "pm_card_visa"}`},
			{pi + capturable.Id + "/confirm", ""},
			{pi + fresh.Id + "/capture", ""},
			{pi + fresh.Id + "/confirm", ""},
			{"/v1/test_helpers/payment_intents/" + fresh.Id + "/complete_action", `{"outcome": "succeeded"}`},
		} {
			resp := h.expect(call{method: "POST", path: c.path, body: c.body}, http.StatusBadRequest)
			var e openapi.ErrorResponse
			resp.decode(t, &e)
			if e.Error.Code != "payment_intent_unexpected_state" && e.Error.Code != "parameter_invalid" {
				t.Errorf("%s: %s", c.path, resp.body)
			}
		}
	})
	h.consistent()
}

func TestCaptureRefundAndTheLedger(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 60000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
	if posted, held := h.merchantBalance(); posted != 0 || held != 60000 {
		t.Fatalf("after authorization: posted %d held %d, want 0 and 60000", posted, held)
	}

	it = h.post("/v1/payment_intents/"+it.Id+"/capture", map[string]any{"amount_to_capture": 45000}, http.StatusOK)
	if it.AmountReceived != 45000 || it.AmountCapturable != 0 {
		t.Fatalf("after partial capture: %+v", it)
	}
	// Less Jupiter's fee: 2.99% of R$ 450.00, rounded half up.
	if posted, held := h.merchantBalance(); posted != 45000-1346 || held != 0 {
		t.Fatalf("after capture: posted %d held %d, want 43654 and 0", posted, held)
	}

	refund := func(amount any, status int) openapi.Refund {
		body := map[string]any{"payment_intent": it.Id}
		if amount != nil {
			body["amount"] = amount
		}
		resp := h.expect(call{method: "POST", path: "/v1/refunds", body: body}, status)
		var r openapi.Refund
		if status == http.StatusOK {
			conforms(t, "Refund", resp.body)
			resp.decode(t, &r)
		}
		return r
	}
	if r := refund(10000, http.StatusOK); r.Status != "succeeded" || r.Amount != 10000 {
		t.Fatalf("partial refund = %+v", r)
	}
	refund(40000, http.StatusBadRequest)
	if r := refund(nil, http.StatusOK); r.Amount != 35000 {
		t.Fatalf("refunding the rest = %+v, want 35000", r)
	}
	refund(nil, http.StatusBadRequest)
	if it = h.getIntent(it.Id); it.AmountRefunded != 45000 {
		t.Fatalf("amount_refunded = %d", it.AmountRefunded)
	}
	// Refunds give the fee back with the payment.
	if posted, _ := h.merchantBalance(); posted != 0 {
		t.Fatalf("after refunding everything, posted = %d", posted)
	}
	list := h.expect(call{method: "GET", path: "/v1/refunds?payment_intent=" + it.Id}, http.StatusOK)
	conforms(t, "RefundList", list.body)
	h.consistent()
}

// The test rail's magic amounts lose the answer in three ways. Each attempt starts in
// processing, and the resolver finds out what really happened.
func TestUnknownOutcomesAreResolved(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	for _, c := range []struct {
		amount  int64
		rounds  int
		comment string
	}{
		{60091, 1, "approved, response lost: the query finds the approval"},
		{60092, 1, "request lost: the query finds nothing, the retry is approved"},
		{60093, 2, "late response: the first query finds it pending, the second approved"},
	} {
		it := h.createIntent(map[string]any{"amount": c.amount, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
		wantStatus(t, it, "processing")
		for range c.rounds {
			h.resolve()
		}
		if got := h.getIntent(it.Id); got.Status != "requires_capture" {
			t.Errorf("%d (%s): status %s after %d resolutions", c.amount, c.comment, got.Status, c.rounds)
		}
	}
	h.consistent()
}

// silentRail never answers an authorization, and accepts everything else.
type silentRail struct{}

func (silentRail) Authorize(context.Context, payments.AuthorizeRequest) payments.Result {
	return payments.Result{Outcome: payments.Unknown}
}

func (silentRail) Query(context.Context, string) payments.Result {
	return payments.Result{Outcome: payments.Pending}
}

func (silentRail) Capture(context.Context, payments.OperationRequest) payments.Result {
	return payments.Result{Outcome: payments.Approved}
}

func (silentRail) Void(context.Context, payments.OperationRequest) payments.Result {
	return payments.Result{Outcome: payments.Approved}
}

func (silentRail) Refund(context.Context, payments.OperationRequest) payments.Result {
	return payments.Result{Outcome: payments.Approved}
}

// An authorization whose outcome never becomes known is reversed and failed after the
// give-up time, so no attempt stays unknown for ever.
func TestAnAuthorizationThatStaysUnknownIsReversed(t *testing.T) {
	h := newHarness(t, api.CurrentVersion, withRail(silentRail{}))

	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "processing")
	h.resolve()
	wantStatus(t, h.getIntent(it.Id), "processing")

	h.clock.Advance(20 * time.Minute)
	h.resolve()
	it = h.getIntent(it.Id)
	wantStatus(t, it, "requires_payment_method")
	if it.LastPaymentError == nil || it.LastPaymentError.Code != "authorization_unresolved" {
		t.Fatalf("last_payment_error = %+v", it.LastPaymentError)
	}
	h.consistent()
}

func TestAnUncapturedAuthorizationExpires(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
	// Visa's rule for a customer-initiated card-not-present authorization: ten days.
	h.clock.Advance(10*24*time.Hour + time.Minute)
	if n, err := h.payments.ExpireAuthorizations(t.Context(), h.pool); err != nil || n != 1 {
		t.Fatalf("ExpireAuthorizations = %d, %v", n, err)
	}
	it = h.getIntent(it.Id)
	wantStatus(t, it, "canceled")
	if *it.CancellationReason != "expired" {
		t.Fatalf("cancellation_reason = %s", *it.CancellationReason)
	}
	if _, held := h.merchantBalance(); held != 0 {
		t.Fatalf("held after expiry = %d", held)
	}
	h.consistent()
}

func TestADeclinedConfirmationIsReplayedVerbatim(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardInsufficientFunds}, http.StatusOK)
	headers := map[string]string{"Idempotency-Key": "confirm-1"}
	first := h.expect(call{method: "POST", path: "/v1/payment_intents/" + it.Id + "/confirm", headers: headers}, http.StatusPaymentRequired)
	conforms(t, "ErrorResponse", first.body)
	again := h.expect(call{method: "POST", path: "/v1/payment_intents/" + it.Id + "/confirm", headers: headers}, http.StatusPaymentRequired)
	if string(first.body) != string(again.body) {
		t.Fatalf("replay differs:\n%s\n%s", first.body, again.body)
	}
	var attempts int
	if err := h.pool.QueryRow(t.Context(), "SELECT count(*) FROM payments.attempts WHERE intent_id = $1", it.Id).Scan(&attempts); err != nil || attempts != 1 {
		t.Fatalf("attempts = %d, %v; want one", attempts, err)
	}
}

func TestLiveModeWithoutARail(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	live := h.keys["sk_live_"]
	resp := h.expect(call{
		method: "POST", path: "/v1/payment_intents", key: live,
		body: map[string]any{"amount": 1000, "currency": "brl", "payment_method": payments.TestCardVisa, "confirm": true},
	}, http.StatusBadRequest)
	if !strings.Contains(string(resp.body), "for test mode only") {
		t.Fatalf("a test payment method in live mode: %s", resp.body)
	}
	var pm openapi.PaymentMethod
	h.expect(call{method: "POST", path: "/v1/payment_methods", key: live, body: cardBody("4242424242424242")}, http.StatusOK).decode(t, &pm)
	resp = h.expect(call{
		method: "POST", path: "/v1/payment_intents", key: live,
		body: map[string]any{"amount": 1000, "currency": "brl", "payment_method": pm.Id, "confirm": true},
	}, http.StatusBadRequest)
	var e openapi.ErrorResponse
	resp.decode(t, &e)
	if e.Error.Code != "livemode_unsupported" {
		t.Fatalf("error = %s", resp.body)
	}
}

func TestPaymentEventsAndInclude(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	resp := h.expect(call{method: "GET", path: "/v1/events?type=payment_intent.succeeded&include[]=related_object"}, http.StatusOK)
	var events openapi.EventList
	resp.decode(t, &events)
	if len(events.Data) != 1 || events.Data[0].RelatedObject.Id != it.Id || (*events.Data[0].RelatedObject.Object)["status"] != "succeeded" {
		t.Fatalf("events = %s", resp.body)
	}
	var types []string
	rows, err := h.pool.Query(t.Context(), "SELECT type FROM events.events WHERE object_id = $1 ORDER BY id", it.Id)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ string
		if err := rows.Scan(&typ); err != nil {
			t.Fatal(err)
		}
		types = append(types, typ)
	}
	want := []string{"payment_intent.created", "payment_intent.processing", "payment_intent.succeeded"}
	if len(types) != len(want) || types[0] != want[0] || types[1] != want[1] || types[2] != want[2] {
		t.Fatalf("events for the intent = %v, want %v", types, want)
	}
}

// laggingCaptureRail loses the first answer to every capture, so a capture can be left
// unknown across an outage.
type laggingCaptureRail struct {
	payments.Rail
	lost bool
}

func (r *laggingCaptureRail) Capture(ctx context.Context, req payments.OperationRequest) payments.Result {
	res := r.Rail.Capture(ctx, req)
	if !r.lost {
		r.lost = true
		return payments.Result{Outcome: payments.Unknown}
	}
	return res
}

// A capture whose answer arrives after the ledger's backstop has expired its hold is
// posted on its own: the money was taken, so the ledger must show it exactly once.
func TestACaptureResolvedAfterItsHoldLapsedIsPostedOnce(t *testing.T) {
	rail := &laggingCaptureRail{}
	h := newHarness(t, api.CurrentVersion, withRail(rail))
	rail.Rail = payments.NewTestRail(h.pool, h.clock.Now, nil)

	it := h.createIntent(map[string]any{"amount": 5000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "processing")

	h.clock.Advance(12 * 24 * time.Hour)
	if n, err := h.ledger.ExpireDue(t.Context(), h.pool, 100); err != nil || n != 1 {
		t.Fatalf("ledger ExpireDue = %d, %v; want the hold expired", n, err)
	}
	h.resolve()
	it = h.getIntent(it.Id)
	wantStatus(t, it, "succeeded")
	if posted, held := h.merchantBalance(); posted != 5000-150 || held != 0 {
		t.Fatalf("merchant balance: posted %d, held %d; want 4850 (less the fee) and 0", posted, held)
	}
	h.consistent()
}

// refusingCaptureRail refuses every capture, and counts the voids it is asked for.
type refusingCaptureRail struct {
	payments.Rail
	voids atomic.Int32
}

func (r *refusingCaptureRail) Capture(context.Context, payments.OperationRequest) payments.Result {
	return payments.Result{Outcome: payments.Declined, DeclineCode: "processing_error"}
}

func (r *refusingCaptureRail) Void(ctx context.Context, req payments.OperationRequest) payments.Result {
	r.voids.Add(1)
	return r.Rail.Void(ctx, req)
}

// A capture the rail refuses ends the payment at once, and its authorization, which may
// still hold the cardholder's limit, is voided at the issuer in the background, once.
func TestARefusedCaptureVoidsItsAuthorization(t *testing.T) {
	rail := &refusingCaptureRail{}
	h := newHarness(t, api.CurrentVersion, withRail(rail))
	rail.Rail = payments.NewTestRail(h.pool, h.clock.Now, nil)

	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
	wantStatus(t, it, "requires_capture")
	wantStatus(t, h.post("/v1/payment_intents/"+it.Id+"/capture", nil, http.StatusOK), "canceled")
	if rail.voids.Load() != 0 {
		t.Fatal("the request itself waited on the void")
	}
	h.clock.Advance(2 * time.Minute)
	h.resolve()
	if n := rail.voids.Load(); n != 1 {
		t.Fatalf("the rail was asked for %d voids, want 1", n)
	}
	h.clock.Advance(2 * time.Minute)
	h.resolve()
	if n := rail.voids.Load(); n != 1 {
		t.Fatalf("a settled void was asked for again: %d", n)
	}
	wantStatus(t, h.getIntent(it.Id), "canceled")
	if posted, held := h.merchantBalance(); posted != 0 || held != 0 {
		t.Fatalf("merchant balance: posted %d, held %d", posted, held)
	}
	h.consistent()
}
