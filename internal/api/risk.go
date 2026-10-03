package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/risk"
)

func (a *API) riskOperations() []operation {
	write := merchant.ScopeRiskWrite
	return []operation{
		{name: "create_risk_rule", scope: write, phases: []phase{{point: pointStarted, atomic: a.createRiskRule}}},
		{name: "create_risk_list_item", scope: write, phases: []phase{{point: pointStarted, atomic: a.createRiskListItem}}},
	}
}

func riskOwner(p merchant.Principal) risk.Owner {
	return risk.Owner{Merchant: p.Merchant, Livemode: p.Livemode}
}

func (a *API) riskReady(w http.ResponseWriter, r *http.Request, scope merchant.Scope) (merchant.Principal, bool) {
	p, ok := a.authorize(w, r, scope)
	if ok && a.deps.Risk == nil {
		a.writeError(w, r, &Error{Status: http.StatusNotFound, Type: openapi.InvalidRequestError, Code: "url_invalid", Message: "The risk engine is not enabled."})
		return p, false
	}
	return p, ok
}

func (a *API) ListRiskDecisions(w http.ResponseWriter, r *http.Request, params openapi.ListRiskDecisionsParams) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	filter := risk.DecisionFilter{Intent: deref(params.PaymentIntent)}
	if params.Action != nil {
		filter.Action = risk.Action(*params.Action)
	}
	decisions, more, err := a.deps.Risk.Decisions(r.Context(), a.deps.Pool, riskOwner(p), filter, pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.RiskDecisionList{Object: "list", Url: "/v1/risk/decisions", HasMore: more, Data: make([]openapi.RiskDecision, 0, len(decisions))}
	for _, d := range decisions {
		list.Data = append(list.Data, decisionJSON(d, p.Livemode))
	}
	a.writeJSON(w, r, list)
}

func (a *API) GetRiskDecision(w http.ResponseWriter, r *http.Request, decisionID openapi.ID) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskRead)
	if !ok {
		return
	}
	d, err := a.deps.Risk.Decision(r.Context(), a.deps.Pool, riskOwner(p), decisionID)
	if err != nil {
		a.fail(w, r, riskError(err, "risk_decision", decisionID))
		return
	}
	a.writeJSON(w, r, decisionJSON(d, p.Livemode))
}

func (a *API) ListRiskRules(w http.ResponseWriter, r *http.Request) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskRead)
	if !ok {
		return
	}
	own, err := a.deps.Risk.Rules(r.Context(), a.deps.Pool, riskOwner(p))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.RiskRulePage{Object: "list", Data: []openapi.RiskRule{}}
	for _, rule := range risk.PlatformRules() {
		list.Data = append(list.Data, ruleJSON(rule, true, nil))
	}
	for _, rule := range own {
		created := rule.Created.Unix()
		list.Data = append(list.Data, ruleJSON(rule.Rule, false, &created))
	}
	a.writeJSON(w, r, list)
}

func (a *API) CreateRiskRule(w http.ResponseWriter, r *http.Request, _ openapi.CreateRiskRuleParams) {
	if _, ok := a.riskReady(w, r, merchant.ScopeRiskWrite); ok {
		a.serve(w, r, "create_risk_rule", "")
	}
}

func (a *API) createRiskRule(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateRiskRuleRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	rule, err := a.deps.Risk.CreateRule(ctx, tx, riskOwner(r.principal), risk.Action(body.Action), body.Expression, deref(body.Description))
	if err != nil {
		return outcome{}, riskError(err, "risk_rule", "")
	}
	created := rule.Created.Unix()
	return respond(ruleJSON(rule.Rule, false, &created))
}

func (a *API) DeleteRiskRule(w http.ResponseWriter, r *http.Request, ruleID openapi.ID) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskWrite)
	if !ok {
		return
	}
	err := postgres.InTx(r.Context(), a.deps.Pool, func(tx pgx.Tx) error { //nolint:contextcheck // the closure uses the request's context
		_, err := a.deps.Risk.DeleteRule(r.Context(), tx, riskOwner(p), ruleID)
		return err
	})
	if err != nil {
		a.fail(w, r, riskError(err, "risk_rule", ruleID))
		return
	}
	a.writeJSON(w, r, openapi.DeletedObject{Id: ruleID, Object: "risk_rule", Deleted: true})
}

