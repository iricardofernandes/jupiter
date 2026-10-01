package cardnetwork_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
	"github.com/iricardofernandes/jupiter/pkg/threeds"
)

type movingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *movingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *movingClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// events collects what the network tells the acquirer, checking each signature.
type events struct {
	mu   sync.Mutex
	got  []cardnet.DisputeEvent
	frau []cardnet.FraudReportEvent
}

const eventsSecret = "disputes secret"

func (e *events) server(t *testing.T, now func() time.Time) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := cardnet.VerifyEvent(body, r.Header.Get(cardnet.EventSignatureHeader), eventsSecret, time.Minute, now()); err != nil {
			t.Errorf("an event's signature: %v", err)
		}
		var head struct{ Type string }
		_ = json.Unmarshal(body, &head)
		e.mu.Lock()
		defer e.mu.Unlock()
		if head.Type == "fraud_report.created" {
			var f cardnet.FraudReportEvent
			_ = json.Unmarshal(body, &f)
			e.frau = append(e.frau, f)
			return
		}
		var d cardnet.DisputeEvent
		_ = json.Unmarshal(body, &d)
		e.got = append(e.got, d)
	}))
	t.Cleanup(s.Close)
	return s
}

func (e *events) last() cardnet.Dispute {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.got[len(e.got)-1].Dispute
}

// completed authorizes and completes a payment, authenticated by 3-D Secure if key is set,
// and returns its network transaction id.
func completed(t *testing.T, a *acquirerSide, amount int64, key []byte) string {
	t.Helper()
	m := auth("4242424242424242", amount)
	if key != nil {
		m.Private = &cardnet.PrivateData{DSTransID: "ds-9", ECI: "05", AuthenticationValue: threeds.AuthenticationValue(key, m.PAN, amount, "ds-9")}
	}
	resp := a.send(m)
	wantRC(t, resp, cardnet.Approved)
	wantRC(t, a.send(completion(resp.NetworkTransactionID(), amount)), cardnet.Approved)
	return resp.NetworkTransactionID()
}

func disputeNetwork(t *testing.T) (*acquirerSide, *movingClock, *events) {
	t.Helper()
	clock := &movingClock{now: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	e := &events{}
	srv := e.server(t, clock.Now)
	a := startWith(t, cardnetwork.Config{
		Now: clock.Now, LateAfter: 2 * timeout, AuthenticationKey: []byte("scheme key"),
		DisputeEventsURL: srv.URL, DisputeEventsSecret: eventsSecret,
	})
	return a, clock, e
}

func TestAChargebackAtEachStage(t *testing.T) {
	a, clock, e := disputeNetwork(t)
	ntid := completed(t, a, 30000, nil)
	d, err := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: ntid, ReasonCode: "13.1", Issuer: cardnetwork.IssuerPreArbitration})
	if err != nil || d.Stage != cardnet.StageChargeback || d.Amount != 30000 || !d.Valid() || e.last().ID != d.ID {
		t.Fatalf("opening: %+v, %v", d, err)
	}
	if _, err := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: ntid, ReasonCode: "13.1"}); err == nil {
		t.Fatal("a second chargeback of all of it was opened")
	}
	d, err = a.net.Act(t.Context(), acquirer, d.ID, cardnet.DisputeAction{Action: cardnet.ActionRepresent, Stage: cardnet.StageChargeback, Evidence: "tracking BR1"})
	if err != nil || d.Stage != cardnet.StagePreArbitration || d.Status != cardnet.DisputeOpen {
		t.Fatalf("representing: %+v, %v", d, err)
	}
	again, err := a.net.Act(t.Context(), acquirer, d.ID, cardnet.DisputeAction{Action: cardnet.ActionRepresent, Stage: cardnet.StageChargeback})
	if err != nil || again.Version != d.Version {
		t.Fatalf("the same representment again: %+v, %v", again, err)
	}
	if _, err := a.net.Act(t.Context(), "99999999999", d.ID, cardnet.DisputeAction{Action: cardnet.ActionAccept, Stage: cardnet.StagePreArbitration}); err == nil {
		t.Fatal("another acquirer acted on the case")
	}
	// Left unanswered past its 30 days, the pre-arbitration is the issuer's.
	clock.Advance(31 * 24 * time.Hour)
	a.net.AdvanceDisputes(t.Context())
	if last := e.last(); last.Status != cardnet.DisputeClosed || last.Outcome != cardnet.IssuerWon {
		t.Fatalf("unanswered: %+v", last)
	}
}

