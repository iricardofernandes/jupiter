package cardnetwork

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// IssuerScript is what the issuer does with a representment: accept it, reject it into
// pre-arbitration, or let its deadline pass, which the acquirer then wins.
type IssuerScript string

const (
	IssuerAccepts        IssuerScript = "accepts"
	IssuerPreArbitration IssuerScript = "pre_arbitration"
	IssuerSilent         IssuerScript = "silent"
)

// liabilityCap is how long after authorization a dispute leaves the acquirer liable
// (Res. BCB 522/2025, as the network applies it to the arrangement).
const liabilityCap = 180 * 24 * time.Hour

const oneDay = 24 * time.Hour

var (
	errNoDispute     = errors.New("no such dispute")
	errDisputeAction = errors.New("the dispute does not take that action now")
)

type disputeCase struct {
	d        cardnet.Dispute
	pan      string
	issuer   IssuerScript
	ruling   string    // who arbitration rules for
	decideBy time.Time // when the issuer, or the network, has to have answered
	changed  time.Time
	done     map[string]bool
}

type disputeBook struct {
	cases   map[string]*disputeCase
	reports []cardnet.FraudReport
	seq     int64
}

// DisputeParams open a chargeback: the issuer disputes Amount (all that is left, when
// zero) of a completed authorization, and scripts what it and the network do next.
type DisputeParams struct {
	NetworkTransactionID string       `json:"network_transaction_id"`
	Amount               int64        `json:"amount"`
	ReasonCode           string       `json:"reason_code"`
	Issuer               IssuerScript `json:"issuer"`
	Arbitration          string       `json:"arbitration"` // acquirer or issuer
}

// representWithin is how long the acquirer has to answer a chargeback: Mastercard gives
// 45 days, the others 30, as Jupiter's table has it.
func representWithin(pan string) time.Duration {
	if strings.HasPrefix(pan, "5") || strings.HasPrefix(pan, "2") {
		return 45 * oneDay
	}
	return 30 * oneDay
}

// OpenDispute opens a chargeback on a completed authorization. A fraud dispute of one
// authenticated by 3-D Secure is refused: the liability shifted to the issuer.
func (n *Network) OpenDispute(ctx context.Context, p DisputeParams) (cardnet.Dispute, error) {
	category := cardnet.CategoryOf(p.ReasonCode)
	switch {
	case category == "":
		return cardnet.Dispute{}, fmt.Errorf("unknown reason code %q", p.ReasonCode)
	case p.Issuer == "":
		p.Issuer = IssuerSilent
	case p.Issuer != IssuerAccepts && p.Issuer != IssuerPreArbitration && p.Issuer != IssuerSilent:
		return cardnet.Dispute{}, fmt.Errorf("issuer %q", p.Issuer)
	}
	if p.Arbitration == "" {
		p.Arbitration = "issuer"
	}
	s := n.issuer
	s.mu.Lock()
	a, ok := s.byNTI[p.NetworkTransactionID]
	if !ok || a.Status != completed {
		s.mu.Unlock()
		return cardnet.Dispute{}, fmt.Errorf("%w: no completed authorization %s", errNoDispute, p.NetworkTransactionID)
	}
	left := a.Completed - a.Refunded - a.Disputed
	if p.Amount == 0 {
		p.Amount = left
	}
	if p.Amount <= 0 || p.Amount > left {
		s.mu.Unlock()
		return cardnet.Dispute{}, fmt.Errorf("%d can be disputed, not %d", left, p.Amount)
	}
	if category == cardnet.CategoryFraud && a.Authenticated {
		s.mu.Unlock()
		return cardnet.Dispute{}, errors.New("authenticated by 3-D Secure: the liability for fraud is the issuer's")
	}
	a.Disputed += p.Amount
	now := n.cfg.Now().UTC()
	n.book.seq++
	c := &disputeCase{
		pan: a.PAN, issuer: p.Issuer, ruling: p.Arbitration, done: map[string]bool{}, changed: now,
		d: cardnet.Dispute{
			ID: fmt.Sprintf("cb_%08d", n.book.seq), NetworkTransactionID: a.NTI, Acquirer: a.AcquirerID, Merchant: a.MerchantID,
			Amount: p.Amount, Currency: "BRL", ReasonCode: p.ReasonCode, Category: category, Stage: cardnet.StageChargeback,
			Status: cardnet.DisputeOpen, RespondBy: now.Add(representWithin(a.PAN)), AuthorizedAt: a.At, OpenedAt: now, Version: 1,
		},
	}
	n.book.cases[c.d.ID] = c
	out := c.d
	s.mu.Unlock()
	n.tellDispute(ctx, out)
	return out, nil
}

