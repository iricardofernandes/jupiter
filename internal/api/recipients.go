package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
)

const (
	pointAnticipating = "anticipating"
	pointQuoting      = "quoting"
	stateAnticipation = "anticipation"
	stateRecipient    = "recipient"
)

func (a *API) recipientOperations() []operation {
	write := merchant.ScopeRecipientsWrite
	anticipate := phase{point: pointAnticipating, foreign: a.anticipate, atomic: a.anticipated}
	return []operation{
		{name: "create_recipient", scope: write, phases: []phase{{point: pointStarted, atomic: a.createRecipient}}},
		{name: "update_recipient", scope: write, phases: []phase{{point: pointStarted, atomic: a.updateRecipient}}},
		{name: "verify_recipient", scope: write, phases: []phase{{point: pointStarted, atomic: a.verifyRecipient}}},
		{name: "simulate_anticipation", scope: merchant.ScopeReceivablesWrite, phases: []phase{
			{point: pointStarted, atomic: a.startQuote}, {point: pointQuoting, foreign: a.readRegistry, atomic: a.quote},
		}},
		{name: "create_anticipation", scope: merchant.ScopeReceivablesWrite, phases: []phase{{point: pointStarted, atomic: a.startAnticipation}, anticipate}},
	}
}

func (a *API) CreateRecipient(w http.ResponseWriter, r *http.Request, _ openapi.CreateRecipientParams) {
	a.serve(w, r, "create_recipient", "")
}

func (a *API) UpdateRecipient(w http.ResponseWriter, r *http.Request, recipientID openapi.ID, _ openapi.UpdateRecipientParams) {
	a.serve(w, r, "update_recipient", recipientID)
}

func (a *API) VerifyRecipient(w http.ResponseWriter, r *http.Request, recipientID openapi.ID, _ openapi.VerifyRecipientParams) {
	a.serve(w, r, "verify_recipient", recipientID)
}

func (a *API) CreateAnticipation(w http.ResponseWriter, r *http.Request, _ openapi.CreateAnticipationParams) {
	a.serve(w, r, "create_anticipation", "")
}

func recipientParams(destination *openapi.RecipientDestination, transfers *openapi.TransferSettings, auto *openapi.AutomaticAnticipation) recipients.Params {
	var p recipients.Params
	if d := destination; d != nil {
		p.Destination = &recipients.Destination{Method: string(d.Type), PixKey: valueOf(d.PixKey), ISPB: valueOf(d.Ispb), Branch: valueOf(d.Branch), Account: valueOf(d.Account)}
	}
	if t := transfers; t != nil {
		p.Transfers = &recipients.Transfers{Interval: recipients.Interval(t.Interval), Day: valueOf(t.Day)}
	}
	if au := auto; au != nil {
		p.AutoAnticipation = &recipients.AutoAnticipation{Enabled: au.Enabled, DelayDays: 1}
		if au.DelayDays != nil {
			p.AutoAnticipation.DelayDays = *au.DelayDays
		}
	}
	return p
}

func (a *API) createRecipient(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateRecipientRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	p := recipientParams(body.PayoutDestination, body.TransferSettings, body.AutomaticAnticipation)
	p.Name, p.TaxID = body.Name, body.TaxId
	rec, err := a.deps.Recipients.Create(ctx, tx, owner(r.principal), p)
	if err != nil {
		return outcome{}, recipientsError(err, "")
	}
	return respond(recipientJSON(rec))
}

func (a *API) updateRecipient(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.UpdateRecipientRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	pathID, err := a.resolveRecipient(ctx, tx, r.principal, r.pathID)
	if err != nil {
		return outcome{}, err
	}
	recipientID, err := recipients.Prefix.Parse(pathID)
	if err != nil {
		return outcome{}, notFound("recipient", r.pathID)
	}
	p := recipientParams(body.PayoutDestination, body.TransferSettings, body.AutomaticAnticipation)
	p.Name = valueOf(body.Name)
	rec, err := a.deps.Recipients.Update(ctx, tx, owner(r.principal), recipientID, p)
	if err != nil {
		return outcome{}, recipientsError(err, r.pathID)
	}
	return respond(recipientJSON(rec))
}

func (a *API) verifyRecipient(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	if r.principal.Livemode {
		return outcome{}, invalidRequest("livemode_unsupported", "", "Test helpers are only available in test mode.")
	}
	var body openapi.VerifyRecipientRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	recipientID, err := recipients.Prefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("recipient", r.pathID)
	}
	rec, err := a.deps.Recipients.Verify(ctx, tx, owner(r.principal), recipientID, recipients.Status(body.Status))
	if err != nil {
		return outcome{}, recipientsError(err, r.pathID)
	}
	return respond(recipientJSON(rec))
}

