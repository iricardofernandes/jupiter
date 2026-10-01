package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

const (
	pointAnswering = "answering"
	stateDispute   = "dispute"
)

func (a *API) disputeOperations() []operation {
	write := merchant.ScopeDisputesWrite
	send := phase{point: pointAnswering, foreign: a.sendDispute, atomic: a.renderDispute}
	return []operation{
		{name: "update_dispute", scope: write, phases: []phase{{point: pointStarted, atomic: a.updateDispute}, send}},
		{name: "close_dispute", scope: write, phases: []phase{{point: pointStarted, atomic: a.closeDispute}, send}},
		{name: "create_test_dispute", scope: write, phases: []phase{{point: pointStarted, atomic: a.createTestDispute}, send}},
		{name: "create_test_fraud_report", scope: write, phases: []phase{{point: pointStarted, atomic: a.createTestFraudReport}}},
	}
}

func (a *API) UpdateDispute(w http.ResponseWriter, r *http.Request, disputeID openapi.ID, _ openapi.UpdateDisputeParams) {
	a.serve(w, r, "update_dispute", disputeID)
}

func (a *API) CloseDispute(w http.ResponseWriter, r *http.Request, disputeID openapi.ID, _ openapi.CloseDisputeParams) {
	a.serve(w, r, "close_dispute", disputeID)
}

func (a *API) CreateTestDispute(w http.ResponseWriter, r *http.Request, _ openapi.CreateTestDisputeParams) {
	a.serve(w, r, "create_test_dispute", "")
}

func (a *API) CreateTestFraudReport(w http.ResponseWriter, r *http.Request, _ openapi.CreateTestFraudReportParams) {
	a.serve(w, r, "create_test_fraud_report", "")
}

func (a *API) updateDispute(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.UpdateDisputeRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	disputeID, err := a.disputeID(r.pathID)
	if err != nil {
		return outcome{}, err
	}
	var evidence disputes.Evidence
	if body.Evidence != nil {
		evidence = evidenceParam(*body.Evidence)
	}
	d, err := a.deps.Disputes.Update(ctx, tx, paymentsOwner(r.principal), disputeID, evidence, body.Submit != nil && *body.Submit)
	if err != nil {
		return outcome{}, disputesError(err, r.pathID)
	}
	r.state[stateDispute] = d.ID.String()
	return proceed(pointAnswering)
}

func (a *API) closeDispute(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	disputeID, err := a.disputeID(r.pathID)
	if err != nil {
		return outcome{}, err
	}
	d, err := a.deps.Disputes.Close(ctx, tx, paymentsOwner(r.principal), disputeID)
	if err != nil {
		return outcome{}, disputesError(err, r.pathID)
	}
	r.state[stateDispute] = d.ID.String()
	return proceed(pointAnswering)
}

func (a *API) createTestDispute(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	if r.principal.Livemode {
		return outcome{}, invalidRequest("livemode_unsupported", "", "Test helpers are only available in test mode.")
	}
	var body openapi.CreateTestDisputeRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	intentID, err := payments.IntentPrefix.Parse(body.PaymentIntent)
	if err != nil {
		return outcome{}, notFound("payment_intent", body.PaymentIntent)
	}
	d, err := a.deps.Disputes.CreateTestDispute(ctx, tx, paymentsOwner(r.principal), intentID, body.ReasonCode, valueOf(body.Amount))
	if err != nil {
		return outcome{}, disputesError(err, body.PaymentIntent)
	}
	r.state[stateDispute] = d.ID.String()
	return proceed(pointAnswering)
}

func (a *API) createTestFraudReport(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	if r.principal.Livemode {
		return outcome{}, invalidRequest("livemode_unsupported", "", "Test helpers are only available in test mode.")
	}
	var body openapi.CreateTestFraudReportRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	intentID, err := payments.IntentPrefix.Parse(body.PaymentIntent)
	if err != nil {
		return outcome{}, notFound("payment_intent", body.PaymentIntent)
	}
	f, err := a.deps.Disputes.CreateTestFraudReport(ctx, tx, paymentsOwner(r.principal), intentID, string(body.FraudType))
	if err != nil {
		return outcome{}, disputesError(err, body.PaymentIntent)
	}
	return respond(fraudReportJSON(f))
}