func (a *API) ListRiskListItems(w http.ResponseWriter, r *http.Request, params openapi.ListRiskListItemsParams) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	items, more, err := a.deps.Risk.ListItems(r.Context(), a.deps.Pool, riskOwner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.RiskListItemPage{Object: "list", Url: "/v1/risk/list_items", HasMore: more, Data: make([]openapi.RiskListItem, 0, len(items))}
	for _, item := range items {
		list.Data = append(list.Data, listItemJSON(item))
	}
	a.writeJSON(w, r, list)
}

func (a *API) CreateRiskListItem(w http.ResponseWriter, r *http.Request, _ openapi.CreateRiskListItemParams) {
	if _, ok := a.riskReady(w, r, merchant.ScopeRiskWrite); ok {
		a.serve(w, r, "create_risk_list_item", "")
	}
}

func (a *API) createRiskListItem(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateRiskListItemRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	item, err := a.deps.Risk.AddListItem(ctx, tx, riskOwner(r.principal), string(body.List), string(body.Kind), body.Value)
	if err != nil {
		return outcome{}, riskError(err, "risk_list_item", "")
	}
	return respond(listItemJSON(item))
}

func (a *API) DeleteRiskListItem(w http.ResponseWriter, r *http.Request, itemID openapi.ID) {
	p, ok := a.riskReady(w, r, merchant.ScopeRiskWrite)
	if !ok {
		return
	}
	err := postgres.InTx(r.Context(), a.deps.Pool, func(tx pgx.Tx) error { //nolint:contextcheck // the closure uses the request's context
		return a.deps.Risk.RemoveListItem(r.Context(), tx, riskOwner(p), itemID)
	})
	if err != nil {
		a.fail(w, r, riskError(err, "risk_list_item", itemID))
		return
	}
	a.writeJSON(w, r, openapi.DeletedObject{Id: itemID, Object: "risk_list_item", Deleted: true})
}

func riskError(err error, resource, objectID string) error {
	switch {
	case errors.Is(err, risk.ErrInvalid):
		return invalidRequest("parameter_invalid", "", "%s", strings.TrimPrefix(err.Error(), risk.ErrInvalid.Error()+": "))
	case errors.Is(err, risk.ErrNotFound):
		return notFound(resource, objectID)
	default:
		return err
	}
}

func decisionJSON(d risk.Decision, livemode bool) openapi.RiskDecision {
	out := openapi.RiskDecision{
		Id: d.ID, Object: "risk_decision", Livemode: livemode, PaymentIntent: d.Intent, Attempt: d.Attempt,
		Action: openapi.RiskDecisionAction(d.Action), Rules: make([]openapi.FiredRule, 0, len(d.Rules)),
		Features: featuresJSON(d.Features), Created: d.Created.Unix(),
	}
	for _, f := range d.Rules {
		out.Rules = append(out.Rules, openapi.FiredRule{
			Id: f.ID, Action: openapi.FiredRuleAction(f.Action), Description: f.Description, Expression: optional(f.Expression),
		})
	}
	return out
}

func featuresJSON(f risk.Features) map[string]any {
	raw, err := json.Marshal(f)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	// What a card did at other merchants decides, but is not shown: a merchant holding a
	// card number would learn whether it is declined elsewhere.
	for _, shared := range []string{"card_attempts_1h", "card_attempts_24h", "card_declines_24h"} {
		delete(out, shared)
	}
	return out
}

func ruleJSON(rule risk.Rule, platform bool, created *int64) openapi.RiskRule {
	return openapi.RiskRule{
		Id: rule.ID, Object: "risk_rule", Action: openapi.RiskRuleAction(rule.Action), Expression: rule.Expression,
		Description: rule.Description, Platform: platform, Created: created,
	}
}

func listItemJSON(item risk.ListItem) openapi.RiskListItem {
	return openapi.RiskListItem{
		Id: item.ID, Object: "risk_list_item", List: openapi.RiskListItemList(item.List),
		Kind: openapi.RiskListItemKind(item.Kind), Value: item.Value, Created: item.Created.Unix(),
	}
}
