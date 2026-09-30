//go:build integration

package cardrail_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// The network clears a completion whose acknowledgement Jupiter never received: the
// record waits as an exception until the capture is resolved, then matches.
func TestAClearingRecordWaitsForItsCapture(t *testing.T) {
	h := newHarness(t)
	var lost atomic.Bool
	h.setFaults(func(m cardnet.Message) cardnetwork.Fault {
		if m.MTI == cardnet.CompletionAdvice && lost.CompareAndSwap(false, true) {
			return cardnetwork.LoseAnswer
		}
		return cardnetwork.NoFault
	})
	_, it := h.pay(map[string]any{"amount": 9000, "payment_method": h.saveCard("4242424242424242")})
	today := h.clock.Now().UTC()
	if err := h.network.CloseDay(today); err != nil {
		t.Fatal(err)
	}
	report, err := h.connector.ImportClearing(t.Context(), today, h.payments)
	if err != nil || report.Exceptions != 1 {
		t.Fatalf("with the capture unacknowledged: %+v, %v", report, err)
	}
	if n, err := h.connector.RetryExceptions(t.Context(), h.payments); err != nil || n != 0 {
		t.Fatalf("RetryExceptions before the capture resolved = %d, %v", n, err)
	}
	h.resolve()
	h.resolve()
	if n, err := h.connector.RetryExceptions(t.Context(), h.payments); err != nil || n != 1 {
		t.Fatalf("RetryExceptions after = %d, %v", n, err)
	}
	if a := h.attempt(str(it, "id")); a.AmountCleared != 9000 {
		t.Fatalf("after the retry: %+v", a)
	}
}

// A clearing record must agree with what was sent under its RRN.
func TestClearingRecordsMustMatchWhatWasSent(t *testing.T) {
	h := newHarness(t)
	_, it := h.pay(map[string]any{"amount": 5000, "payment_method": h.saveCard("4242424242424242")})
	nti := h.attempt(str(it, "id")).NetworkTransactionID
	var rrn, merchant string
	if err := h.pool.QueryRow(t.Context(), "SELECT rrn, merchant_code FROM acquirer.exchanges WHERE kind = 'capture'").Scan(&rrn, &merchant); err != nil {
		t.Fatal(err)
	}
	record := cardnet.ClearingRecord{Kind: cardnet.ClearingCompletion, RRN: rrn, NetworkTransactionID: nti, Amount: 5000, Currency: "986", MerchantID: merchant}
	wrongMerchant, wrongNTI, wrongKind, unknown := record, record, record, record
	wrongMerchant.MerchantID, wrongNTI.NetworkTransactionID, wrongKind.Kind, unknown.RRN = "MOTHERMERCHANT0", "899999999999999", cardnet.ClearingRefund, "600100000000"
	var file cardnet.ClearingFile
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(file.Encode()) }))
	defer files.Close()
	reader, err := acquirer.New(acquirer.Config{Pool: h.pool, Addr: h.network.Addr(), NetworkURL: files.URL, Cards: nopCards{}})
	if err != nil {
		t.Fatal(err)
	}
	for i, tt := range []struct {
		records             []cardnet.ClearingRecord
		cleared, exceptions int
		reason              string
	}{
		{[]cardnet.ClearingRecord{wrongMerchant}, 0, 1, "the merchant differs"},
		{[]cardnet.ClearingRecord{wrongNTI}, 0, 1, "the network transaction differs"},
		{[]cardnet.ClearingRecord{wrongKind}, 0, 1, "belongs to a capture"},
		{[]cardnet.ClearingRecord{unknown}, 0, 1, "no capture or refund"},
		{[]cardnet.ClearingRecord{record, record}, 1, 1, "twice"},
	} {
		day := time.Date(2026, 1, 2+i, 0, 0, 0, 0, time.UTC)
		file = cardnet.ClearingFile{BusinessDate: day, AcquirerID: "10000000001", Records: tt.records}
		report, err := reader.ImportClearing(t.Context(), day, h.payments)
		if err != nil || report.Cleared != tt.cleared || report.Exceptions != tt.exceptions {
			t.Fatalf("file %d: %+v, %v", i, report, err)
		}
		var reason string
		if err := h.pool.QueryRow(t.Context(), "SELECT reason FROM acquirer.clearing_exceptions WHERE business_date = $1", day).Scan(&reason); err != nil || !strings.Contains(reason, tt.reason) {
			t.Fatalf("file %d: exception %q (%v), want %q", i, reason, err, tt.reason)
		}
	}
}

// A void before the authorization was ever sent leaves a tombstone: the authorization,
// should it come later, is never sent.
func TestAVoidBeforeTheAuthorization(t *testing.T) {
	h := newHarness(t)
	amount, _ := money.New(1000, money.BRL)
	if res := h.connector.Void(t.Context(), payments.OperationRequest{Key: "pa_early:void", AuthorizationKey: "pa_early", Amount: amount}); res.Outcome != payments.Approved {
		t.Fatalf("Void = %+v", res)
	}
	res := h.connector.Authorize(t.Context(), payments.AuthorizeRequest{Key: "pa_early", Amount: amount, Card: &payments.CardReference{Token: "tok_x", Owner: "o"}})
	if res.Outcome != payments.Declined || len(h.network.Holds()) != 0 {
		t.Fatalf("the authorization after its void: %+v, holds %+v", res, h.network.Holds())
	}
}
