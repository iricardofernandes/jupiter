package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
)

const (
	pointAuthenticating = "authenticating"
	pointAuthorizing    = "authorizing"
	pointCapturing      = "capturing"
	pointVoiding        = "voiding"
	pointRefunding      = "refunding"
	stateIntent         = "intent"
	stateReturnURL      = "return_url"
	stateRefund         = "refund"
)

// The phases that talk to the rail are shared: creating with confirm, confirming and
// completing an action all go on to authorize, and authorizing with automatic capture
// goes on to capture.
func (a *API) paymentOperations() []operation {
	authenticate := phase{point: pointAuthenticating, foreign: a.authenticateCardholder, atomic: a.finishAuthentication}
	authorize := phase{point: pointAuthorizing, foreign: a.authorizeOnRail, atomic: a.finishAuthorization}
	capture := phase{point: pointCapturing, foreign: a.captureOnRail, atomic: a.finishCapture}
	void := phase{point: pointVoiding, foreign: a.voidOnRail, atomic: a.finishCancel}
	refund := phase{point: pointRefunding, foreign: a.refundOnRail, atomic: a.finishRefund}
	write := merchant.ScopePaymentIntentsWrite
	return []operation{
		{name: "create_payment_intent", scope: write, phases: []phase{{point: pointStarted, atomic: a.createPaymentIntent}, authenticate, authorize, capture}},
		{name: "update_payment_intent", scope: write, phases: []phase{{point: pointStarted, atomic: a.updatePaymentIntent}}},
		{name: "confirm_payment_intent", scope: write, phases: []phase{{point: pointStarted, atomic: a.confirmPaymentIntent}, authenticate, authorize, capture}},
		{name: "capture_payment_intent", scope: write, phases: []phase{{point: pointStarted, atomic: a.capturePaymentIntent}, capture}},
		{name: "cancel_payment_intent", scope: write, phases: []phase{{point: pointStarted, atomic: a.cancelPaymentIntent}, void}},
		{name: "complete_payment_intent_action", scope: write, phases: []phase{{point: pointStarted, atomic: a.completeAction}, authorize, capture}},
		{name: "create_refund", scope: merchant.ScopeRefundsWrite, phases: []phase{{point: pointStarted, atomic: a.createRefund}, refund}},
		// A payment whose cardholder completed a 3-D Secure challenge resumes here.
		{name: "resume_payment_intent", scope: write, phases: []phase{authorize, capture}},
	}
}

func paymentsOwner(p merchant.Principal) payments.Owner {
	return payments.Owner{Merchant: p.Merchant, Livemode: p.Livemode}
}

func (a *API) createPaymentIntent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreatePaymentIntentRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	amount, err := parseAmount(body.Amount, body.Currency, "amount")
	if err != nil {
		return outcome{}, err
	}
	params := payments.CreateParams{
		Amount: amount, PaymentMethod: deref(body.PaymentMethod), Description: deref(body.Description),
		Installments: installmentsParam(body.Installments), Split: splitParam(body.Split), Boleto: boletoParam(body.Boleto),
	}
	if params.Pix, err = pixOptionsParam(body.Pix); err != nil {
		return outcome{}, err
	}
	if body.CaptureMethod != nil {
		params.CaptureMethod = payments.CaptureMethod(*body.CaptureMethod)
	}
	if body.RequestThreeDSecure != nil {
		params.RequestThreeDSecure = string(*body.RequestThreeDSecure)
	}
	if body.SetupFutureUsage != nil {
		params.SetupFutureUsage = string(*body.SetupFutureUsage)
	}
	offSession := body.OffSession != nil && *body.OffSession
	confirm := body.Confirm != nil && *body.Confirm
	if offSession && !confirm {
		return outcome{}, invalidRequest("parameter_invalid", "off_session", "off_session applies only with confirm.")
	}
	returnURL, err := checkReturnURL(body.ReturnUrl)
	if err != nil {
		return outcome{}, err
	}
	if err := a.resolveSplit(ctx, tx, r.principal, params.Split); err != nil {
		return outcome{}, err
	}
	owner := paymentsOwner(r.principal)
	it, err := a.deps.Payments.Create(ctx, tx, owner, params)
	if err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	r.state[stateIntent] = it.ID.String()
	if !confirm {
		return respond(paymentIntentJSON(it))
	}
	r.state[stateReturnURL] = returnURL
	confirmed, step, err := a.deps.Payments.StartConfirm(ctx, tx, owner, it.ID, payments.ConfirmParams{OffSession: offSession, IP: deref(body.CustomerIp)})
	if err != nil {
		return outcome{}, paymentsError(err, it.ID.String())
	}
	return a.next(r, confirmed, step)
}

