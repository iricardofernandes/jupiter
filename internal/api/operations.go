package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
)

const defaultSecretOverlap = 24 * time.Hour

func (a *API) registerOperations() map[string]operation {
	ops := []operation{
		{name: "create_api_key", scope: merchant.ScopeAPIKeysWrite, phases: []phase{{point: pointStarted, atomic: a.createAPIKey}}, secret: true},
		{name: "revoke_api_key", scope: merchant.ScopeAPIKeysWrite, phases: []phase{{point: pointStarted, atomic: a.revokeAPIKey}}},
		{name: "create_webhook_endpoint", scope: merchant.ScopeWebhookEndpointWrite, phases: []phase{{point: pointStarted, atomic: a.createWebhookEndpoint}}, secret: true},
		{name: "update_webhook_endpoint", scope: merchant.ScopeWebhookEndpointWrite, phases: []phase{{point: pointStarted, atomic: a.updateWebhookEndpoint}}},
		{name: "roll_webhook_endpoint_secret", scope: merchant.ScopeWebhookEndpointWrite, phases: []phase{{point: pointStarted, atomic: a.rollWebhookEndpointSecret}}, secret: true},
		{name: "resend_event", scope: merchant.ScopeEventsWrite, phases: []phase{{point: pointStarted, atomic: a.resendEvent}}},
	}
	ops = append(ops, a.paymentOperations()...)
	ops = append(ops, a.payoutOperations()...)
	ops = append(ops, a.subscriptionOperations()...)
	ops = append(ops, a.receivablesOperations()...)
	ops = append(ops, a.recipientOperations()...)
	ops = append(ops, a.paymentMethodOperations()...)
	ops = append(ops, a.riskOperations()...)
	ops = append(ops, a.disputeOperations()...)
	out := make(map[string]operation, len(ops))
	for _, op := range ops {
		out[op.name] = op
	}
	return out
}

func (a *API) createAPIKey(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateApiKeyRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	scopes := make([]merchant.Scope, len(body.Scopes))
	for i, s := range body.Scopes {
		scopes[i] = merchant.Scope(s)
	}
	k, err := a.deps.Merchants.CreateRestrictedKey(ctx, tx, r.principal, deref(body.Name), scopes)
	if err != nil {
		return outcome{}, domainError(err, "api_key", "")
	}
	if _, err := a.deps.Events.Publish(ctx, tx, owner(r.principal), events.TypeAPIKeyCreated, events.ObjectRef{ID: k.ID.String(), Type: "api_key"}); err != nil {
		return outcome{}, err
	}
	return respond(apiKeyJSON(k.Key, &k.Value))
}

func (a *API) revokeAPIKey(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	keyID, err := merchant.KeyPrefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("api_key", r.pathID)
	}
	k, changed, err := a.deps.Merchants.RevokeKey(ctx, tx, r.principal, keyID)
	if err != nil {
		return outcome{}, domainError(err, "api_key", r.pathID)
	}
	if changed {
		if _, err := a.deps.Events.Publish(ctx, tx, owner(r.principal), events.TypeAPIKeyRevoked, events.ObjectRef{ID: k.ID.String(), Type: "api_key"}); err != nil {
			return outcome{}, err
		}
	}
	return respond(apiKeyJSON(k, nil))
}

func (a *API) createWebhookEndpoint(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateWebhookEndpointRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	e, secret, err := a.deps.Events.CreateEndpoint(ctx, tx, owner(r.principal), events.EndpointSpec{
		URL: body.Url, Description: deref(body.Description), EnabledEvents: body.EnabledEvents,
	}, r.version)
	if err != nil {
		return outcome{}, domainError(err, "webhook_endpoint", "")
	}
	return respond(endpointJSON(e, &secret))
}

func (a *API) updateWebhookEndpoint(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	endpointID, err := events.EndpointPrefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("webhook_endpoint", r.pathID)
	}
	var body openapi.UpdateWebhookEndpointRequest
	if decodeErr := decode(r.body, &body, false); decodeErr != nil {
		return outcome{}, decodeErr
	}
	update := events.EndpointUpdate{URL: body.Url, Description: body.Description}
	if body.EnabledEvents != nil {
		update.EnabledEvents = *body.EnabledEvents
	}
	if body.Status != nil {
		status := events.Status(*body.Status)
		update.Status = &status
	}
	e, err := a.deps.Events.UpdateEndpoint(ctx, tx, owner(r.principal), endpointID, update)
	if err != nil {
		return outcome{}, domainError(err, "webhook_endpoint", r.pathID)
	}
	return respond(endpointJSON(e, nil))
}

