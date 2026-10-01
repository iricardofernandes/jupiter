package cardnetwork_test

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moov-io/iso8583"
	connection "github.com/moov-io/iso8583-connection"

	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

const (
	acquirer = "10000000001"
	timeout  = 300 * time.Millisecond
)

type acquirerSide struct {
	t        *testing.T
	net      *cardnetwork.Network
	conn     *connection.Connection
	stan     atomic.Int64
	received chan cardnet.Message
}

func start(t *testing.T, faults func(cardnet.Message) cardnetwork.Fault) *acquirerSide {
	t.Helper()
	return startWith(t, cardnetwork.Config{
		Now:       func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) },
		LateAfter: 2 * timeout, Faults: faults,
	})
}

func startWith(t *testing.T, cfg cardnetwork.Config) *acquirerSide {
	t.Helper()
	n := cardnetwork.New(cfg)
	if err := n.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	a := &acquirerSide{t: t, net: n, received: make(chan cardnet.Message, 10)}
	conn, err := connection.New(n.Addr(), cardnet.Spec, cardnet.ReadLength, cardnet.WriteLength,
		connection.SendTimeout(timeout),
		connection.InboundMessageHandler(func(_ *connection.Connection, raw *iso8583.Message) {
			m, err := cardnet.Unpack(raw)
			if err != nil {
				t.Error(err)
				return
			}
			a.received <- m
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	a.conn = conn
	a.send(cardnet.Message{MTI: cardnet.NetworkRequest, NetworkCode: cardnet.SignOn})
	return a
}

// send fills in the routing fields and sends m, returning the answer or nil on a timeout.
func (a *acquirerSide) send(m cardnet.Message) *cardnet.Message {
	a.t.Helper()
	resp, err := a.try(m)
	if errors.Is(err, connection.ErrSendTimeout) {
		return nil
	}
	if err != nil {
		a.t.Fatal(err)
	}
	return &resp
}

func (a *acquirerSide) try(m cardnet.Message) (cardnet.Message, error) {
	if m.STAN == "" {
		m.STAN = fmt.Sprintf("%06d", a.stan.Add(1))
	}
	if m.TransmissionDateTime == "" {
		m.TransmissionDateTime = "1001120000"
	}
	m.AcquirerID = acquirer
	if m.RRN == "" && m.MTI != cardnet.NetworkRequest {
		m.RRN = "6274" + fmt.Sprintf("%08s", m.STAN)
	}
	msg, err := cardnet.Pack(m)
	if err != nil {
		return cardnet.Message{}, err
	}
	raw, err := a.conn.Send(msg)
	if err != nil {
		return cardnet.Message{}, err
	}
	return cardnet.Unpack(raw)
}

func auth(pan string, amount int64) cardnet.Message {
	return cardnet.Message{
		MTI: cardnet.AuthorizationRequest, PAN: pan, ProcessingCode: cardnet.ProcessingPurchase, Amount: amount, TransmissionDateTime: "1001120000",
		Expiry: "3012", EntryMode: cardnet.EntryECommerce, TerminalID: "JUPITER1", MerchantID: "M0000000000000A",
		Currency: cardnet.CurrencyBRL, Installments: "06", Private: &cardnet.PrivateData{InstallmentFinancing: "M"},
	}
}

func completion(nti string, amount int64) cardnet.Message {
	return cardnet.Message{
		MTI: cardnet.CompletionAdvice, Amount: amount, Currency: cardnet.CurrencyBRL,
		Private: &cardnet.PrivateData{NetworkTransactionID: nti},
	}
}

func reversal(mti string, original cardnet.Message, amount int64) cardnet.Message {
	original.AcquirerID = acquirer
	return cardnet.Message{MTI: mti, Amount: amount, Currency: cardnet.CurrencyBRL, OriginalData: cardnet.Original(original).Encode()}
}

func wantRC(t *testing.T, resp *cardnet.Message, rc string) {
	t.Helper()
	if resp == nil {
		t.Fatalf("no answer, want %s", rc)
	}
	if resp.ResponseCode != rc {
		t.Fatalf("answer %s, want response code %s", resp, rc)
	}
}

func TestSignOnAndEcho(t *testing.T) {
	a := start(t, nil)
	resp := a.send(cardnet.Message{MTI: cardnet.NetworkRequest, NetworkCode: cardnet.Echo})
	wantRC(t, resp, cardnet.Approved)
	if resp.MTI != cardnet.NetworkResponse || resp.NetworkCode != cardnet.Echo {
		t.Fatalf("echo = %s %s", resp.MTI, resp.NetworkCode)
	}
}

func TestAuthorizeCompleteAndClear(t *testing.T) {
	a := start(t, nil)
	resp := a.send(auth("4242424242424242", 60000))
	wantRC(t, resp, cardnet.Approved)
	nti := resp.NetworkTransactionID()
	if resp.MTI != cardnet.AuthorizationResponse || len(nti) != 15 || len(resp.AuthorizationCode) != 6 || resp.Amount != 60000 {
		t.Fatalf("approval = %+v %s", resp, nti)
	}
	if holds := a.net.Holds(); len(holds) != 1 || holds[0].Amount != 60000 {
		t.Fatalf("holds = %+v", holds)
	}
	// A capture of less than was authorized releases the rest.
	wantRC(t, a.send(completion(nti, 50000)), cardnet.Approved)
	repeat := completion(nti, 50000)
	repeat.MTI = cardnet.CompletionAdviceRepeat
	wantRC(t, a.send(repeat), cardnet.Approved)
	wantRC(t, a.send(completion(nti, 40000)), cardnet.InvalidTransaction)
	if holds := a.net.Holds(); len(holds) != 0 {
		t.Fatalf("holds after completion = %+v", holds)
	}
	if c := a.net.Completions(); len(c) != 1 || c[0].Completed != 50000 {
		t.Fatalf("completions = %+v", c)
	}

	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if err := a.net.CloseDay(day); err != nil {
		t.Fatal(err)
	}
	if err := a.net.CloseDay(day); err == nil {
		t.Fatal("a business day closed twice")
	}
	srv := httptest.NewServer(a.net.Handler())
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/acquirers/" + acquirer + "/clearing/2026-10-01") //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	file, err := cardnet.DecodeClearing(body)
	if err != nil || len(file.Records) != 1 {
		t.Fatalf("clearing file = %+v, %v\n%s", file, err, body)
	}
	if r := file.Records[0]; r.Kind != cardnet.ClearingCompletion || r.Amount != 50000 || r.NetworkTransactionID != nti || r.Installments != 6 {
		t.Fatalf("record = %+v", r)
	}
	if res, _ := http.Get(srv.URL + "/v1/acquirers/" + acquirer + "/clearing/2026-10-02"); res.StatusCode != http.StatusNotFound { //nolint:noctx,bodyclose // a test
		t.Fatalf("an open day's file: %d", res.StatusCode)
	}
}

func TestIssuerDeclines(t *testing.T) {
	a := start(t, nil)
	expiredCard := auth("4242424242424242", 1000)
	expiredCard.Expiry = "2609"
	mit := auth("4242424242424242", 1000)
	mit.EntryMode, mit.Private = cardnet.EntryCredentialOnFile, &cardnet.PrivateData{StoredCredential: cardnet.StoredCredentialMerchant, NetworkTransactionID: "800000000000999"}
	for name, tt := range map[string]struct {
		m  cardnet.Message
		rc string
	}{
		"do not honour":      {auth("4000000000000002", 1000), cardnet.DoNotHonour},
		"insufficient funds": {auth("4000000000009995", 1000), cardnet.InsufficientFunds},
		"expired test card":  {auth("4000000000000069", 1000), cardnet.ExpiredCard},
		"expired by date":    {expiredCard, cardnet.ExpiredCard},
		"bad check digit":    {auth("4242424242424241", 1000), cardnet.InvalidCardNumber},
		"over the limit":     {auth("5555555555554444", 10_000_001), cardnet.InsufficientFunds},
		"unknown first CIT":  {mit, cardnet.NotPermittedToCardholder},
	} {
		t.Run(name, func(t *testing.T) {
			wantRC(t, a.send(tt.m), tt.rc)
		})
	}
	if holds := a.net.Holds(); len(holds) != 0 {
		t.Fatalf("declines left holds: %+v", holds)
	}
}

func TestAMerchantInitiatedPaymentNamesTheFirst(t *testing.T) {
	a := start(t, nil)
	first := auth("4242424242424242", 1000)
	first.Private = &cardnet.PrivateData{StoredCredential: cardnet.StoredCredentialInitial}
	nti := a.send(first).NetworkTransactionID()
	mit := auth("4242424242424242", 2000)
	mit.EntryMode, mit.Private = cardnet.EntryCredentialOnFile, &cardnet.PrivateData{StoredCredential: cardnet.StoredCredentialMerchant, NetworkTransactionID: nti}
	wantRC(t, a.send(mit), cardnet.Approved)
	other := mit
	other.PAN = "5555555555554444"
	wantRC(t, a.send(other), cardnet.NotPermittedToCardholder)
}

// A lost answer leaves a hold at the issuer until the acquirer's reversal releases it.
func TestAReversalReleasesAHoldWhoseAnswerWasLost(t *testing.T) {
	a := start(t, nil)
	m := auth("4000000000000119", 7000)
	m.STAN = "000900"
	if resp := a.send(m); resp != nil {
		t.Fatalf("the answer arrived: %s", resp)
	}
	if holds := a.net.Holds(); len(holds) != 1 {
		t.Fatalf("holds = %+v, want the one approved", holds)
	}
	wantRC(t, a.send(reversal(cardnet.ReversalAdvice, m, 7000)), cardnet.Approved)
	if holds := a.net.Holds(); len(holds) != 0 {
		t.Fatalf("the reversal left %+v", holds)
	}
	// A repeat, and a reversal of something the issuer never saw, are acknowledged.
	wantRC(t, a.send(reversal(cardnet.ReversalAdviceRepeat, m, 7000)), cardnet.Approved)
	lost := auth("4000000000000168", 7000)
	lost.STAN = "000901"
	if resp := a.send(lost); resp != nil || len(a.net.Holds()) != 0 {
		t.Fatalf("a lost request was answered or held: %v %+v", resp, a.net.Holds())
	}
	wantRC(t, a.send(reversal(cardnet.ReversalAdvice, lost, 7000)), cardnet.Approved)
}

// The late answer arrives after the reversal, and the reversal wins.
func TestALateAnswerAfterTheReversal(t *testing.T) {
	a := start(t, nil)
	m := auth("4000000000000127", 8000)
	m.STAN = "000950"
	if resp := a.send(m); resp != nil {
		t.Fatalf("the answer was not late: %s", resp)
	}
	wantRC(t, a.send(reversal(cardnet.ReversalAdvice, m, 8000)), cardnet.Approved)
	select {
	case late := <-a.received:
		if late.MTI != cardnet.AuthorizationResponse || late.STAN != "000950" || late.ResponseCode != cardnet.Approved {
			t.Fatalf("late answer = %s", late)
		}
	case <-time.After(5 * timeout):
		t.Fatal("the late answer never came")
	}
	if holds := a.net.Holds(); len(holds) != 0 {
		t.Fatalf("the late approval left a hold after the reversal: %+v", holds)
	}
}

func TestAnAnswerSentTwice(t *testing.T) {
	a := start(t, nil)
	resp := a.send(auth("4000000000000135", 1000))
	wantRC(t, resp, cardnet.Approved)
	select {
	case dup := <-a.received:
		if dup.STAN != resp.STAN || dup.NetworkTransactionID() != resp.NetworkTransactionID() {
			t.Fatalf("duplicate = %s", dup)
		}
	case <-time.After(5 * timeout):
		t.Fatal("no duplicate")
	}
}

func TestPartialApprovalOnlyWhenAsked(t *testing.T) {
	a := start(t, nil)
	wantRC(t, a.send(auth("4000000000000143", 10000)), cardnet.Approved)
	capable := auth("4000000000000143", 10000)
	capable.Private = &cardnet.PrivateData{PartialApproval: "1"}
	resp := a.send(capable)
	wantRC(t, resp, cardnet.PartiallyApproved)
	if resp.Amount != 5000 {
		t.Fatalf("approved %d, want half", resp.Amount)
	}
}

func TestStandIn(t *testing.T) {
	a := start(t, nil)
	resp := a.send(auth("4000000000000150", 50000))
	wantRC(t, resp, cardnet.Approved)
	if resp.Private == nil || resp.Private.StandIn != "1" {
		t.Fatalf("a stand-in approval not flagged: %+v", resp.Private)
	}
	wantRC(t, a.send(auth("4000000000000150", 50001)), cardnet.IssuerUnavailable)
}

func TestReversalRequestsAndRefunds(t *testing.T) {
	a := start(t, nil)
	m := auth("4242424242424242", 9000)
	m.STAN = "000970"
	nti := a.send(m).NetworkTransactionID()
	wantRC(t, a.send(reversal(cardnet.ReversalRequest, m, 9000)), cardnet.Approved)
	wantRC(t, a.send(completion(nti, 9000)), cardnet.InvalidTransaction)

	m2 := auth("4242424242424242", 9000)
	m2.STAN = "000971"
	nti2 := a.send(m2).NetworkTransactionID()
	wantRC(t, a.send(completion(nti2, 9000)), cardnet.Approved)
	resp := a.send(reversal(cardnet.ReversalRequest, m2, 9000))
	wantRC(t, resp, cardnet.InvalidTransaction)
	if resp.MTI != cardnet.ReversalResponse {
		t.Fatalf("MTI %s", resp.MTI)
	}

	refund := cardnet.Message{
		MTI: cardnet.FinancialRequest, ProcessingCode: cardnet.ProcessingRefund, Amount: 4000, STAN: "000972", TransmissionDateTime: "1001120000",
		Currency: cardnet.CurrencyBRL, Private: &cardnet.PrivateData{NetworkTransactionID: nti2},
	}
	wantRC(t, a.send(refund), cardnet.Approved)
	tooMuch := refund
	tooMuch.STAN, tooMuch.Amount = "000973", 6000
	wantRC(t, a.send(tooMuch), cardnet.InvalidAmount)
	wantRC(t, a.send(reversal(cardnet.ReversalAdvice, refund, 4000)), cardnet.Approved)
	if err := a.net.CloseDay(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	raw, _ := a.net.ClearingFile(acquirer, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	file, err := cardnet.DecodeClearing(raw)
	if err != nil || len(file.Records) != 1 || file.Records[0].Kind != cardnet.ClearingCompletion {
		t.Fatalf("a reversed refund was cleared: %+v %v", file.Records, err)
	}
}

func TestASingleMessagePurchaseIsCompletedAtOnce(t *testing.T) {
	a := start(t, nil)
	m := auth("4242424242424242", 3000)
	m.MTI = cardnet.FinancialRequest
	resp := a.send(m)
	wantRC(t, resp, cardnet.Approved)
	if resp.MTI != cardnet.FinancialResponse || len(a.net.Completions()) != 1 || len(a.net.Holds()) != 0 {
		t.Fatalf("purchase = %s, completions %+v", resp, a.net.Completions())
	}
}

func TestInjectedFaults(t *testing.T) {
	var lose atomic.Bool
	a := start(t, func(m cardnet.Message) cardnetwork.Fault {
		if lose.Load() && m.MTI == cardnet.CompletionAdvice {
			return cardnetwork.LoseAnswer
		}
		return cardnetwork.NoFault
	})
	nti := a.send(auth("4242424242424242", 1000)).NetworkTransactionID()
	lose.Store(true)
	if resp := a.send(completion(nti, 1000)); resp != nil {
		t.Fatalf("answered despite the fault: %s", resp)
	}
	lose.Store(false)
	repeat := completion(nti, 1000)
	repeat.MTI = cardnet.CompletionAdviceRepeat
	wantRC(t, a.send(repeat), cardnet.Approved)
	if c := a.net.Completions(); len(c) != 1 {
		t.Fatalf("completions = %+v", c)
	}
}

func TestAdminCloseDay(t *testing.T) {
	a := start(t, nil)
	srv := httptest.NewServer(a.net.Handler())
	defer srv.Close()
	for _, tt := range []struct {
		query  string
		status int
	}{{"?date=2026-10-05", http.StatusNoContent}, {"?date=2026-10-05", http.StatusConflict}, {"?date=bad", http.StatusBadRequest}, {"", http.StatusNoContent}} {
		res, err := http.Post(srv.URL+"/admin/close-day"+tt.query, "application/json", strings.NewReader("")) //nolint:noctx // a test
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != tt.status {
			t.Errorf("close-day%s = %d, want %d", tt.query, res.StatusCode, tt.status)
		}
	}
}

func TestAnAuthenticationValueMustCheckOut(t *testing.T) {
	key := []byte("scheme key")
	n := cardnetwork.New(cardnetwork.Config{AuthenticationKey: key, Now: func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }})
	if err := n.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	a := &acquirerSide{t: t, net: n, received: make(chan cardnet.Message, 10)}
	conn, err := connection.New(n.Addr(), cardnet.Spec, cardnet.ReadLength, cardnet.WriteLength, connection.SendTimeout(timeout))
	if err != nil || conn.Connect() != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	a.conn = conn
	good := auth("4242424242424242", 5000)
	good.Private = &cardnet.PrivateData{DSTransID: "ds-1", ECI: "05", AuthenticationValue: threeds.AuthenticationValue(key, "4242424242424242", 5000, "ds-1")}
	wantRC(t, a.send(good), cardnet.Approved)
	forged := auth("4242424242424242", 5000)
	forged.Private = &cardnet.PrivateData{DSTransID: "ds-1", ECI: "05", AuthenticationValue: threeds.AuthenticationValue(key, "4242424242424242", 9000, "ds-1")}
	wantRC(t, a.send(forged), cardnet.DoNotHonour)
}
