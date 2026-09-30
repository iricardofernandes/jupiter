package api

import (
	"strings"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

var objectPaths = map[string]string{
	"api_key":          "/v1/api_keys/",
	"webhook_endpoint": "/v1/webhook_endpoints/",
	"payment_intent":   "/v1/payment_intents/",
	"refund":           "/v1/refunds/",
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func paymentIntentJSON(it payments.Intent) openapi.PaymentIntent {
	out := openapi.PaymentIntent{
		Id: it.ID.String(), Object: "payment_intent", Livemode: it.Owner.Livemode,
		Amount: it.Amount.Minor(), Currency: strings.ToLower(it.Amount.Currency().Code()),
		CaptureMethod: openapi.PaymentIntentCaptureMethod(it.CaptureMethod), Status: openapi.PaymentIntentStatus(it.Status),
		PaymentMethod: optional(it.PaymentMethod), Description: it.Description,
		AmountCapturable: it.AmountCapturable.Minor(), AmountReceived: it.AmountReceived.Minor(),
		AmountRefunded: it.AmountRefunded.Minor(), CancellationReason: optional(it.CancellationReason),
		Created: it.CreatedAt.Unix(),
	}
	if !it.LatestAttempt.IsZero() {
		out.LatestAttempt = optional(it.LatestAttempt.String())
	}
	if it.LastError != nil {
		out.LastPaymentError = &openapi.PaymentError{Code: it.LastError.Code, Message: it.LastError.Message, DeclineCode: optional(it.LastError.DeclineCode)}
	}
	if it.NextAction != "" {
		out.NextAction = &openapi.NextAction{Type: it.NextAction}
	}
	if it.Installments != nil {
		out.Installments = &openapi.Installments{Count: it.Installments.Count, FinancedBy: openapi.InstallmentsFinancedBy(it.Installments.FinancedBy)}
	}
	if it.SetupFutureUsage != "" {
		setup := openapi.PaymentIntentSetupFutureUsage(it.SetupFutureUsage)
		out.SetupFutureUsage = &setup
	}
	return out
}

func refundJSON(r payments.Refund) openapi.Refund {
	return openapi.Refund{
		Id: r.ID.String(), Object: "refund", Livemode: r.Owner.Livemode, Amount: r.Amount.Minor(),
		Currency: strings.ToLower(r.Amount.Currency().Code()), PaymentIntent: r.Intent.String(),
		Reason: optional(r.Reason), Status: openapi.RefundStatus(r.Status), FailureReason: optional(r.FailureReason),
		Created: r.CreatedAt.Unix(),
	}
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