// next goes from a confirmation, or a 3-D Secure answer, to what the payment needs.
func (a *API) next(r *request, it payments.Intent, step payments.Step) (outcome, error) {
	switch step {
	case payments.StepAuthenticate:
		return proceed(pointAuthenticating)
	case payments.StepConfirm:
		return proceed(pointAuthorizing)
	default:
		return respondIntent(r, it)
	}
}

func (a *API) updatePaymentIntent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	intentID, err := intentFromPath(r)
	if err != nil {
		return outcome{}, err
	}
	var body openapi.UpdatePaymentIntentRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	params := payments.UpdateParams{
		PaymentMethod: body.PaymentMethod, Description: body.Description, Installments: installmentsParam(body.Installments),
		Boleto: boletoParam(body.Boleto),
	}
	if body.Split != nil {
		rules := splitParam(body.Split)
		if err := a.resolveSplit(ctx, tx, r.principal, rules); err != nil {
			return outcome{}, err
		}
		params.Split = &rules
	}
	if params.Pix, err = pixOptionsParam(body.Pix); err != nil {
		return outcome{}, err
	}
	if body.RequestThreeDSecure != nil {
		threeDS := string(*body.RequestThreeDSecure)
		params.RequestThreeDSecure = &threeDS
	}
	if body.SetupFutureUsage != nil {
		setup := string(*body.SetupFutureUsage)
		params.SetupFutureUsage = &setup
	}
	if body.Amount != nil {
		current, err := a.deps.Payments.Intent(ctx, tx, paymentsOwner(r.principal), intentID)
		if err != nil {
			return outcome{}, paymentsError(err, r.pathID)
		}
		amount, err := money.New(*body.Amount, current.Amount.Currency())
		if err != nil {
			return outcome{}, err
		}
		params.Amount = &amount
	}
	it, err := a.deps.Payments.Update(ctx, tx, paymentsOwner(r.principal), intentID, params)
	if err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	return respond(paymentIntentJSON(it))
}

func (a *API) confirmPaymentIntent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	intentID, err := intentFromPath(r)
	if err != nil {
		return outcome{}, err
	}
	var body openapi.ConfirmPaymentIntentRequest
	if err := decode(r.body, &body, true); err != nil {
		return outcome{}, err
	}
	returnURL, err := checkReturnURL(body.ReturnUrl)
	if err != nil {
		return outcome{}, err
	}
	confirm := payments.ConfirmParams{
		PaymentMethod: deref(body.PaymentMethod), OffSession: body.OffSession != nil && *body.OffSession, IP: deref(body.CustomerIp),
	}
	it, step, err := a.deps.Payments.StartConfirm(ctx, tx, paymentsOwner(r.principal), intentID, confirm)
	if err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	r.state[stateIntent], r.state[stateReturnURL] = intentID.String(), returnURL
	return a.next(r, it, step)
}

// maxReturnURL bounds the return_url a merchant may send.
const maxReturnURL = 2048

// checkReturnURL accepts where the customer goes after 3-D Secure: an absolute HTTPS
// address, without credentials in it.
func checkReturnURL(raw *string) (string, error) {
	s := deref(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil || len(s) > maxReturnURL || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return "", invalidRequest("url_invalid", "return_url", "return_url must be an absolute https URL.")
	}
	return s, nil
}

func (a *API) authenticateCardholder(ctx context.Context, r *request) error {
	res, err := a.deps.Payments.Authenticate(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateIntent), r.state[stateReturnURL])
	r.scratch = res
	return err
}

func (a *API) finishAuthentication(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	res, _ := r.scratch.(payments.Authentication)
	it, step, err := a.deps.Payments.FinishAuthentication(ctx, tx, paymentsOwner(r.principal), stateID(r, stateIntent), res)
	if err != nil {
		return outcome{}, err
	}
	return a.next(r, it, step)
}

// ResumePayment carries a payment on to its authorization after the cardholder completed
// a 3-D Secure challenge, as the operation that confirmed it would have. Should it stop
// half-way, the resolver picks the attempt up.
func (a *API) ResumePayment(ctx context.Context, owner payments.Owner, intentID string) error {
	req := &request{
		principal: merchant.Principal{Merchant: owner.Merchant, Livemode: owner.Livemode, Kind: merchant.Secret, APIVersion: CurrentVersion},
		version:   CurrentVersion, requestID: requestPrefix.New().String() + "_3ds",
		state: map[string]string{stateIntent: intentID},
	}
	_, _, err := a.runPhases(ctx, a.operations["resume_payment_intent"], req, 0, pgtype.UUID{}, pointAuthorizing)
	if _, isClientError := asError(err); isClientError {
		return nil
	}
	return err
}

