package acquirer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

// Disputes reach the acquirer over HTTP: the network tells it of every change to a case
// and of every fraud report, in signed events, and the acquirer answers each case at its
// stage. An event is only a hint: what it names is read back from the network before it
// is applied. A case is live mode's: test mode has no network.

var _ disputes.Network = (*Connector)(nil)

// DisputeEventsPath is where the network sends dispute and fraud report events, on
// Jupiter's public address.
const DisputeEventsPath = "/network/v1/dispute-events"

const (
	maxDisputeEvent = 64 << 10
	maxCases        = 8 << 20
)

func (c *Connector) Act(ctx context.Context, a disputes.Action) (disputes.Notice, error) {
	var out cardnet.Dispute
	err := c.disputeCall(ctx, http.MethodPost, "/disputes/"+url.PathEscape(a.NetworkID)+"/actions",
		cardnet.DisputeAction{Action: a.Kind, Stage: a.Stage, Evidence: a.Evidence, Reason: a.Reason}, &out)
	return noticeOf(out), err
}

func (c *Connector) Case(ctx context.Context, networkID string) (disputes.Notice, error) {
	var out cardnet.Dispute
	err := c.disputeCall(ctx, http.MethodGet, "/disputes/"+url.PathEscape(networkID), nil, &out)
	return noticeOf(out), err
}

func (c *Connector) Cases(ctx context.Context, since time.Time) ([]disputes.Notice, error) {
	var out struct {
		Disputes []cardnet.Dispute `json:"disputes"`
	}
	if err := c.disputeCall(ctx, http.MethodGet, "/disputes?since="+url.QueryEscape(since.UTC().Format(time.RFC3339)), nil, &out); err != nil {
		return nil, err
	}
	notices := make([]disputes.Notice, 0, len(out.Disputes))
	for _, d := range out.Disputes {
		if d.Valid() {
			notices = append(notices, noticeOf(d))
		}
	}
	return notices, nil
}

// disputeCall sends one request about the acquirer's cases. A 4xx is the network's
// refusal, but for a timeout, too many requests or a token it did not take; anything
// else that is not a success may be tried again.
func (c *Connector) disputeCall(ctx context.Context, method, path string, body, out any) error {
	if c.cfg.NetworkURL == "" {
		return disputes.ErrNoNetwork
	}
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	full := c.cfg.NetworkURL + "/v1/acquirers/" + cardnet.PadAcquirer(c.cfg.AcquirerID) + path
	req, err := http.NewRequestWithContext(ctx, method, full, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	c.authorize(req)
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("the network's dispute system: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, maxCases))
	if err != nil {
		return err
	}
	status := resp.StatusCode
	switch {
	case status == http.StatusOK:
		if err := json.Unmarshal(answer, out); err != nil {
			return fmt.Errorf("the network answered %s with what is not a case: %w", path, err)
		}
		if d, ok := out.(*cardnet.Dispute); ok && !d.Valid() {
			return fmt.Errorf("the network answered %s with a case that is not one", path)
		}
		return nil
	case status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests && status != http.StatusUnauthorized:
		return fmt.Errorf("%w: %s %d", disputes.ErrRefused, path, status)
	default:
		return fmt.Errorf("the network answered %s with %d", path, status)
	}
}

func noticeOf(d cardnet.Dispute) disputes.Notice {
	return disputes.Notice{
		NetworkID: d.ID, NetworkTransactionID: d.NetworkTransactionID, Amount: d.Amount, Currency: d.Currency,
		ReasonCode: d.ReasonCode, Category: d.Category, Stage: d.Stage, Status: d.Status, Outcome: d.Outcome,
		RespondBy: d.RespondBy, AuthorizedAt: d.AuthorizedAt, OpenedAt: d.OpenedAt, Version: d.Version,
	}
}

// disputeEvents takes the network's dispute and fraud report events, signed with the
// dispute events secret. Each names a case or a report of this acquirer's, which is read
// back from the network and applied as the network has it.
func (c *Connector) disputeEvents(d *disputes.Service) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxDisputeEvent))
		if err != nil {
			http.Error(w, "unreadable", http.StatusBadRequest)
			return
		}
		if err := cardnet.VerifyEvent(body, r.Header.Get(cardnet.EventSignatureHeader), c.cfg.DisputeEventsSecret, eventsTolerance, c.cfg.Now()); err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var e struct {
			Type    string              `json:"type"`
			Dispute cardnet.Dispute     `json:"dispute"`
			Report  cardnet.FraudReport `json:"report"`
		}
		if err := json.Unmarshal(body, &e); err != nil {
			http.Error(w, "not an event", http.StatusBadRequest)
			return
		}
		switch {
		case e.Type == "dispute.updated" && e.Dispute.ID != "" && e.Dispute.Acquirer == cardnet.PadAcquirer(c.cfg.AcquirerID):
			err = c.applyCase(r.Context(), d, e.Dispute.ID)
		case e.Type == "fraud_report.created" && e.Report.ID != "" && e.Report.Acquirer == cardnet.PadAcquirer(c.cfg.AcquirerID):
			err = c.applyFraudReport(r.Context(), d, e.Report.ID)
		default:
			http.Error(w, "not an event of this acquirer", http.StatusBadRequest)
			return
		}
		if err != nil {
			c.cfg.Logger.ErrorContext(r.Context(), "applying a dispute event", "type", e.Type, "error", err)
			http.Error(w, "not applied", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func (c *Connector) applyCase(ctx context.Context, d *disputes.Service, id string) error {
	n, err := c.Case(ctx, id)
	if err != nil {
		return err
	}
	return d.ApplyNotice(ctx, c.cfg.Pool, true, n)
}

func (c *Connector) applyFraudReport(ctx context.Context, d *disputes.Service, id string) error {
	var report cardnet.FraudReport
	if err := c.disputeCall(ctx, http.MethodGet, "/fraud-reports/"+url.PathEscape(id), nil, &report); err != nil {
		return err
	}
	if report.ID != id || report.Acquirer != cardnet.PadAcquirer(c.cfg.AcquirerID) {
		return fmt.Errorf("the network answered fraud report %s with another", id)
	}
	return d.ApplyFraudReport(ctx, c.cfg.Pool, true, disputes.ReportedFraud{
		NetworkID: report.ID, NetworkTransactionID: report.NetworkTransactionID, FraudType: report.FraudType, ReportedAt: report.ReportedAt,
	})
}
