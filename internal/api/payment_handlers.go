package api

import (
	"context"
	"net/http"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

func (a *API) ListPaymentIntents(w http.ResponseWriter, r *http.Request, params openapi.ListPaymentIntentsParams) {
	p, ok := a.authorize(w, r, merchant.ScopePaymentIntentsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	intents, more, err := a.deps.Payments.Intents(r.Context(), a.deps.Pool, paymentsOwner(p), pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.PaymentIntentList{Object: "list", Url: "/v1/payment_intents", HasMore: more, Data: make([]openapi.PaymentIntent, 0, len(intents))}
	for _, it := range intents {
		list.Data = append(list.Data, paymentIntentJSON(it))
	}
	a.writeJSON(w, r, list)
}

func (a *API) GetPaymentIntent(w http.ResponseWriter, r *http.Request, intentID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopePaymentIntentsRead)
	if !ok {
		return
	}
	parsed, err := payments.IntentPrefix.Parse(intentID)
	if err != nil {
		a.writeError(w, r, notFound("payment_intent", intentID))
		return
	}
	it, err := a.deps.Payments.Intent(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, paymentsError(err, intentID))
		return
	}
	a.writeJSON(w, r, paymentIntentJSON(it))
}

func (a *API) CreatePaymentIntent(w http.ResponseWriter, r *http.Request, _ openapi.CreatePaymentIntentParams) {
	a.serve(w, r, "create_payment_intent", "")
}

func (a *API) UpdatePaymentIntent(w http.ResponseWriter, r *http.Request, intentID openapi.ID, _ openapi.UpdatePaymentIntentParams) {
	a.serve(w, r, "update_payment_intent", intentID)
}

func (a *API) ConfirmPaymentIntent(w http.ResponseWriter, r *http.Request, intentID openapi.ID, _ openapi.ConfirmPaymentIntentParams) {
	a.serve(w, r, "confirm_payment_intent", intentID)
}

func (a *API) CapturePaymentIntent(w http.ResponseWriter, r *http.Request, intentID openapi.ID, _ openapi.CapturePaymentIntentParams) {
	a.serve(w, r, "capture_payment_intent", intentID)
}

func (a *API) CancelPaymentIntent(w http.ResponseWriter, r *http.Request, intentID openapi.ID, _ openapi.CancelPaymentIntentParams) {
	a.serve(w, r, "cancel_payment_intent", intentID)
}

func (a *API) CompletePaymentIntentAction(w http.ResponseWriter, r *http.Request, intentID openapi.ID, _ openapi.CompletePaymentIntentActionParams) {
	a.serve(w, r, "complete_payment_intent_action", intentID)
}

func (a *API) CreateRefund(w http.ResponseWriter, r *http.Request, _ openapi.CreateRefundParams) {
	a.serve(w, r, "create_refund", "")
}

func (a *API) GetRefund(w http.ResponseWriter, r *http.Request, refundID openapi.ID) {
	p, ok := a.authorize(w, r, merchant.ScopeRefundsRead)
	if !ok {
		return
	}
	parsed, err := payments.RefundPrefix.Parse(refundID)
	if err != nil {
		a.writeError(w, r, notFound("refund", refundID))
		return
	}
	refund, err := a.deps.Payments.Refund(r.Context(), a.deps.Pool, paymentsOwner(p), parsed)
	if err != nil {
		a.fail(w, r, paymentsError(err, refundID))
		return
	}
	a.writeJSON(w, r, refundJSON(refund))
}

func (a *API) ListRefunds(w http.ResponseWriter, r *http.Request, params openapi.ListRefundsParams) {
	p, ok := a.authorize(w, r, merchant.ScopeRefundsRead)
	if !ok {
		return
	}
	pr, err := pageRequest(params.Limit, params.StartingAfter, params.EndingBefore)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var intent *id.ID
	if params.PaymentIntent != nil {
		parsed, err := payments.IntentPrefix.Parse(*params.PaymentIntent)
		if err != nil {
			a.writeError(w, r, invalidRequest("resource_missing", "payment_intent", "No such payment_intent: %q", *params.PaymentIntent))
			return
		}
		intent = &parsed
	}
	refunds, more, err := a.deps.Payments.Refunds(r.Context(), a.deps.Pool, paymentsOwner(p), intent, pr)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	list := openapi.RefundList{Object: "list", Url: "/v1/refunds", HasMore: more, Data: make([]openapi.Refund, 0, len(refunds))}
	for _, refund := range refunds {
		list.Data = append(list.Data, refundJSON(refund))
	}
	a.writeJSON(w, r, list)
}

// relatedPayment reads a payment object for include[], with the caller's scopes.
func (a *API) relatedPayment(ctx context.Context, p merchant.Principal, ref events.ObjectRef) (any, error) {
	switch ref.Type {
	case "payment_intent":
		if !p.Can(merchant.ScopePaymentIntentsRead) {
			return nil, forbidden(string(merchant.ScopePaymentIntentsRead))
		}
		intentID, err := payments.IntentPrefix.Parse(ref.ID)
		if err != nil {
			return nil, err
		}
		it, err := a.deps.Payments.Intent(ctx, a.deps.Pool, paymentsOwner(p), intentID)
		if err != nil {
			return nil, err
		}
		return paymentIntentJSON(it), nil
	default:
		if !p.Can(merchant.ScopeRefundsRead) {
			return nil, forbidden(string(merchant.ScopeRefundsRead))
		}
		refundID, err := payments.RefundPrefix.Parse(ref.ID)
		if err != nil {
			return nil, err
		}
		refund, err := a.deps.Payments.Refund(ctx, a.deps.Pool, paymentsOwner(p), refundID)
		if err != nil {
			return nil, err
		}
		return refundJSON(refund), nil
	}
}