// sendDispute sends what the dispute has pending to the network or the bank. One that
// cannot be sent now is left to the worker: the dispute answers as it stands.
func (a *API) sendDispute(ctx context.Context, r *request) error {
	if err := a.deps.Disputes.Send(ctx, a.deps.Pool, r.state[stateDispute]); err != nil {
		a.deps.Logger.WarnContext(ctx, "sending a dispute's answer", "dispute", r.state[stateDispute], "error", err)
	}
	return nil
}

func (a *API) renderDispute(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	disputeID, err := disputes.DisputePrefix.Parse(r.state[stateDispute])
	if err != nil {
		return outcome{}, err
	}
	d, err := a.deps.Disputes.Get(ctx, tx, paymentsOwner(r.principal), disputeID)
	if err != nil {
		return outcome{}, err
	}
	return respond(disputeJSON(d))
}

func (a *API) GetDispute(w http.ResponseWriter, r *http.Request, disputeID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeDisputesRead)
	if !ok {
		return
	}
	parsed, err := a.disputeID(disputeID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	d, err := a.deps.Disputes.Get(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, disputesError(err, disputeID))
		return
	}
	a.writeJSON(w, r, disputeJSON(d))
}

func (a *API) ListDisputes(w http.ResponseWriter, r *http.Request, params openapi.ListDisputesParams) {
	p, ok := a.authorize(w, r, merchant.ScopeDisputesRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	found, more, err := a.deps.Disputes.List(r.Context(), a.deps.Pool, paymentsOwner(p), valueOf(params.PaymentIntent), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.DisputeList{Object: "list", Url: "/v1/disputes", HasMore: more, Data: make([]openapi.Dispute, 0, len(found))}
	for _, d := range found {
		list.Data = append(list.Data, disputeJSON(d))
	}
	a.writeJSON(w, r, list)
}

func (a *API) ListFraudReports(w http.ResponseWriter, r *http.Request, params openapi.ListFraudReportsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeDisputesRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	found, more, err := a.deps.Disputes.FraudReports(r.Context(), a.deps.Pool, paymentsOwner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.FraudReportList{Object: "list", Url: "/v1/fraud_reports", HasMore: more, Data: make([]openapi.FraudReport, 0, len(found))}
	for _, f := range found {
		list.Data = append(list.Data, fraudReportJSON(f))
	}
	a.writeJSON(w, r, list)
}

func (a *API) GetDisputeMonitoring(w http.ResponseWriter, r *http.Request, params openapi.GetDisputeMonitoringParams) {
	p, ok := a.authorize(w, r, merchant.ScopeDisputesRead)
	if !ok {
		return
	}
	month := a.deps.Now()
	if params.Month != nil {
		parsed, err := time.Parse("2006-01", *params.Month)
		if err != nil {
			a.writeError(w, r, invalidRequest("parameter_invalid", "month", "month is YYYY-MM."))
			return
		}
		// The middle of the month, wherever its start falls in Brasília.
		month = parsed.AddDate(0, 0, 14)
	}
	m, err := a.deps.Disputes.Monitor(r.Context(), a.deps.Pool, paymentsOwner(p), month)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, openapi.DisputeMonitoring{
		Object: "dispute_monitoring", Livemode: p.Livemode, Month: m.Month, Transactions: m.Transactions, Disputes: m.Disputes,
		FraudReports: m.FraudReports, RatioBps: m.RatioBps, ThresholdBps: m.Threshold.Bps, MinimumCount: m.Threshold.MinimumCount,
		Excessive: m.Excessive,
	})
}

func (a *API) disputeID(raw string) (id.ID, error) {
	parsed, err := disputes.DisputePrefix.Parse(raw)
	if err != nil {
		return id.ID{}, notFound("dispute", raw)
	}
	return parsed, nil
}

func evidenceParam(e openapi.DisputeEvidence) disputes.Evidence {
	return disputes.Evidence{
		ProductDescription: deref(e.ProductDescription), CustomerName: deref(e.CustomerName), CustomerEmail: deref(e.CustomerEmail),
		CustomerCommunication: deref(e.CustomerCommunication), ShippingTrackingNumber: deref(e.ShippingTrackingNumber),
		RefundPolicy: deref(e.RefundPolicy), UncategorizedText: deref(e.UncategorizedText),
	}
}

func disputeJSON(d disputes.Dispute) openapi.Dispute {
	e := d.Evidence
	out := openapi.Dispute{
		Id: d.ID.String(), Object: "dispute", Livemode: d.Owner.Livemode, PaymentIntent: d.PaymentIntent, Kind: openapi.DisputeKind(d.Kind),
		Network: d.Network, Reason: d.Reason, ReasonCode: d.ReasonCode, Amount: d.Amount.Minor(), Currency: strings.ToLower(d.Amount.Currency().Code()),
		Stage: openapi.DisputeStage(d.Stage), Status: openapi.DisputeStatus(d.Status), Liability: openapi.DisputeLiability(d.Liability),
		Funds: openapi.DisputeFunds(d.Funds), Created: d.CreatedAt.Unix(), History: make([]openapi.DisputeChange, 0, len(d.History)),
		Evidence: openapi.DisputeEvidence{
			ProductDescription: optional(e.ProductDescription), CustomerName: optional(e.CustomerName), CustomerEmail: optional(e.CustomerEmail),
			CustomerCommunication: optional(e.CustomerCommunication), ShippingTrackingNumber: optional(e.ShippingTrackingNumber),
			RefundPolicy: optional(e.RefundPolicy), UncategorizedText: optional(e.UncategorizedText),
		},
		Outcome: optional(d.Outcome), DueBy: unixOrNil(d.DueBy), EvidenceSubmitted: unixOrNil(d.EvidenceSubmittedAt), Closed: unixOrNil(d.ClosedAt),
	}
	if d.Kind == disputes.KindMED {
		held := d.Blocked
		out.Held = &held
		trace := make([]openapi.DisputeTraceHop, 0, len(d.Trace))
		for _, h := range d.Trace {
			trace = append(trace, openapi.DisputeTraceHop{Payout: h.Payout, EndToEndId: h.EndToEndID, Amount: h.Amount})
		}
		out.Trace = &trace
	}
	for _, c := range d.History {
		out.History = append(out.History, openapi.DisputeChange{At: c.At.Unix(), Kind: c.Kind, Detail: c.Detail})
	}
	return out
}

func fraudReportJSON(f disputes.FraudReport) openapi.FraudReport {
	return openapi.FraudReport{
		Id: f.ID.String(), Object: "fraud_report", Livemode: f.Owner.Livemode, PaymentIntent: f.PaymentIntent, Network: f.Network,
		FraudType: f.FraudType, Amount: f.Amount.Minor(), Currency: strings.ToLower(f.Amount.Currency().Code()), Reported: f.ReportedAt.Unix(),
	}
}

func unixOrNil(t time.Time) *int64 {
	if t.IsZero() {
		return nil
	}
	u := t.Unix()
	return &u
}

func disputesError(err error, objectID string) error {
	for sentinel, code := range map[error]string{
		disputes.ErrInvalid:      "parameter_invalid",
		disputes.ErrInvalidState: "dispute_unexpected_state",
		disputes.ErrNoNetwork:    "livemode_unsupported",
	} {
		if errors.Is(err, sentinel) {
			return invalidRequest(code, "", "%s", strings.TrimPrefix(err.Error(), sentinel.Error()+": "))
		}
	}
	if errors.Is(err, disputes.ErrNotFound) {
		return notFound("dispute", objectID)
	}
	return paymentsError(err, objectID)
}
