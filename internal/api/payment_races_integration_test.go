//go:build integration

package api_test

import (
	"net/http"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

// Capture and cancel racing for one authorization: exactly one wins, and the ledger
// agrees with whichever it was.
func TestCaptureAndCancelRace(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	for round := range 10 {
		it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK)
		statuses := make([]int, 2)
		var wg sync.WaitGroup
		for i, path := range []string{"/capture", "/cancel"} {
			wg.Go(func() {
				resp, err := h.send(t.Context(), call{method: "POST", path: "/v1/payment_intents/" + it.Id + path})
				if err != nil {
					t.Error(err)
					return
				}
				statuses[i] = resp.status
			})
		}
		wg.Wait()
		ok := 0
		for _, s := range statuses {
			if s == http.StatusOK {
				ok++
			}
		}
		final := h.getIntent(it.Id)
		if ok != 1 || (final.Status != "succeeded" && final.Status != "canceled") {
			t.Fatalf("round %d: statuses %v, final %s; want exactly one winner", round, statuses, final.Status)
		}
	}
	h.consistent()
}

// Concurrent refunds never add up to more than was received.
func TestConcurrentRefundsNeverExceedThePayment(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 10000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	var succeeded atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			resp, err := h.send(t.Context(), call{method: "POST", path: "/v1/refunds", body: map[string]any{"payment_intent": it.Id, "amount": 3000}})
			if err == nil && resp.status == http.StatusOK {
				succeeded.Add(1)
			}
		})
	}
	wg.Wait()
	if succeeded.Load() != 3 {
		t.Fatalf("%d refunds of 3000 succeeded on a 10000 payment, want 3", succeeded.Load())
	}
	if refunded := h.getIntent(it.Id).AmountRefunded; refunded != 9000 {
		t.Fatalf("amount_refunded = %d, want 9000", refunded)
	}
	h.consistent()
}

// The expiry job and a capture reaching the same authorization at once.
func TestExpiryAndCaptureRace(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	var ids []string
	for range 10 {
		ids = append(ids, h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardVisa, "capture_method": "manual", "confirm": true}, http.StatusOK).Id)
	}
	h.clock.Advance(7*24*time.Hour + time.Minute)
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := h.payments.ExpireAuthorizations(t.Context(), h.pool); err != nil {
			t.Error(err)
		}
	})
	for _, id := range ids {
		wg.Go(func() {
			_, _ = h.send(t.Context(), call{method: "POST", path: "/v1/payment_intents/" + id + "/capture"})
		})
	}
	wg.Wait()
	for _, id := range ids {
		if s := h.getIntent(id).Status; s != "succeeded" && s != "canceled" {
			t.Errorf("%s ended %s", id, s)
		}
	}
	h.consistent()
}

// Review finding: giving up on an authorization the rail is still processing must
// reverse it on the rail, so its late approval never charges the customer.
func TestGivingUpReversesALateApprovalOnTheRail(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 60093, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "processing")

	// The worker is down for longer than the give-up time; its first look finds the
	// authorization still pending on the rail.
	h.clock.Advance(20 * time.Minute)
	h.resolve()
	it = h.getIntent(it.Id)
	wantStatus(t, it, "requires_payment_method")

	var railStatus string
	if err := h.pool.QueryRow(t.Context(), "SELECT status FROM payments.test_rail WHERE key = $1", *it.LatestAttempt).Scan(&railStatus); err != nil {
		t.Fatal(err)
	}
	if railStatus != "voided" {
		t.Fatalf("the rail holds the authorization as %q after Jupiter gave up; want voided", railStatus)
	}
	// The customer pays again. The amount still makes the test rail answer late, so the
	// new attempt resolves like any other.
	it = h.post("/v1/payment_intents/"+it.Id+"/confirm", map[string]any{"payment_method": payments.TestCardVisa}, http.StatusOK)
	wantStatus(t, it, "processing")
	for range 3 { // pending, then approved, then the automatic capture
		h.resolve()
	}
	wantStatus(t, h.getIntent(it.Id), "succeeded")
	h.consistent()
}

// Review finding: a request that dies after the cardholder authenticated must not send
// them back to requires_action when the resolver picks it up.
func TestAnAuthenticationSurvivesACrash(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	var crash atomic.Bool
	h.api.SetAfterPhase(func(point string) {
		if point == "authorizing" && crash.CompareAndSwap(true, false) {
			runtime.Goexit()
		}
	})
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": payments.TestCardAuthenticationRequired, "capture_method": "manual", "confirm": true}, http.StatusOK)
	wantStatus(t, it, "requires_action")

	// Go's HTTP client resends a request that carries an Idempotency-Key when its
	// connection breaks, so the killed request comes back as a retry that finds the key
	// still locked: a 409, or an error if the retry breaks too.
	h.api.SetIdempotencyTiming(time.Hour, 50*time.Millisecond)
	crash.Store(true)
	resp, err := h.send(t.Context(), call{
		method: "POST", path: "/v1/test_helpers/payment_intents/" + it.Id + "/complete_action",
		body: map[string]any{"outcome": "succeeded"}, headers: map[string]string{"Idempotency-Key": "auth-1"},
	})
	if err == nil && resp.status != http.StatusConflict {
		t.Fatalf("the request completed although it was killed: %d %s", resp.status, resp.body)
	}
	h.resolve()
	wantStatus(t, h.getIntent(it.Id), "requires_capture")
	h.consistent()
}

func TestRefundListPaginates(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	it := h.createIntent(map[string]any{"amount": 10000, "payment_method": payments.TestCardVisa, "confirm": true}, http.StatusOK)
	for range 3 {
		h.expect(call{method: "POST", path: "/v1/refunds", body: map[string]any{"payment_intent": it.Id, "amount": 100}}, http.StatusOK)
	}
	var page openapi.RefundList
	h.expect(call{method: "GET", path: "/v1/refunds?limit=2&payment_intent=" + it.Id}, http.StatusOK).decode(t, &page)
	if len(page.Data) != 2 || !page.HasMore {
		t.Fatalf("page = %+v", page)
	}
}
