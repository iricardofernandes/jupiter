package api

import (
	"net/http"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
)

func (a *API) GetReconciliationReport(w http.ResponseWriter, r *http.Request, date openapi_types.Date) {
	p, ok := a.authorize(w, r, merchant.ScopeReconciliationRead)
	if !ok {
		return
	}
	report, err := a.deps.Reconciliation.Report(r.Context(), a.deps.Pool, p.Livemode, date.Time, p.Merchant.String())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := openapi.ReconciliationReport{
		Object: "reconciliation_report", Livemode: p.Livemode, Date: date,
		Streams: make([]openapi.ReconciliationStream, 0, len(report.Streams)), Breaks: make([]openapi.ReconciliationBreak, 0, len(report.Breaks)),
	}
	for _, s := range report.Streams {
		st := openapi.ReconciliationStream{
			Counterparty: s.Counterparty, Stream: s.Stream, Matched: s.Matched, MatchedAmount: s.Amount, Open: s.Open, Opened: s.OpenedOnDay,
			Resolved: s.ResolvedOn,
		}
		st.Ageing.UpTo1Day, st.Ageing.UpTo7Days, st.Ageing.UpTo30Days, st.Ageing.Older = s.Ageing[0], s.Ageing[1], s.Ageing[2], s.Ageing[3]
		out.Streams = append(out.Streams, st)
	}
	for _, b := range report.Breaks {
		out.Breaks = append(out.Breaks, breakJSON(b, report.Day))
	}
	a.writeJSON(w, r, out)
}

func (a *API) ListReconciliationBreaks(w http.ResponseWriter, r *http.Request, params openapi.ListReconciliationBreaksParams) {
	p, ok := a.authorize(w, r, merchant.ScopeReconciliationRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	status := ""
	if params.Status != nil {
		status = string(*params.Status)
	}
	found, more, err := a.deps.Reconciliation.Breaks(r.Context(), a.deps.Pool, p.Livemode, p.Merchant.String(), status, pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	today := reconciliation.Day(a.deps.Now())
	list := openapi.ReconciliationBreakList{Object: "list", Url: "/v1/reconciliation/breaks", HasMore: more, Data: make([]openapi.ReconciliationBreak, 0, len(found))}
	for _, b := range found {
		list.Data = append(list.Data, breakJSON(b, today))
	}
	a.writeJSON(w, r, list)
}

func breakJSON(b reconciliation.Break, today time.Time) openapi.ReconciliationBreak {
	out := openapi.ReconciliationBreak{
		Id: b.ID.String(), Object: "reconciliation_break", Livemode: b.Livemode, Counterparty: b.Counterparty, Stream: b.Stream,
		Kind: openapi.ReconciliationBreakKind(b.Kind), Key: optional(b.Key), Amount: b.Amount, Detail: optional(b.Detail), Reasons: b.Reasons,
		Status: openapi.ReconciliationBreakStatus(b.Status), OpenedOn: openapi_types.Date{Time: b.OpenedOn},
		AgeDays: b.Age(today),
	}
	if b.Reasons == nil {
		out.Reasons = []string{}
	}
	if code := reconciliation.ResolutionCode(b.Resolution); code != "" {
		resolution := openapi.ReconciliationBreakResolution(code)
		out.Resolution = &resolution
	}
	if b.Score > 0 {
		score := b.Score
		out.Score = &score
	}
	if !b.ValueDate.IsZero() {
		out.ValueDate = &openapi_types.Date{Time: b.ValueDate}
	}
	if !b.ResolvedOn.IsZero() {
		out.ResolvedOn = &openapi_types.Date{Time: b.ResolvedOn}
	}
	return out
}