func TestTheIssuerAndTheNetworkDecide(t *testing.T) {
	a, clock, _ := disputeNetwork(t)
	silent := completed(t, a, 1000, nil)
	ruled := completed(t, a, 2000, nil)
	s, _ := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: silent, ReasonCode: "4853"})
	r, _ := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: ruled, ReasonCode: "13.3", Issuer: cardnetwork.IssuerPreArbitration, Arbitration: "acquirer"})
	if _, err := a.net.Act(t.Context(), acquirer, s.ID, cardnet.DisputeAction{Action: cardnet.ActionRepresent, Stage: cardnet.StageChargeback}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.net.Act(t.Context(), acquirer, r.ID, cardnet.DisputeAction{Action: cardnet.ActionRepresent, Stage: cardnet.StageChargeback}); err != nil {
		t.Fatal(err)
	}
	if d, err := a.net.Act(t.Context(), acquirer, r.ID, cardnet.DisputeAction{Action: cardnet.ActionEscalate, Stage: cardnet.StagePreArbitration}); err != nil ||
		d.Stage != cardnet.StageArbitration || d.Status != cardnet.DisputeResponded {
		t.Fatalf("escalating: %+v, %v", d, err)
	}
	clock.Advance(31 * 24 * time.Hour)
	for _, id := range []string{s.ID, r.ID} {
		if d, err := a.net.DisputeCase(t.Context(), acquirer, id); err != nil || d.Outcome != cardnet.AcquirerWon {
			t.Fatalf("%s after 31 days: %+v, %v", id, d, err)
		}
	}
	since := a.net.DisputesSince(t.Context(), acquirer, clock.Now().Add(-time.Hour), 10)
	if len(since) != 2 {
		t.Fatalf("cases changed in the last hour: %+v", since)
	}
}

// Fraud disputes of payments 3-D Secure authenticated are the issuer's; any dispute is
// the scheme's once the participants' 180 days have passed.
func TestLiabilityShiftsAndTheCap(t *testing.T) {
	a, clock, e := disputeNetwork(t)
	authenticated := completed(t, a, 5000, []byte("scheme key"))
	if _, err := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: authenticated, ReasonCode: "10.4"}); err == nil {
		t.Fatal("a fraud dispute of an authenticated payment was opened")
	}
	if _, err := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: authenticated, ReasonCode: "13.1"}); err != nil {
		t.Fatalf("a non-fraud dispute of it: %v", err)
	}
	old := completed(t, a, 4000, nil)
	clock.Advance(181 * 24 * time.Hour)
	d, err := a.net.OpenDispute(t.Context(), cardnetwork.DisputeParams{NetworkTransactionID: old, ReasonCode: "10.4", Issuer: cardnetwork.IssuerPreArbitration})
	if err != nil {
		t.Fatal(err)
	}
	if d, err = a.net.Act(t.Context(), acquirer, d.ID, cardnet.DisputeAction{
		Action: cardnet.ActionRepresent, Stage: cardnet.StageChargeback, Reason: cardnet.ReasonLiabilityCap,
	}); err != nil || d.Outcome != cardnet.AcquirerWon {
		t.Fatalf("citing the cap: %+v, %v", d, err)
	}
	if _, err := a.net.ReportFraud(t.Context(), old, "card_not_present"); err != nil || len(e.frau) != 1 || e.frau[0].Report.NetworkTransactionID != old {
		t.Fatalf("a fraud report: %+v, %v", e.frau, err)
	}
}

func TestCategories(t *testing.T) {
	for code, want := range map[string]string{
		"10.4": cardnet.CategoryFraud, "4837": cardnet.CategoryFraud, "11.1": cardnet.CategoryAuthorization,
		"12.6.1": cardnet.CategoryProcessingError, "13.1": cardnet.CategoryConsumer, "4855": cardnet.CategoryConsumer, "99": "",
	} {
		if got := cardnet.CategoryOf(code); got != want {
			t.Errorf("CategoryOf(%s) = %q, want %q", code, got, want)
		}
	}
}