// ReportFraud records an issuer's fraud report on a completed authorization and tells the
// acquirer.
func (n *Network) ReportFraud(ctx context.Context, ntid, fraudType string) (cardnet.FraudReport, error) {
	s := n.issuer
	s.mu.Lock()
	a, ok := s.byNTI[ntid]
	if !ok || a.Status != completed {
		s.mu.Unlock()
		return cardnet.FraudReport{}, fmt.Errorf("%w: no completed authorization %s", errNoDispute, ntid)
	}
	n.book.seq++
	r := cardnet.FraudReport{
		ID: fmt.Sprintf("tc40_%08d", n.book.seq), NetworkTransactionID: ntid, Acquirer: a.AcquirerID, Amount: a.Completed,
		FraudType: fraudType, ReportedAt: n.cfg.Now().UTC(),
	}
	n.book.reports = append(n.book.reports, r)
	s.mu.Unlock()
	n.tell(ctx, cardnet.FraudReportEvent{Type: "fraud_report.created", Report: r})
	return r, nil
}

// Act applies an acquirer's answer to its case. The same action at the same stage again
// answers with the case as it is.
func (n *Network) Act(ctx context.Context, acquirer, id string, a cardnet.DisputeAction) (cardnet.Dispute, error) {
	n.AdvanceDisputes(ctx)
	s := n.issuer
	s.mu.Lock()
	c, ok := n.book.cases[id]
	if !ok || c.d.Acquirer != cardnet.PadAcquirer(acquirer) {
		s.mu.Unlock()
		return cardnet.Dispute{}, errNoDispute
	}
	key := a.Stage + "/" + a.Action
	if c.done[key] {
		out := c.d
		s.mu.Unlock()
		return out, nil
	}
	if c.d.Status != cardnet.DisputeOpen || c.d.Stage != a.Stage {
		s.mu.Unlock()
		return cardnet.Dispute{}, errDisputeAction
	}
	now := n.cfg.Now().UTC()
	switch {
	case a.Action == cardnet.ActionAccept:
		c.close(cardnet.IssuerWon)
	case a.Action == cardnet.ActionRepresent && a.Stage == cardnet.StageChargeback:
		n.represent(c, a, now)
	case a.Action == cardnet.ActionEscalate && a.Stage == cardnet.StagePreArbitration:
		c.d.Stage, c.d.Status, c.d.RespondBy, c.decideBy = cardnet.StageArbitration, cardnet.DisputeResponded, time.Time{}, now.Add(30*oneDay)
	default:
		s.mu.Unlock()
		return cardnet.Dispute{}, errDisputeAction
	}
	c.done[key] = true
	c.d.Version++
	c.changed = now
	out := c.d
	s.mu.Unlock()
	n.tellDispute(ctx, out)
	return out, nil
}

// represent applies a representment: one citing the liability cap wins when the dispute
// came after it; any other goes to the issuer's script.
func (n *Network) represent(c *disputeCase, a cardnet.DisputeAction, now time.Time) {
	if a.Reason == cardnet.ReasonLiabilityCap && c.d.OpenedAt.Sub(c.d.AuthorizedAt) > liabilityCap {
		c.close(cardnet.AcquirerWon)
		return
	}
	switch c.issuer {
	case IssuerAccepts:
		c.close(cardnet.AcquirerWon)
	case IssuerPreArbitration:
		c.d.Stage, c.d.RespondBy = cardnet.StagePreArbitration, now.Add(30*oneDay)
	default:
		c.d.Status, c.d.RespondBy, c.decideBy = cardnet.DisputeResponded, time.Time{}, now.Add(30*oneDay)
	}
}

