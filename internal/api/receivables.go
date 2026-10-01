package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/receivables"
)

func (a *API) receivablesOperations() []operation {
	write := merchant.ScopeReceivablesWrite
	return []operation{
		{name: "create_receivables_opt_in", scope: write, phases: []phase{{point: pointStarted, atomic: a.optIn(true)}}},
		{name: "revoke_receivables_opt_in", scope: write, phases: []phase{{point: pointStarted, atomic: a.optIn(false)}}},
	}
}

func (a *API) CreateReceivablesOptIn(w http.ResponseWriter, r *http.Request, _ openapi.CreateReceivablesOptInParams) {
	a.serve(w, r, "create_receivables_opt_in", "")
}

func (a *API) RevokeReceivablesOptIn(w http.ResponseWriter, r *http.Request, financier string, _ openapi.RevokeReceivablesOptInParams) {
	a.serve(w, r, "revoke_receivables_opt_in", financier)
}

func (a *API) optIn(active bool) func(context.Context, pgx.Tx, *request) (outcome, error) {
	return func(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
		financier := r.pathID
		if active {
			var body openapi.CreateReceivablesOptInRequest
			if err := decode(r.body, &body, false); err != nil {
				return outcome{}, err
			}
			financier = body.Financier
		}
		o, err := a.deps.Receivables.SetOptIn(ctx, tx, paymentsOwner(r.principal), financier, active)
		if err != nil {
			return outcome{}, receivablesError(err)
		}
		return respond(optInJSON(o))
	}
}

func (a *API) ListReceivablesOptIns(w http.ResponseWriter, r *http.Request) {
	p, ok := a.authorize(w, r, merchant.ScopeReceivablesRead)
	if !ok {
		return
	}
	optIns, err := a.deps.Receivables.OptIns(r.Context(), a.deps.Pool, paymentsOwner(p))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.ReceivablesOptInList{Object: "list", Data: make([]openapi.ReceivablesOptIn, 0, len(optIns))}
	for _, o := range optIns {
		list.Data = append(list.Data, optInJSON(o))
	}
	a.writeJSON(w, r, list)
}

func (a *API) GetReceivablesAgenda(w http.ResponseWriter, r *http.Request, params openapi.GetReceivablesAgendaParams) {
	p, ok := a.authorize(w, r, merchant.ScopeReceivablesRead)
	if !ok {
		return
	}
	q := receivables.AgendaQuery{From: params.From.Time, To: params.To.Time, AsOf: a.deps.Now()}
	if params.AsOf != nil {
		q.AsOf = time.Unix(*params.AsOf, 0)
	}
	entries, err := a.deps.Receivables.Agenda(r.Context(), a.deps.Pool, paymentsOwner(p), q)
	if err != nil {
		a.fail(w, r, receivablesError(err))
		return
	}
	out := openapi.ReceivablesAgenda{Object: "receivables_agenda", AsOf: q.AsOf.Unix(), Data: make([]openapi.ReceivableUnit, 0, len(entries))}
	for _, e := range entries {
		out.Data = append(out.Data, unitJSON(e))
	}
	a.writeJSON(w, r, out)
}

func unitJSON(e receivables.Entry) openapi.ReceivableUnit {
	out := openapi.ReceivableUnit{
		Id: e.Unit, Object: "receivable_unit", Arrangement: e.Arrangement, Currency: strings.ToLower(e.Currency),
		Amount: e.Value, Blocked: e.Blocked, Free: e.Free, Registered: e.Registered,
		Committed: make([]openapi.ReceivableCommitment, 0, len(e.Committed)),
	}
	_ = out.SettlementDate.UnmarshalText([]byte(e.SettlementDate))
	if e.SettledOn != "" {
		_ = allocate(&out.SettledOn).UnmarshalText([]byte(e.SettledOn))
		settled := e.Settled
		out.SettledAmount = &settled
	}
	for _, c := range e.Committed {
		out.Committed = append(out.Committed, openapi.ReceivableCommitment{
			Contract: c.Contract, Beneficiary: c.Beneficiary, Effect: openapi.ReceivableCommitmentEffect(c.Effect), Amount: c.Amount,
		})
	}
	return out
}

func optInJSON(o receivables.OptIn) openapi.ReceivablesOptIn {
	return openapi.ReceivablesOptIn{Object: "receivables_opt_in", Financier: o.Financier, Active: o.Active, Synced: o.Synced, Updated: o.UpdatedAt.Unix()}
}

func receivablesError(err error) error {
	if errors.Is(err, receivables.ErrInvalid) {
		return invalidRequest("parameter_invalid", "", "%s", strings.TrimPrefix(err.Error(), receivables.ErrInvalid.Error()+": "))
	}
	return err
}
