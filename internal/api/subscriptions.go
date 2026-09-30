package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
)

const (
	pointSettingUp      = "setting_up"
	pointCancelingAtPSP = "canceling_at_bank"
	stateSubscription   = "subscription"
)

func (a *API) subscriptionOperations() []operation {
	setUp := phase{point: pointSettingUp, foreign: a.setUpSubscription, atomic: a.finishSetUp}
	cancel := phase{point: pointCancelingAtPSP, foreign: a.cancelAtBank, atomic: a.applyCancel}
	write := merchant.ScopeSubscriptionsWrite
	return []operation{
		{name: "create_subscription", scope: write, phases: []phase{{point: pointStarted, atomic: a.createSubscription}, setUp}},
		{name: "cancel_subscription", scope: write, phases: []phase{{point: pointStarted, atomic: a.startCancelSubscription}, cancel}},
	}
}

func (a *API) CreateSubscription(w http.ResponseWriter, r *http.Request, _ openapi.CreateSubscriptionParams) {
	a.serve(w, r, "create_subscription", "")
}

func (a *API) CancelSubscription(w http.ResponseWriter, r *http.Request, subscriptionID openapi.ID, _ openapi.CancelSubscriptionParams) {
	a.serve(w, r, "cancel_subscription", subscriptionID)
}

func (a *API) GetSubscription(w http.ResponseWriter, r *http.Request, subscriptionID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeSubscriptionsRead)
	if !ok {
		return
	}
	parsed, err := subscriptions.Prefix.Parse(subscriptionID)
	if err != nil {
		a.writeError(w, r, notFound("subscription", subscriptionID))
		return
	}
	sub, err := a.deps.Subscriptions.Get(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, subscriptionsError(err, subscriptionID))
		return
	}
	a.writeJSON(w, r, subscriptionJSON(sub))
}

func (a *API) ListSubscriptions(w http.ResponseWriter, r *http.Request, params openapi.ListSubscriptionsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeSubscriptionsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	subs, more, err := a.deps.Subscriptions.List(r.Context(), a.deps.Pool, paymentsOwner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.SubscriptionList{Object: "list", Url: "/v1/subscriptions", HasMore: more, Data: make([]openapi.Subscription, 0, len(subs))}
	for _, sub := range subs {
		list.Data = append(list.Data, subscriptionJSON(sub))
	}
	a.writeJSON(w, r, list)
}

func (a *API) createSubscription(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateSubscriptionRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	amount, err := parseAmount(body.Amount, string(body.Currency), "amount")
	if err != nil {
		return outcome{}, err
	}
	params := subscriptions.Params{
		Amount: amount, Interval: subscriptions.Interval(body.Interval), StartDate: body.StartDate.String(),
		Description: body.Description, Customer: subscriptions.Customer{Name: body.Customer.Name, TaxID: body.Customer.TaxId},
		Retries:       body.Retries == nil || *body.Retries == openapi.CreateSubscriptionRequestRetriesThreeInSeven,
		Authorization: subscriptions.Authorization{Method: string(body.Authorization.Method)},
	}
	if body.EndDate != nil {
		params.EndDate = body.EndDate.String()
	}
	if b := body.Authorization.PayerBank; b != nil {
		params.Authorization.PayerISPB, params.Authorization.PayerAccount = b.Ispb, b.Account
		if b.Branch != nil {
			params.Authorization.PayerBranch = *b.Branch
		}
	}
	sub, err := a.deps.Subscriptions.Create(ctx, tx, paymentsOwner(r.principal), params)
	if err != nil {
		return outcome{}, subscriptionsError(err, "")
	}
	r.state[stateSubscription] = sub.ID.String()
	return proceed(pointSettingUp)
}

// setUpSubscription asks the bank for the recurrence. A failure leaves the subscription
// incomplete, and the worker sets it up later: the request still answers with it.
func (a *API) setUpSubscription(ctx context.Context, r *request) error {
	rec, err := a.deps.Subscriptions.SetUp(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateSubscription))
	if err != nil {
		a.deps.Logger.WarnContext(ctx, "setting up a subscription at the bank", "subscription", r.state[stateSubscription], "error", err)
	}
	r.scratch = rec
	return nil
}