func (c *disputeCase) close(outcome string) {
	c.d.Status, c.d.Outcome, c.d.RespondBy, c.decideBy = cardnet.DisputeClosed, outcome, time.Time{}, time.Time{}
}

// AdvanceDisputes closes the cases whose deadline passed: an acquirer that did not answer
// loses, an issuer that did not answer a representment loses, and arbitration rules as
// scripted.
func (n *Network) AdvanceDisputes(ctx context.Context) {
	now := n.cfg.Now().UTC()
	var changed []cardnet.Dispute
	n.issuer.mu.Lock()
	for _, c := range n.book.cases {
		switch {
		case c.d.Status == cardnet.DisputeOpen && now.After(c.d.RespondBy):
			c.close(cardnet.IssuerWon)
		case c.d.Status == cardnet.DisputeResponded && now.After(c.decideBy) && c.d.Stage == cardnet.StageArbitration:
			outcome := cardnet.IssuerWon
			if c.ruling == "acquirer" {
				outcome = cardnet.AcquirerWon
			}
			c.close(outcome)
		case c.d.Status == cardnet.DisputeResponded && now.After(c.decideBy):
			c.close(cardnet.AcquirerWon)
		default:
			continue
		}
		c.d.Version++
		c.changed = now
		changed = append(changed, c.d)
	}
	n.issuer.mu.Unlock()
	slices.SortFunc(changed, func(a, b cardnet.Dispute) int { return strings.Compare(a.ID, b.ID) })
	for _, d := range changed {
		n.tellDispute(ctx, d)
	}
}

// DisputeCase is a case as the network has it, for its acquirer.
func (n *Network) DisputeCase(ctx context.Context, acquirer, id string) (cardnet.Dispute, error) {
	n.AdvanceDisputes(ctx)
	n.issuer.mu.Lock()
	defer n.issuer.mu.Unlock()
	c, ok := n.book.cases[id]
	if !ok || c.d.Acquirer != cardnet.PadAcquirer(acquirer) {
		return cardnet.Dispute{}, errNoDispute
	}
	return c.d, nil
}

// maxCasesListed is the most cases one listing answers, the oldest changes first.
const maxCasesListed = 500

// DisputesSince lists up to limit of an acquirer's cases changed since a moment, the
// oldest changes first: what events missed.
func (n *Network) DisputesSince(ctx context.Context, acquirer string, since time.Time, limit int) []cardnet.Dispute {
	n.AdvanceDisputes(ctx)
	n.issuer.mu.Lock()
	defer n.issuer.mu.Unlock()
	var found []*disputeCase
	for _, c := range n.book.cases {
		if c.d.Acquirer == cardnet.PadAcquirer(acquirer) && !c.changed.Before(since) {
			found = append(found, c)
		}
	}
	slices.SortFunc(found, func(a, b *disputeCase) int {
		if c := a.changed.Compare(b.changed); c != 0 {
			return c
		}
		return strings.Compare(a.d.ID, b.d.ID)
	})
	out := []cardnet.Dispute{}
	for _, c := range found[:min(len(found), limit)] {
		out = append(out, c.d)
	}
	return out
}