func (a *API) completeAction(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	if r.principal.Livemode {
		return outcome{}, invalidRequest("livemode_unsupported", "", "Test helpers are only available in test mode.")
	}
	intentID, err := intentFromPath(r)
	if err != nil {
		return outcome{}, err
	}
	var body openapi.CompleteActionRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	if body.Outcome != "succeeded" && body.Outcome != "failed" {
		return outcome{}, invalidRequest("parameter_invalid", "outcome", "outcome must be succeeded or failed.")
	}
	it, step, err := a.deps.Payments.CompleteAction(ctx, tx, paymentsOwner(r.principal), intentID, body.Outcome == "succeeded")
	if err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	r.state[stateIntent] = intentID.String()
	if step == payments.StepConfirm {
		return proceed(pointAuthorizing)
	}
	return respondIntent(r, it)
}

func (a *API) authorizeOnRail(ctx context.Context, r *request) error {
	res, err := a.deps.Payments.Authorize(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateIntent))
	r.scratch = res
	return err
}

func (a *API) finishAuthorization(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	it, step, err := a.deps.Payments.FinishAuthorization(ctx, tx, paymentsOwner(r.principal), stateID(r, stateIntent), railResult(r))
	if err != nil {
		return outcome{}, err
	}
	if step == payments.StepCapture {
		return proceed(pointCapturing)
	}
	return respondIntent(r, it)
}

func (a *API) capturePaymentIntent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	intentID, err := intentFromPath(r)
	if err != nil {
		return outcome{}, err
	}
	var body openapi.CapturePaymentIntentRequest
	if err := decode(r.body, &body, true); err != nil {
		return outcome{}, err
	}
	var amount *money.Amount
	if body.AmountToCapture != nil {
		current, err := a.deps.Payments.Intent(ctx, tx, paymentsOwner(r.principal), intentID)
		if err != nil {
			return outcome{}, paymentsError(err, r.pathID)
		}
		value, err := money.New(*body.AmountToCapture, current.Amount.Currency())
		if err != nil {
			return outcome{}, err
		}
		amount = &value
	}
	if _, err := a.deps.Payments.StartCapture(ctx, tx, paymentsOwner(r.principal), intentID, amount); err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	r.state[stateIntent] = intentID.String()
	return proceed(pointCapturing)
}

func (a *API) captureOnRail(ctx context.Context, r *request) error {
	res, err := a.deps.Payments.Capture(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateIntent))
	r.scratch = res
	return err
}

func (a *API) finishCapture(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	it, err := a.deps.Payments.FinishCapture(ctx, tx, paymentsOwner(r.principal), stateID(r, stateIntent), railResult(r))
	if err != nil {
		return outcome{}, err
	}
	return respond(paymentIntentJSON(it))
}

func (a *API) cancelPaymentIntent(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	intentID, err := intentFromPath(r)
	if err != nil {
		return outcome{}, err
	}
	var body openapi.CancelPaymentIntentRequest
	if err := decode(r.body, &body, true); err != nil {
		return outcome{}, err
	}
	reason := "requested_by_customer"
	if body.CancellationReason != nil {
		reason = string(*body.CancellationReason)
	}
	it, step, err := a.deps.Payments.StartCancel(ctx, tx, paymentsOwner(r.principal), intentID, reason)
	if err != nil {
		return outcome{}, paymentsError(err, r.pathID)
	}
	r.state[stateIntent] = intentID.String()
	if step == payments.StepVoid {
		return proceed(pointVoiding)
	}
	return respond(paymentIntentJSON(it))
}

func (a *API) voidOnRail(ctx context.Context, r *request) error {
	res, err := a.deps.Payments.Void(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateIntent))
	r.scratch = res
	return err
}

func (a *API) finishCancel(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	it, err := a.deps.Payments.FinishCancel(ctx, tx, paymentsOwner(r.principal), stateID(r, stateIntent), railResult(r))
	if err != nil {
		return outcome{}, err
	}
	return respond(paymentIntentJSON(it))
}

func (a *API) createRefund(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	var body openapi.CreateRefundRequest
	if err := decode(r.body, &body, false); err != nil {
		return outcome{}, err
	}
	intentID, err := payments.IntentPrefix.Parse(body.PaymentIntent)
	if err != nil {
		return outcome{}, invalidRequest("resource_missing", "payment_intent", "No such payment_intent: %q", body.PaymentIntent)
	}
	params := payments.RefundParams{Intent: intentID}
	if body.Reason != nil {
		params.Reason = string(*body.Reason)
	}
	if body.Amount != nil {
		current, err := a.deps.Payments.Intent(ctx, tx, paymentsOwner(r.principal), intentID)
		if err != nil {
			return outcome{}, paymentsError(err, body.PaymentIntent)
		}
		amount, err := money.New(*body.Amount, current.Amount.Currency())
		if err != nil {
			return outcome{}, err
		}
		params.Amount = &amount
	}
	refund, err := a.deps.Payments.StartRefund(ctx, tx, paymentsOwner(r.principal), params)
	if err != nil {
		return outcome{}, paymentsError(err, body.PaymentIntent)
	}
	r.state[stateRefund] = refund.ID.String()
	return proceed(pointRefunding)
}

