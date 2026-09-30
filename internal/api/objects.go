package api

import (
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/merchant"
)

var objectPaths = map[string]string{
	"api_key":          "/v1/api_keys/",
	"webhook_endpoint": "/v1/webhook_endpoints/",
}

func apiKeyJSON(k merchant.Key, value *string) openapi.ApiKey {
	out := openapi.ApiKey{
		Id: k.ID.String(), Object: "api_key", Livemode: k.Livemode, Kind: openapi.ApiKeyKind(k.Kind),
		Name: k.Name, Last4: k.Last4, Scopes: make([]string, len(k.Scopes)), Created: k.CreatedAt.Unix(), Value: value,
	}
	for i, s := range k.Scopes {
		out.Scopes[i] = string(s)
	}
	if !k.RevokedAt.IsZero() {
		revoked := k.RevokedAt.Unix()
		out.Revoked = &revoked
	}
	return out
}

func endpointJSON(e events.Endpoint, secret *string) openapi.WebhookEndpoint {
	return openapi.WebhookEndpoint{
		Id: e.ID.String(), Object: "webhook_endpoint", Livemode: e.Owner.Livemode, Url: e.URL,
		Description: e.Description, EnabledEvents: e.EnabledEvents, Status: openapi.WebhookEndpointStatus(e.Status),
		ApiVersion: e.APIVersion, Created: e.CreatedAt.Unix(), Secret: secret,
	}
}

func eventJSON(e events.Event, related map[string]any) openapi.Event {
	out := openapi.Event{
		Id: e.ID.String(), Object: "event", Type: e.Type, Livemode: e.Owner.Livemode, Created: e.CreatedAt.Unix(),
		RelatedObject: openapi.RelatedObject{Id: e.Object.ID, Type: e.Object.Type, Url: objectPaths[e.Object.Type] + e.Object.ID},
	}
	if related != nil {
		out.RelatedObject.Object = &related
	}
	return out
}