// Disputes lists every case, for tests.
func (n *Network) Disputes() []cardnet.Dispute {
	n.issuer.mu.Lock()
	defer n.issuer.mu.Unlock()
	out := make([]cardnet.Dispute, 0, len(n.book.cases))
	for _, c := range n.book.cases {
		out = append(out, c.d)
	}
	slices.SortFunc(out, func(a, b cardnet.Dispute) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func (n *Network) tellDispute(ctx context.Context, d cardnet.Dispute) {
	n.tell(ctx, cardnet.DisputeEvent{Type: "dispute.updated", Dispute: d})
}

// tell posts an event to the acquirer's dispute events URL, signed with the dispute events
// secret. One that does not arrive is found when the acquirer lists its cases.
func (n *Network) tell(ctx context.Context, event any) {
	if n.cfg.DisputeEventsURL == "" {
		return
	}
	body, _ := json.Marshal(event)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.cfg.DisputeEventsURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(cardnet.EventSignatureHeader, cardnet.SignEvent(body, n.cfg.DisputeEventsSecret, n.cfg.Now()))
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		n.cfg.Logger.WarnContext(ctx, "telling the acquirer of a dispute", "error", err)
		return
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		n.cfg.Logger.WarnContext(ctx, "the acquirer refused a dispute event", "status", resp.StatusCode)
	}
}

// disputeRoutes serve the acquirer's side of disputes and the issuers' controls:
//
//	GET  /v1/acquirers/{acquirer}/disputes?since=RFC3339 the cases changed since
//	GET  /v1/acquirers/{acquirer}/disputes/{id}          a case as it stands
//	GET  /v1/acquirers/{acquirer}/fraud-reports/{id}     a fraud report
//	POST /v1/acquirers/{acquirer}/disputes/{id}/actions  represent, escalate or accept
//	POST /admin/disputes                                 an issuer opens a chargeback
//	POST /admin/fraud-reports                            an issuer reports fraud
func (n *Network) disputeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/acquirers/{acquirer}/disputes", n.acquirerOnly(func(w http.ResponseWriter, r *http.Request) {
		since, err := time.Parse(time.RFC3339, r.URL.Query().Get("since"))
		if err != nil {
			http.Error(w, "since must be RFC 3339", http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"disputes": n.DisputesSince(r.Context(), r.PathValue("acquirer"), since, maxCasesListed)})
	}))
	mux.HandleFunc("GET /v1/acquirers/{acquirer}/fraud-reports/{id}", n.acquirerOnly(func(w http.ResponseWriter, r *http.Request) {
		report, ok := n.FraudReport(r.PathValue("acquirer"), r.PathValue("id"))
		if !ok {
			http.Error(w, "no such fraud report", http.StatusNotFound)
			return
		}
		writeJSON(w, report)
	}))
	mux.HandleFunc("GET /v1/acquirers/{acquirer}/disputes/{id}", n.acquirerOnly(func(w http.ResponseWriter, r *http.Request) {
		d, err := n.DisputeCase(r.Context(), r.PathValue("acquirer"), r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		}
		writeJSON(w, d)
	}))
	mux.HandleFunc("POST /v1/acquirers/{acquirer}/disputes/{id}/actions", n.acquirerOnly(func(w http.ResponseWriter, r *http.Request) {
		var a cardnet.DisputeAction
		if json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&a) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d, err := n.Act(r.Context(), r.PathValue("acquirer"), r.PathValue("id"), a)
		switch {
		case errors.Is(err, errNoDispute):
			http.Error(w, err.Error(), http.StatusNotFound)
		case err != nil:
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			writeJSON(w, d)
		}
	}))
	n.disputeAdminRoutes(mux)
}

// acquirerOnly asks an acquirer's dispute requests for its token, when one is set.
func (n *Network) acquirerOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if n.cfg.AcquirerToken != "" && subtle.ConstantTimeCompare([]byte(got), []byte(n.cfg.AcquirerToken)) != 1 {
			http.Error(w, "an acquirer token is required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// FraudReport is one of an acquirer's fraud reports.
func (n *Network) FraudReport(acquirer, id string) (cardnet.FraudReport, bool) {
	n.issuer.mu.Lock()
	defer n.issuer.mu.Unlock()
	for _, r := range n.book.reports {
		if r.ID == id && r.Acquirer == cardnet.PadAcquirer(acquirer) {
			return r, true
		}
	}
	return cardnet.FraudReport{}, false
}

func (n *Network) disputeAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/disputes", func(w http.ResponseWriter, r *http.Request) {
		var p DisputeParams
		if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&p) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		d, err := n.OpenDispute(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(d)
	})
	mux.HandleFunc("POST /admin/fraud-reports", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			NetworkTransactionID string `json:"network_transaction_id"`
			FraudType            string `json:"fraud_type"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&body) != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		report, err := n.ReportFraud(r.Context(), body.NetworkTransactionID, body.FraudType)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(report)
	})
}