func (a *API) GetRecipient(w http.ResponseWriter, r *http.Request, recipientID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeRecipientsRead)
	if !ok {
		return
	}
	var rec recipients.Recipient
	ctx := r.Context()
	err := postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		resolved, err := a.lookUpRecipient(ctx, tx, p, recipientID)
		if err != nil {
			return err
		}
		parsed, err := recipients.Prefix.Parse(resolved)
		if err != nil {
			return notFound("recipient", recipientID)
		}
		rec, err = a.deps.Recipients.Get(ctx, tx, owner(p), parsed)
		return recipientsError(err, recipientID)
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, recipientJSON(rec))
}

func (a *API) ListRecipients(w http.ResponseWriter, r *http.Request, params openapi.ListRecipientsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeRecipientsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	recs, more, err := a.deps.Recipients.List(r.Context(), a.deps.Pool, owner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.RecipientList{Object: "list", Url: "/v1/recipients", HasMore: more, Data: make([]openapi.Recipient, 0, len(recs))}
	for _, rec := range recs {
		list.Data = append(list.Data, recipientJSON(rec))
	}
	a.writeJSON(w, r, list)
}

func (a *API) GetRecipientBalance(w http.ResponseWriter, r *http.Request, recipientID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeRecipientsRead)
	if !ok {
		return
	}
	var b receivables.Balance
	ctx := r.Context()
	err := postgres.InTx(ctx, a.deps.Pool, func(tx pgx.Tx) error {
		resolved, err := a.lookUpRecipient(ctx, tx, p, recipientID)
		if err != nil {
			return err
		}
		b, err = a.deps.Receivables.Balance(ctx, tx, paymentsOwner(p), resolved, money.BRL)
		return err
	})
	if err != nil {
		a.fail(w, r, receivablesError(err))
		return
	}
	out := openapi.RecipientBalance{
		Object: "recipient_balance", Recipient: b.Recipient, Currency: strings.ToLower(b.Currency.Code()),
		Pending: b.Pending, Available: b.Available, Reserved: b.Reserved,
	}
	out.PendingOn = make([]struct {
		Amount      int64              `json:"amount"`
		AvailableOn openapi_types.Date `json:"available_on"`
	}, len(b.PendingOn))
	for i, d := range b.PendingOn {
		out.PendingOn[i].Amount = d.Amount
		_ = out.PendingOn[i].AvailableOn.UnmarshalText([]byte(d.Date))
	}
	a.writeJSON(w, r, out)
}

func (a *API) SimulateAnticipation(w http.ResponseWriter, r *http.Request) {
	a.serve(w, r, "simulate_anticipation", "")
}

// startQuote names the recipient to quote, made if it is the merchant's own and was not.
func (a *API) startQuote(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.SimulateAnticipationRequest
	if err := decode(r.body, &body, true); err != nil {
		return outcome{}, err
	}
	recipientID, err := a.resolveRecipient(ctx, tx, r.principal, valueOf(body.Recipient))
	if err != nil {
		return outcome{}, err
	}
	if recipientID == "" {
		rec, err := a.deps.Recipients.Default(ctx, tx, owner(r.principal))
		if err != nil {
			return outcome{}, recipientsError(err, me)
		}
		recipientID = rec.ID.String()
	}
	r.state[stateRecipient] = recipientID
	return proceed(pointQuoting)
}

// readRegistry asks the registry what it holds free on the recipient's units, outside
// any transaction: a slow registry must not hold the database's connections.
func (a *API) readRegistry(ctx context.Context, r *request) error {
	free, err := a.deps.Receivables.FreeAtRegistry(ctx, a.deps.Pool, paymentsOwner(r.principal), r.state[stateRecipient])
	r.scratch = free
	return receivablesError(err)
}

func (a *API) quote(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.SimulateAnticipationRequest
	if err := decode(r.body, &body, true); err != nil {
		return outcome{}, err
	}
	free, _ := r.scratch.(receivables.RegistryFree)
	q, err := a.deps.Receivables.Simulate(ctx, tx, paymentsOwner(r.principal), r.state[stateRecipient], valueOf(body.Units), free)
	if err != nil {
		return outcome{}, receivablesError(err)
	}
	return respond(openapi.AnticipationQuote{
		Id: q.ID, Object: "anticipation_quote", Recipient: q.Recipient, Currency: strings.ToLower(q.Currency.Code()),
		MonthlyRate: q.MonthlyRate, Units: anticipatedJSON(q.Units), Amount: q.Amount, Price: q.Price, Fee: q.Amount - q.Price,
		ExpiresAt: q.ExpiresAt.Unix(),
	})
}

func (a *API) startAnticipation(_ context.Context, _ pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateAnticipationRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	r.state[stateAnticipation] = body.Quote
	return proceed(pointAnticipating)
}

// anticipate buys the quoted units: at the registry and on the ledger, in a transaction
// of its own; a repeat answers the anticipation the quote made.
func (a *API) anticipate(ctx context.Context, r *request) error {
	ant, err := a.deps.Receivables.Anticipate(ctx, a.deps.Pool, paymentsOwner(r.principal), r.state[stateAnticipation])
	r.scratch = ant
	if errors.Is(err, receivables.ErrNotFound) {
		return notFound("anticipation_quote", r.state[stateAnticipation])
	}
	return receivablesError(err)
}

