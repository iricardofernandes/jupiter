package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

var _ openapi.ServerInterface = (*API)(nil)

func (a *API) authorize(w http.ResponseWriter, r *http.Request, scope merchant.Scope) (merchant.Principal, bool) {
	p := principalFrom(r.Context())
	if !p.Can(scope) {
		a.writeError(w, r, forbidden(string(scope)))
		return p, false
	}
	return p, true
}

func pageRequest(limit *int, after, before *string) (page.Request, error) {
	r := page.Request{Limit: page.DefaultLimit}
	if limit != nil {
		r.Limit = *limit
	}
	if after != nil {
		r.StartingAfter = *after
	}
	if before != nil {
		r.EndingBefore = *before
	}
	if err := r.Validate(); err != nil {
		return r, invalidRequest("parameter_invalid", "limit", "%s", err.Error())
	}
	return r, nil
}

func (a *API) ListApiKeys(w http.ResponseWriter, r *http.Request, params openapi.ListApiKeysParams) { //nolint:revive // name fixed by the generated interface
	p, ok := a.authorize(w, r, merchant.ScopeAPIKeysRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	keys, more, err := a.deps.Merchants.ListKeys(r.Context(), a.deps.Pool, p, pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.ApiKeyList{Object: "list", Url: "/v1/api_keys", HasMore: more, Data: make([]openapi.ApiKey, 0, len(keys))}
	for _, k := range keys {
		list.Data = append(list.Data, apiKeyJSON(k, nil))
	}
	a.writeJSON(w, r, list)
}

func (a *API) CreateApiKey(w http.ResponseWriter, r *http.Request, _ openapi.CreateApiKeyParams) { //nolint:revive // name fixed by the generated interface
	a.serve(w, r, "create_api_key", "")
}

func (a *API) GetApiKey(w http.ResponseWriter, r *http.Request, keyID openapi.ID) { //nolint:revive // name fixed by the generated interface
	p, ok := a.authorize(w, r, merchant.ScopeAPIKeysRead)
	if !ok {
		return
	}
	parsed, err := merchant.KeyPrefix.Parse(keyID)
	if err != nil {
		a.writeError(w, r, notFound("api_key", keyID))
		return
	}
	k, err := a.deps.Merchants.GetKey(r.Context(), a.deps.Pool, p, parsed)
	if err != nil {
		a.fail(w, r, domainError(err, "api_key", keyID))
		return
	}
	a.writeJSON(w, r, apiKeyJSON(k, nil))
}

func (a *API) RevokeApiKey(w http.ResponseWriter, r *http.Request, keyID openapi.ID, _ openapi.RevokeApiKeyParams) { //nolint:revive // name fixed by the generated interface
	a.serve(w, r, "revoke_api_key", keyID)
}

func (a *API) ListWebhookEndpoints(w http.ResponseWriter, r *http.Request, params openapi.ListWebhookEndpointsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeWebhookEndpointsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	endpoints, more, err := a.deps.Events.Endpoints(r.Context(), a.deps.Pool, owner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.WebhookEndpointList{Object: "list", Url: "/v1/webhook_endpoints", HasMore: more, Data: make([]openapi.WebhookEndpoint, 0, len(endpoints))}
	for _, e := range endpoints {
		list.Data = append(list.Data, endpointJSON(e, nil))
	}
	a.writeJSON(w, r, list)
}

func (a *API) CreateWebhookEndpoint(w http.ResponseWriter, r *http.Request, _ openapi.CreateWebhookEndpointParams) {
	a.serve(w, r, "create_webhook_endpoint", "")
}

func (a *API) GetWebhookEndpoint(w http.ResponseWriter, r *http.Request, endpointID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeWebhookEndpointsRead)
	if !ok {
		return
	}
	parsed, err := events.EndpointPrefix.Parse(endpointID)
	if err != nil {
		a.writeError(w, r, notFound("webhook_endpoint", endpointID))
		return
	}
	e, err := a.deps.Events.Endpoint(r.Context(), a.deps.Pool, owner(p), parsed)
	if err != nil {
		a.fail(w, r, domainError(err, "webhook_endpoint", endpointID))
		return
	}
	a.writeJSON(w, r, endpointJSON(e, nil))
}

func (a *API) UpdateWebhookEndpoint(w http.ResponseWriter, r *http.Request, endpointID openapi.ID, _ openapi.UpdateWebhookEndpointParams) {
	a.serve(w, r, "update_webhook_endpoint", endpointID)
}

func (a *API) RollWebhookEndpointSecret(w http.ResponseWriter, r *http.Request, endpointID openapi.ID, _ openapi.RollWebhookEndpointSecretParams) {
	a.serve(w, r, "roll_webhook_endpoint_secret", endpointID)
}

// DeleteWebhookEndpoint needs no Idempotency-Key: deleting twice answers 404 the second
// time and changes nothing.
func (a *API) DeleteWebhookEndpoint(w http.ResponseWriter, r *http.Request, endpointID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeWebhookEndpointWrite)
	if !ok {
		return
	}
	parsed, err := events.EndpointPrefix.Parse(endpointID)
	if err != nil {
		a.writeError(w, r, notFound("webhook_endpoint", endpointID))
		return
	}
	err = postgres.InTx(r.Context(), a.deps.Pool, func(tx pgx.Tx) error { //nolint:contextcheck // the closure uses the request's context
		return a.deps.Events.DeleteEndpoint(r.Context(), tx, owner(p), parsed)
	})
	if err != nil {
		a.fail(w, r, domainError(err, "webhook_endpoint", endpointID))
		return
	}
	a.writeJSON(w, r, openapi.DeletedObject{Id: endpointID, Object: "webhook_endpoint", Deleted: true})
}

func (a *API) ListEvents(w http.ResponseWriter, r *http.Request, params openapi.ListEventsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeEventsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	include, err := includes(params.Include)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var eventType string
	if params.Type != nil {
		eventType = *params.Type
	}
	list, more, err := a.deps.Events.Events(r.Context(), a.deps.Pool, owner(p), eventType, pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := openapi.EventList{Object: "list", Url: "/v1/events", HasMore: more, Data: make([]openapi.Event, 0, len(list))}
	// Events of one object share its current state, read once.
	related := map[events.ObjectRef]map[string]any{}
	for _, e := range list {
		ev, err := a.event(r.Context(), p, e, include, related)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		out.Data = append(out.Data, ev)
	}
	a.writeJSON(w, r, out)
}

func (a *API) GetEvent(w http.ResponseWriter, r *http.Request, eventID openapi.ID, params openapi.GetEventParams) {
	p, ok := a.authorize(w, r, merchant.ScopeEventsRead)
	if !ok {
		return
	}
	include, err := includes(params.Include)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	parsed, err := events.EventPrefix.Parse(eventID)
	if err != nil {
		a.writeError(w, r, notFound("event", eventID))
		return
	}
	e, err := a.deps.Events.Event(r.Context(), a.deps.Pool, owner(p), parsed)
	if err != nil {
		a.fail(w, r, domainError(err, "event", eventID))
		return
	}
	ev, err := a.event(r.Context(), p, e, include, nil)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.writeJSON(w, r, ev)
}

func (a *API) ResendEvent(w http.ResponseWriter, r *http.Request, eventID openapi.ID, _ openapi.ResendEventParams) {
	a.serve(w, r, "resend_event", eventID)
}

func includes(raw *openapi.Include) (bool, error) {
	if raw == nil {
		return false, nil
	}
	for _, field := range *raw {
		if field != "related_object" {
			return false, invalidRequest("parameter_invalid", "include[]", "%q cannot be included; events accept related_object.", field)
		}
	}
	return slices.Contains(*raw, "related_object"), nil
}

// event renders an event, with its related object inline when asked for and still there.
// The related object is read with the caller's own scopes, so include[] reveals nothing
// the key could not fetch directly.
func (a *API) event(ctx context.Context, p merchant.Principal, e events.Event, include bool, read map[events.ObjectRef]map[string]any) (openapi.Event, error) {
	if !include {
		return eventJSON(e, nil), nil
	}
	if object, ok := read[e.Object]; ok {
		return eventJSON(e, object), nil
	}
	var related any
	var err error
	switch e.Object.Type {
	case "webhook_endpoint", "api_key":
		related, err = a.relatedAccountObject(ctx, p, e.Object)
	case "payment_intent", "refund", "payout":
		related, err = a.relatedPayment(ctx, p, e.Object)
	}
	if errors.Is(err, errRelatedGone) || (err == nil && related == nil) {
		if read != nil {
			read[e.Object] = nil
		}
		return eventJSON(e, nil), nil
	}
	if err != nil {
		return openapi.Event{}, err
	}
	raw, err := json.Marshal(related)
	if err != nil {
		return openapi.Event{}, err
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return openapi.Event{}, err
	}
	if read != nil {
		read[e.Object] = object
	}
	return eventJSON(e, object), nil
}

// errRelatedGone marks a related object deleted since the event; it is left out.
var errRelatedGone = errors.New("related object no longer exists")

// relatedAccountObject reads an endpoint or API key for include[].
func (a *API) relatedAccountObject(ctx context.Context, p merchant.Principal, ref events.ObjectRef) (any, error) {
	if ref.Type == "webhook_endpoint" {
		return a.relatedEndpoint(ctx, p, ref.ID)
	}
	if !p.Can(merchant.ScopeAPIKeysRead) {
		return nil, forbidden(string(merchant.ScopeAPIKeysRead))
	}
	keyID, err := merchant.KeyPrefix.Parse(ref.ID)
	if err != nil {
		return nil, err
	}
	k, err := a.deps.Merchants.GetKey(ctx, a.deps.Pool, p, keyID)
	if errors.Is(err, merchant.ErrNotFound) {
		return nil, errRelatedGone
	}
	if err != nil {
		return nil, err
	}
	return apiKeyJSON(k, nil), nil
}

func (a *API) relatedEndpoint(ctx context.Context, p merchant.Principal, rawID string) (any, error) {
	if !p.Can(merchant.ScopeWebhookEndpointsRead) {
		return nil, forbidden(string(merchant.ScopeWebhookEndpointsRead))
	}
	endpointID, err := events.EndpointPrefix.Parse(rawID)
	if err != nil {
		return nil, err
	}
	endpoint, err := a.deps.Events.Endpoint(ctx, a.deps.Pool, owner(p), endpointID)
	if errors.Is(err, events.ErrNotFound) {
		return nil, errRelatedGone
	}
	if err != nil {
		return nil, err
	}
	return endpointJSON(endpoint, nil), nil
}