func (a *API) refundOnRail(ctx context.Context, r *request) error {
	res, err := a.deps.Payments.RefundOnRail(ctx, a.deps.Pool, paymentsOwner(r.principal), stateID(r, stateRefund))
	r.scratch = res
	return err
}

func (a *API) finishRefund(ctx context.Context, tx pgx.Tx, r *request) (outcome, error) {
	refund, err := a.deps.Payments.FinishRefund(ctx, tx, paymentsOwner(r.principal), stateID(r, stateRefund), railResult(r))
	if err != nil {
		return outcome{}, err
	}
	return respond(refundJSON(refund))
}

// respondIntent answers with the intent, or with a 402 card_error carrying it when the
// attempt just failed, as Stripe does, so a client handling errors sees the decline.
func respondIntent(r *request, it payments.Intent) (outcome, error) {
	if it.Status != payments.RequiresPaymentMethod || it.LastError == nil {
		return respond(paymentIntentJSON(it))
	}
	body := paymentIntentJSON(it)
	e := openapi.Error{
		Type: openapi.CardError, Code: it.LastError.Code, Message: it.LastError.Message,
		RequestId: r.requestID, DocUrl: docBase + it.LastError.Code, PaymentIntent: &body,
	}
	if it.LastError.DeclineCode != "" {
		e.DeclineCode = &it.LastError.DeclineCode
	}
	return outcome{status: http.StatusPaymentRequired, body: openapi.ErrorResponse{Error: e}}, nil
}

func railResult(r *request) payments.Result {
	res, _ := r.scratch.(payments.Result)
	return res
}

func stateID(r *request, key string) id.ID {
	parsed, _ := id.Parse(r.state[key]) // set by this request's first phase
	return parsed
}

func intentFromPath(r *request) (id.ID, error) {
	intentID, err := payments.IntentPrefix.Parse(r.pathID)
	if err != nil {
		return id.ID{}, notFound("payment_intent", r.pathID)
	}
	return intentID, nil
}

func parseAmount(minor int64, currency, param string) (money.Amount, error) {
	c, err := money.CurrencyByCode(strings.ToUpper(currency))
	if err != nil {
		return money.Amount{}, invalidRequest("parameter_invalid", "currency", "Unsupported currency %q.", currency)
	}
	amount, err := money.New(minor, c)
	if err != nil || !amount.IsPositive() {
		return money.Amount{}, invalidRequest("parameter_invalid", param, "%s must be a positive number of minor units.", param)
	}
	if minor > maxAmount {
		return money.Amount{}, invalidRequest("amount_too_large", param, "%s is at most %d minor units.", param, maxAmount)
	}
	return amount, nil
}

// maxAmount bounds one payment or payout: R$ 10,000,000.00, well within what the rails'
// messages can carry and beyond what a mistyped amount should move.
const maxAmount = 10_000_000_00

func paymentsError(err error, objectID string) error {
	for sentinel, code := range map[error]string{
		payments.ErrInvalid:           "parameter_invalid",
		payments.ErrInvalidState:      "payment_intent_unexpected_state",
		payments.ErrRailUnavailable:   "livemode_unsupported",
		payments.ErrAmountTooLarge:    "amount_too_large",
		payments.ErrInsufficientFunds: "balance_insufficient",
		payments.ErrPayoutLimit:       "payout_limit_reached",
	} {
		if errors.Is(err, sentinel) {
			return invalidRequest(code, "", "%s", strings.TrimPrefix(err.Error(), sentinel.Error()+": "))
		}
	}
	if errors.Is(err, payments.ErrNotFound) {
		resource := "payment_intent"
		switch {
		case strings.HasPrefix(objectID, "re_"):
			resource = "refund"
		case strings.HasPrefix(objectID, "pm_"):
			resource = "payment_method"
		case strings.HasPrefix(objectID, "po_"):
			resource = "payout"
		}
		return notFound(resource, objectID)
	}
	return err
}

func installmentsParam(i *openapi.Installments) *payments.Installments {
	if i == nil {
		return nil
	}
	return &payments.Installments{Count: i.Count, FinancedBy: payments.Financing(i.FinancedBy)}
}