func (a *API) anticipated(_ context.Context, _ pgx.Tx, r *request) (outcome, error) {
	ant, _ := r.scratch.(receivables.Anticipation)
	return respond(anticipationJSON(ant, r.principal.Livemode))
}

func (a *API) GetAnticipation(w http.ResponseWriter, r *http.Request, anticipationID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeReceivablesRead)
	if !ok {
		return
	}
	ant, err := a.deps.Receivables.Anticipation(r.Context(), a.deps.Pool, paymentsOwner(p), anticipationID)
	if err != nil {
		a.fail(w, r, receivablesError(err))
		return
	}
	a.writeJSON(w, r, anticipationJSON(ant, p.Livemode))
}

func anticipatedJSON(units []receivables.Anticipated) []openapi.AnticipationUnit {
	out := make([]openapi.AnticipationUnit, 0, len(units))
	for _, u := range units {
		unit := openapi.AnticipationUnit{Unit: u.Unit, Amount: u.Amount, Price: u.Price, Days: u.Days}
		_ = unit.SettlementDate.UnmarshalText([]byte(u.SettlementDate))
		out = append(out, unit)
	}
	return out
}

func anticipationJSON(ant receivables.Anticipation, livemode bool) openapi.Anticipation {
	return openapi.Anticipation{
		Id: ant.ID, Object: "anticipation", Livemode: livemode, Recipient: ant.Recipient, Currency: strings.ToLower(ant.Currency.Code()),
		MonthlyRate: ant.MonthlyRate, Units: anticipatedJSON(ant.Units), Amount: ant.Amount, Price: ant.Price, Fee: ant.Fee(),
		Automatic: ant.Automatic, Created: ant.CreatedAt.Unix(),
	}
}

func recipientJSON(rec recipients.Recipient) openapi.Recipient {
	out := openapi.Recipient{
		Id: rec.ID.String(), Object: "recipient", Livemode: rec.Owner.Livemode, Name: rec.Name, TaxId: rec.TaxID,
		Default: rec.Default, Status: openapi.RecipientStatus(rec.Status), Created: rec.CreatedAt.Unix(),
		TransferSettings:      openapi.TransferSettings{Interval: openapi.TransferSettingsInterval(rec.Transfers.Interval)},
		AutomaticAnticipation: openapi.AutomaticAnticipation{Enabled: rec.AutoAnticipation.Enabled, DelayDays: &rec.AutoAnticipation.DelayDays},
	}
	if rec.Transfers.Day != 0 {
		out.TransferSettings.Day = &rec.Transfers.Day
	}
	if d := rec.Destination; d.Method != "" {
		out.PayoutDestination = &openapi.RecipientDestination{Type: openapi.RecipientDestinationType(d.Method)}
		if d.Method == "pix" {
			out.PayoutDestination.PixKey = &d.PixKey
		} else {
			out.PayoutDestination.Ispb, out.PayoutDestination.Branch, out.PayoutDestination.Account = &d.ISPB, &d.Branch, &d.Account
		}
	}
	return out
}

func recipientsError(err error, recipientID string) error {
	switch {
	case errors.Is(err, recipients.ErrInvalid):
		return invalidRequest("parameter_invalid", "", "%s", strings.TrimPrefix(err.Error(), recipients.ErrInvalid.Error()+": "))
	case errors.Is(err, recipients.ErrNotFound):
		return notFound("recipient", recipientID)
	}
	return err
}

// me names the merchant's own recipient wherever a recipient is named.
const me = "me"

// resolveRecipient turns me into the merchant's own recipient's id, made if it was not.
func (a *API) resolveRecipient(ctx context.Context, tx pgx.Tx, p merchant.Principal, recipientID string) (string, error) {
	if recipientID != me {
		return recipientID, nil
	}
	if a.deps.Recipients == nil {
		return "", invalidRequest("parameter_invalid", "recipient", "Recipients are not available.")
	}
	rec, err := a.deps.Recipients.Default(ctx, tx, owner(p))
	if err != nil {
		return "", recipientsError(err, me)
	}
	return rec.ID.String(), nil
}

// lookUpRecipient is resolveRecipient for reads: it makes nothing, and the merchant's own
// recipient is not found until something made it.
func (a *API) lookUpRecipient(ctx context.Context, q pgx.Tx, p merchant.Principal, recipientID string) (string, error) {
	if recipientID != me {
		return recipientID, nil
	}
	rec, err := a.deps.Recipients.DefaultOf(ctx, q, owner(p))
	if err != nil {
		return "", notFound("recipient", me)
	}
	return rec.ID.String(), nil
}

// resolveSplit names the merchant's own recipient, where a rule says me, by its id.
func (a *API) resolveSplit(ctx context.Context, tx pgx.Tx, p merchant.Principal, rules []payments.SplitRule) error {
	for i := range rules {
		resolved, err := a.resolveRecipient(ctx, tx, p, rules[i].Recipient)
		if err != nil {
			return err
		}
		rules[i].Recipient = resolved
	}
	return nil
}