func (a *API) rollWebhookEndpointSecret(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	endpointID, err := events.EndpointPrefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("webhook_endpoint", r.pathID)
	}
	var body openapi.RollSecretRequest
	if decodeErr := decode(r.body, &body, true); decodeErr != nil {
		return outcome{}, decodeErr
	}
	overlap := defaultSecretOverlap
	if body.ExpireCurrentIn != nil {
		seconds := *body.ExpireCurrentIn
		if seconds < 0 || time.Duration(seconds) > events.MaxSecretOverlap/time.Second {
			return outcome{}, invalidRequest("parameter_invalid", "expire_current_in",
				"expire_current_in must be between 0 and %d seconds.", int(events.MaxSecretOverlap/time.Second))
		}
		overlap = time.Duration(seconds) * time.Second
	}
	secret, err := a.deps.Events.RollSecret(ctx, tx, owner(r.principal), endpointID, overlap)
	if err != nil {
		return outcome{}, domainError(err, "webhook_endpoint", r.pathID)
	}
	e, err := a.deps.Events.Endpoint(ctx, tx, owner(r.principal), endpointID)
	if err != nil {
		return outcome{}, domainError(err, "webhook_endpoint", r.pathID)
	}
	return respond(endpointJSON(e, &secret))
}

func (a *API) resendEvent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	eventID, err := events.EventPrefix.Parse(r.pathID)
	if err != nil {
		return outcome{}, notFound("event", r.pathID)
	}
	var body openapi.ResendEventRequest
	if decodeErr := decode(r.body, &body, true); decodeErr != nil {
		return outcome{}, decodeErr
	}
	var endpoint *id.ID
	if body.WebhookEndpoint != nil {
		endpointID, parseErr := events.EndpointPrefix.Parse(*body.WebhookEndpoint)
		if parseErr != nil {
			return outcome{}, invalidRequest("resource_missing", "webhook_endpoint", "No such webhook_endpoint: %q", *body.WebhookEndpoint)
		}
		endpoint = &endpointID
	}
	if _, resendErr := a.deps.Events.Resend(ctx, tx, owner(r.principal), eventID, endpoint); resendErr != nil {
		return outcome{}, domainError(resendErr, "event", r.pathID)
	}
	e, err := a.deps.Events.Event(ctx, tx, owner(r.principal), eventID)
	if err != nil {
		return outcome{}, domainError(err, "event", r.pathID)
	}
	return respond(eventJSON(e, nil))
}

// decode reads a JSON body strictly: unknown parameters are errors, so a typo is never
// silently ignored.
func decode(body []byte, v any, optional bool) error {
	if len(bytes.TrimSpace(body)) == 0 {
		if optional {
			return nil
		}
		return invalidRequest("body_missing", "", "This request needs a JSON body.")
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil {
		if _, trailing := dec.Token(); !errors.Is(trailing, io.EOF) {
			return invalidRequest("body_invalid", "", "The request body must be a single JSON object.")
		}
		return nil
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return invalidRequest("parameter_invalid", typeErr.Field, "%s must be a %s.", typeErr.Field, typeErr.Type.Kind())
	}
	if field, ok := strings.CutPrefix(err.Error(), "json: unknown field "); ok {
		name := strings.Trim(field, `"`)
		return invalidRequest("parameter_unknown", name, "Received unknown parameter: %s.", name)
	}
	return invalidRequest("body_invalid", "", "The request body is not valid JSON.")
}

// domainError turns a module's error into the API error a client can act on.
func domainError(err error, resource, objectID string) error {
	for _, sentinel := range []error{merchant.ErrInvalid, events.ErrInvalid} {
		if errors.Is(err, sentinel) {
			return invalidRequest("parameter_invalid", "", "%s", strings.TrimPrefix(err.Error(), sentinel.Error()+": "))
		}
	}
	if errors.Is(err, merchant.ErrNotFound) || errors.Is(err, events.ErrNotFound) {
		return notFound(resource, objectID)
	}
	return err
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
