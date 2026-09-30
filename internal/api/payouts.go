package api

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

const (
	pointPayingOut = "paying_out"
	statePayout    = "payout"
)

func (a *API) payoutOperations() []operation {
	send := phase{point: pointPayingOut, foreign: a.sendPayout, atomic: a.finishPayout}
	return []operation{
		{name: "create_payout", scope: merchant.ScopePayoutsWrite, phases: []phase{{point: pointStarted, atomic: a.createPayout}, send}},
	}
}

func (a *API) CreatePayout(w http.ResponseWriter, r *http.Request, _ openapi.CreatePayoutParams) {
	a.serve(w, r, "create_payout", "")
}

func (a *API) GetPayout(w http.ResponseWriter, r *http.Request, payoutID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopePayoutsRead)
	if !ok {
		return
	}
	parsed, err := payments.PayoutPrefix.Parse(payoutID)
	if err != nil {
		a.writeError(w, r, notFound("payout", payoutID))
		return
	}
	payout, err := a.deps.Payments.Payout(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, paymentsError(err, payoutID))
		return
	}
	a.writeJSON(w, r, payoutJSON(payout))
}

func (a *API) ListPayouts(w http.ResponseWriter, r *http.Request, params openapi.ListPayoutsParams) {
	p, ok := a.authorize(w, r, merchant.ScopePayoutsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	payouts, more, err := a.deps.Payments.Payouts(r.Context(), a.deps.Pool, paymentsOwner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.PayoutList{Object: "list", Url: "/v1/payouts", HasMore: more, Data: make([]openapi.Payout, 0, len(payouts))}
	for _, payout := range payouts {
		list.Data = append(list.Data, payoutJSON(payout))
	}
	a.writeJSON(w, r, list)
}

func (a *API) createPayout(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreatePayoutRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	amount, err := parseAmount(body.Amount, string(body.Currency), "amount")
	if err != nil {
		return outcome{}, err
	}
	payout, err := a.deps.Payments.CreatePayout(ctx, tx, paymentsOwner(r.principal), payments.PayoutParams{
		Amount: amount, PixKey: body.Destination.PixKey, Description: deref(body.Description),
	})
	if err != nil {
		return outcome{}, paymentsError(err, "")
	}
	r.state[statePayout] = payout.ID.String()
	return proceed(pointPayingOut)
}

func (a *API) sendPayout(ctx context.Context, r *request) error {
	t, err := a.deps.Payments.SendPayout(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, statePayout))
	r.scratch = t
	return err
}

func (a *API) finishPayout(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	t, _ := r.scratch.(payments.PixTransfer)
	payout, err := a.deps.Payments.FinishPayout(ctx, tx, paymentsOwner(r.principal), stateID(r, statePayout), t)
	if err != nil {
		return outcome{}, err
	}
	return respond(payoutJSON(payout))
}