func (a *API) finishSetUp(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	rec, _ := r.scratch.(subscriptions.Recurrence)
	sub, err := a.deps.Subscriptions.FinishSetUp(ctx, tx, paymentsOwner(r.principal), stateID(r, stateSubscription), rec)
	if err != nil {
		return outcome{}, err
	}
	return respond(subscriptionJSON(sub))
}

func (a *API) startCancelSubscription(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	subscriptionID, err := subscriptions.Prefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("subscription", r.pathID)
	}
	sub, err := a.deps.Subscriptions.StartCancel(ctx, tx, paymentsOwner(r.principal), subscriptionID)
	if err != nil {
		return outcome{}, subscriptionsError(err, r.pathID)
	}
	r.state[stateSubscription] = sub.ID.String()
	return proceed(pointCancelingAtPSP)
}

func (a *API) cancelAtBank(ctx context.Context, r *request) error {
	rec, err := a.deps.Subscriptions.CancelAtBank(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateSubscription))
	r.scratch = rec
	return err
}

func (a *API) applyCancel(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	rec, _ := r.scratch.(subscriptions.Recurrence)
	sub, err := a.deps.Subscriptions.ApplyRecurrence(ctx, tx, paymentsOwner(r.principal), stateID(r, stateSubscription), rec)
	if err != nil {
		return outcome{}, err
	}
	return respond(subscriptionJSON(sub))
}

func subscriptionsError(err error, objectID string) error {
	for sentinel, code := range map[error]string{
		subscriptions.ErrInvalid:         "parameter_invalid",
		subscriptions.ErrInvalidState:    "subscription_unexpected_state",
		subscriptions.ErrBankUnavailable: "livemode_unsupported",
	} {
		if errors.Is(err, sentinel) {
			return invalidRequest(code, "", "%s", strings.TrimPrefix(err.Error(), sentinel.Error()+": "))
		}
	}
	if errors.Is(err, subscriptions.ErrNotFound) {
		return notFound("subscription", objectID)
	}
	return err
}

func subscriptionJSON(s subscriptions.Subscription) openapi.Subscription {
	out := openapi.Subscription{
		Id: s.ID.String(), Object: "subscription", Livemode: s.Owner.Livemode, Status: openapi.SubscriptionStatus(s.Status),
		Amount: s.Amount.Minor(), Currency: strings.ToLower(s.Amount.Currency().Code()), Interval: openapi.SubscriptionInterval(s.Interval),
		Description: s.Description, Customer: openapi.SubscriptionCustomer{Name: s.Customer.Name, TaxId: s.Customer.TaxID},
		Retries: "none", Authorization: openapi.SubscriptionAuthorization{Method: openapi.SubscriptionAuthorizationMethod(s.Authorization.Method)},
		Cycles: make([]openapi.SubscriptionCycle, 0, len(s.Cycles)), Created: s.CreatedAt.Unix(),
	}
	_ = out.StartDate.UnmarshalText([]byte(s.StartDate))
	if s.EndDate != "" {
		_ = allocate(&out.EndDate).UnmarshalText([]byte(s.EndDate))
	}
	if s.Retries {
		out.Retries = "three_in_seven"
	}
	if a := s.Authorization; a.Method == subscriptions.ByPayerRequest {
		bank := allocate(&out.Authorization.PayerBank)
		bank.Ispb, bank.Account, bank.Branch = a.PayerISPB, a.PayerAccount, optional(a.PayerBranch)
	}
	switch {
	case s.QRCode != "":
		out.NextAction = &openapi.NextAction{Type: "pix_display_qr_code"}
		qr := allocate(&out.NextAction.PixDisplayQrCode)
		qr.Data = s.QRCode
	case s.Status == subscriptions.Incomplete && s.Authorization.Method == subscriptions.ByPayerRequest && s.RecurrenceID != "":
		out.NextAction = &openapi.NextAction{Type: "awaiting_payer_authorization"}
	}
	if s.CanceledBy != "" {
		by := openapi.SubscriptionCanceledBy(s.CanceledBy)
		out.CanceledBy = &by
	}
	for _, c := range s.Cycles {
		cycle := openapi.SubscriptionCycle{Number: c.Number, Status: openapi.SubscriptionCycleStatus(c.Status), PaymentIntent: optional(c.PaymentIntent)}
		_ = cycle.DueDate.UnmarshalText([]byte(c.DueDate))
		out.Cycles = append(out.Cycles, cycle)
	}
	return out
}
