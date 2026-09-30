package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/payments/rules"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

// Step tells the caller what an operation needs next.
type Step string

const (
	StepDone    Step = ""
	StepCapture Step = "capture"
	StepVoid    Step = "void"
	StepConfirm Step = "authorize"
	// StepAuthenticate asks the cardholder's issuer to authenticate them (3-D Secure)
	// before the authorization.
	StepAuthenticate Step = "authenticate"
)

// Three-D Secure requests.
const (
	ThreeDSAutomatic = "automatic"
	ThreeDSAny       = "any"
)

type CreateParams struct {
	Amount              money.Amount
	CaptureMethod       CaptureMethod
	PaymentMethod       string
	Description         string
	Installments        *Installments
	SetupFutureUsage    string
	RequestThreeDSecure string
	// Pix says how a Pix charge is made, for the payment method pix.
	Pix *PixOptions
}

type UpdateParams struct {
	Amount              *money.Amount
	PaymentMethod       *string
	Description         *string
	Installments        *Installments
	SetupFutureUsage    *string
	RequestThreeDSecure *string
	Pix                 *PixOptions
}

// ConfirmParams says how to authorize: OffSession for a merchant-initiated payment,
// made without the cardholder, on a card an earlier payment stored. IP is the
// customer's address, for the risk engine.
type ConfirmParams struct {
	PaymentMethod string
	OffSession    bool
	IP            string
}

const maxDescription = 1000

func (s *Service) Create(ctx context.Context, tx pgx.Tx, owner Owner, p CreateParams) (Intent, error) {
	if p.CaptureMethod == "" {
		p.CaptureMethod = CaptureAutomatic
	}
	if p.RequestThreeDSecure == "" {
		p.RequestThreeDSecure = ThreeDSAutomatic
	}
	if err := validateIntent(p.Amount, p.CaptureMethod, p.Description, p.Installments, p.SetupFutureUsage, p.RequestThreeDSecure); err != nil {
		return Intent{}, err
	}
	if err := s.checkPaymentMethod(ctx, tx, owner, p.PaymentMethod); err != nil {
		return Intent{}, err
	}
	if err := s.checkPix(p.PaymentMethod, p.Pix, p.Amount, p.CaptureMethod, p.Installments, p.SetupFutureUsage); err != nil {
		return Intent{}, err
	}
	status := RequiresPaymentMethod
	if p.PaymentMethod != "" {
		status = RequiresConfirmation
	}
	now := s.cfg.Now().UTC()
	row := db.PaymentsIntent{
		ID: IntentPrefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		Amount: p.Amount.Minor(), Currency: p.Amount.Currency().Code(), CaptureMethod: string(p.CaptureMethod),
		Status: string(status), PaymentMethod: p.PaymentMethod, Description: p.Description, SetupFutureUsage: p.SetupFutureUsage,
		RequestThreeDSecure: p.RequestThreeDSecure, PixOptions: pixOptionsColumn(p.Pix),
	}
	row.Installments, row.InstallmentsFinancedBy = installmentColumns(p.Installments)
	q := db.New(tx)
	if err := q.InsertIntent(ctx, db.InsertIntentParams{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Amount: row.Amount, Currency: row.Currency,
		CaptureMethod: row.CaptureMethod, Status: row.Status, PaymentMethod: row.PaymentMethod,
		Description: row.Description, Installments: row.Installments, InstallmentsFinancedBy: row.InstallmentsFinancedBy,
		SetupFutureUsage: row.SetupFutureUsage, RequestThreeDSecure: row.RequestThreeDSecure, PixOptions: row.PixOptions,
		CreatedAt: ts(now),
	}); err != nil {
		return Intent{}, fmt.Errorf("creating payment intent: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypePaymentIntentCreated, row.ID); err != nil {
		return Intent{}, err
	}
	row.CreatedAt = ts(now)
	return intentFromRow(row)
}

const maxInstallments = 12

func validateIntent(amount money.Amount, method CaptureMethod, description string, installments *Installments, setup, threeDS string) error {
	if installments != nil {
		switch {
		case installments.Count < 2 || installments.Count > maxInstallments:
			return fmt.Errorf("%w: installments[count] must be between 2 and %d", ErrInvalid, maxInstallments)
		case installments.FinancedBy != FinancedByMerchant && installments.FinancedBy != FinancedByIssuer:
			return fmt.Errorf("%w: installments[financed_by] must be merchant or issuer", ErrInvalid)
		case amount.Currency() != money.BRL:
			return fmt.Errorf("%w: installments are only for payments in BRL", ErrInvalid)
		}
	}
	switch {
	case setup != "" && setup != SetupOffSession:
		return fmt.Errorf("%w: setup_future_usage must be off_session", ErrInvalid)
	case threeDS != ThreeDSAutomatic && threeDS != ThreeDSAny:
		return fmt.Errorf("%w: request_three_d_secure must be automatic or any", ErrInvalid)
	case !amount.IsPositive():
		return fmt.Errorf("%w: amount must be a positive number of minor units", ErrInvalid)
	case method != CaptureAutomatic && method != CaptureManual:
		return fmt.Errorf("%w: capture_method must be automatic or manual", ErrInvalid)
	case len(description) > maxDescription:
		return fmt.Errorf("%w: description is longer than %d characters", ErrInvalid, maxDescription)
	}
	return nil
}

func (s *Service) Update(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, p UpdateParams) (Intent, error) {
	q := db.New(tx)
	row, err := lockIntent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, err
	}
	status := Status(row.Status)
	if status != RequiresPaymentMethod && status != RequiresConfirmation {
		return Intent{}, fmt.Errorf("%w: a %s payment intent cannot be updated", ErrInvalidState, status)
	}
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return Intent{}, err
	}
	if p.Amount != nil {
		if p.Amount.Currency() != amount.Currency() {
			return Intent{}, fmt.Errorf("%w: the currency of a payment intent cannot change", ErrInvalid)
		}
		amount = *p.Amount
	}
	applyUpdate(&row, p)
	if err := validateIntent(amount, CaptureMethod(row.CaptureMethod), row.Description, installmentsOf(row), row.SetupFutureUsage, row.RequestThreeDSecure); err != nil {
		return Intent{}, err
	}
	row.Amount = amount.Minor()
	if err := s.checkPixRow(row); err != nil {
		return Intent{}, err
	}
	if p.PaymentMethod != nil {
		if err := s.checkPaymentMethod(ctx, tx, owner, row.PaymentMethod); err != nil {
			return Intent{}, err
		}
	}
	next := RequiresPaymentMethod
	if row.PaymentMethod != "" {
		next = RequiresConfirmation
	}
	if next != status {
		if err := s.setStatus(ctx, tx, &row, next); err != nil {
			return Intent{}, err
		}
	}
	return s.save(ctx, q, row)
}

// applyUpdate sets the fields an update names; they are checked after.
func applyUpdate(row *db.PaymentsIntent, p UpdateParams) {
	if p.PaymentMethod != nil {
		if *p.PaymentMethod != row.PaymentMethod && p.Pix == nil {
			row.PixOptions = nil // paid otherwise now: the options of the old method no longer apply
		}
		row.PaymentMethod = *p.PaymentMethod
	}
	if p.Description != nil {
		row.Description = *p.Description
	}
	if p.Installments != nil {
		row.Installments, row.InstallmentsFinancedBy = installmentColumns(p.Installments)
	}
	if p.SetupFutureUsage != nil {
		row.SetupFutureUsage = *p.SetupFutureUsage
	}
	if p.RequestThreeDSecure != nil {
		row.RequestThreeDSecure = *p.RequestThreeDSecure
	}
	if p.Pix != nil {
		row.PixOptions = pixOptionsColumn(p.Pix)
	}
}

func (s *Service) Intent(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (Intent, error) {
	row, err := db.New(q).GetIntent(ctx, db.GetIntentParams{ID: intentID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return Intent{}, fmt.Errorf("%w: %s", ErrNotFound, intentID)
	}
	if err != nil {
		return Intent{}, err
	}
	return intentFromRow(row)
}

func (s *Service) Intents(ctx context.Context, q db.DBTX, owner Owner, r page.Request) ([]Intent, bool, error) {
	rows, err := db.New(q).ListIntents(ctx, db.ListIntentsParams{
		MerchantID: owner.Merchant.String(), Livemode: owner.Livemode,
		StartingAfter: r.StartingAfter, EndingBefore: r.EndingBefore, MaxCount: r.Fetch(),
	})
	if err != nil {
		return nil, false, fmt.Errorf("listing payment intents: %w", err)
	}
	out := make([]Intent, 0, len(rows))
	for _, row := range rows {
		it, err := intentFromRow(row)
		if err != nil {
			return nil, false, err
		}
		out = append(out, it)
	}
	items, more := page.Trim(out, r)
	return items, more, nil
}

// StartConfirm records a new attempt and moves the intent to processing, before any
// call to the rail: whatever happens next, the attempt exists to be resolved.
func (s *Service) StartConfirm(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, p ConfirmParams) (Intent, Step, error) {
	q := db.New(tx)
	row, err := lockIntent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	if p.PaymentMethod != "" {
		if err := s.checkPaymentMethod(ctx, tx, owner, p.PaymentMethod); err != nil {
			return Intent{}, StepDone, err
		}
		row.PaymentMethod = p.PaymentMethod
	}
	if err := s.checkPixRow(row); err != nil {
		return Intent{}, StepDone, err
	}
	status := Status(row.Status)
	switch {
	case status != RequiresPaymentMethod && status != RequiresConfirmation:
		return Intent{}, StepDone, fmt.Errorf("%w: a %s payment intent cannot be confirmed", ErrInvalidState, status)
	case row.PaymentMethod == "":
		return Intent{}, StepDone, fmt.Errorf("%w: a payment_method is needed to confirm", ErrInvalid)
	case p.IP != "" && !validIP(p.IP):
		return Intent{}, StepDone, fmt.Errorf("%w: customer_ip is not an IP address", ErrInvalid)
	}
	if err := s.checkRail(owner, row.PaymentMethod, p.OffSession); err != nil {
		return Intent{}, StepDone, err
	}
	if err := s.checkStoredCredential(ctx, tx, owner, row, p.OffSession); err != nil {
		return Intent{}, StepDone, err
	}
	attemptID, err := s.newAttempt(ctx, q, row, p)
	if err != nil {
		return Intent{}, StepDone, err
	}
	startAttempt(&row, attemptID)
	if err := s.setStatus(ctx, tx, &row, Processing); err != nil {
		return Intent{}, StepDone, err
	}
	attempt, err := q.LockAttempt(ctx, attemptID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	// The risk engine and 3-D Secure are for cards: a Pix is pushed by the payer, from
	// their own bank, which authenticates them.
	step := StepConfirm
	if row.PaymentMethod != PaymentMethodPix {
		if step, err = s.decide(ctx, tx, owner, &row, &attempt, p.OffSession); err != nil {
			return Intent{}, StepDone, err
		}
	}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, StepDone, err
	}
	it, err := s.save(ctx, q, row)
	return it, step, err
}

// startAttempt makes attemptID the intent's latest and forgets what the previous one
// left.
func startAttempt(row *db.PaymentsIntent, attemptID string) {
	row.LatestAttempt = text(attemptID)
	row.LastErrorCode, row.LastDeclineCode, row.LastErrorMessage = "", "", ""
	row.RiskDecision, row.RiskDecisionID, row.NextAction, row.NextActionUrl = "", "", "", ""
	row.NextActionData, row.NextActionExpiresAt = "", pgtype.Timestamptz{}
}

func (s *Service) newAttempt(ctx context.Context, q *db.Queries, row db.PaymentsIntent, p ConfirmParams) (string, error) {
	number, err := q.NextAttemptNumber(ctx, row.ID)
	if err != nil {
		return "", err
	}
	initiator := "customer"
	if p.OffSession {
		initiator = "merchant"
	}
	attemptID := AttemptPrefix.New().String()
	if err := q.InsertAttempt(ctx, db.InsertAttemptParams{
		ID: attemptID, IntentID: row.ID, Number: number, PaymentMethod: row.PaymentMethod,
		Amount: row.Amount, Status: string(attemptAuthorizing), Initiator: initiator,
		StoresCredential: row.SetupFutureUsage == SetupOffSession, Installments: row.Installments,
		InstallmentsFinancedBy: row.InstallmentsFinancedBy, Ip: p.IP, CreatedAt: ts(s.cfg.Now().UTC()),
	}); err != nil {
		return "", fmt.Errorf("recording attempt: %w", err)
	}
	return attemptID, nil
}

// Authorize sends the intent's attempt to the rail. It runs outside any transaction and
// may be repeated: the attempt id is the idempotency key. An empty outcome means the
// attempt is no longer waiting for authorization.
func (s *Service) Authorize(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (Result, error) {
	intent, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return Result{}, err
	}
	if !inStatus(attempt, attemptAuthorizing, attemptAuthorizationUnknown) {
		return Result{}, nil
	}
	return s.authorizeOnRail(ctx, q, owner, intent, attempt)
}

func (s *Service) authorizeOnRail(ctx context.Context, q db.DBTX, owner Owner, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	if isPix(attempt.PaymentMethod) {
		return s.chargePix(ctx, intent, attempt)
	}
	rail, err := s.rail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	card, err := s.cardFor(ctx, q, owner, attempt.PaymentMethod)
	if err != nil {
		return Result{}, err
	}
	first := ""
	if attempt.Initiator == "merchant" {
		if first, err = s.firstTransaction(ctx, q, owner, attempt.PaymentMethod); err != nil {
			return Result{}, err
		}
	}
	var installments *Installments
	if attempt.Installments.Valid {
		installments = &Installments{Count: int(attempt.Installments.Int32), FinancedBy: Financing(attempt.InstallmentsFinancedBy.String)}
	}
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	return rail.Authorize(ctx, AuthorizeRequest{
		Key: attempt.ID, Merchant: owner.Merchant.String(), Amount: amount, PaymentMethod: attempt.PaymentMethod, Card: card,
		Authenticated: attempt.Authenticated, Installments: installments, MerchantInitiated: attempt.Initiator == "merchant",
		FirstTransaction: first, StoresCredential: attempt.StoresCredential, Authentication: authenticationOf(attempt),
	}), nil
}

// FinishAuthorization applies the rail's answer. It changes nothing unless the attempt
// is still waiting for one, so the request, the completer and the resolver can all call
// it with the same answer.
func (s *Service) FinishAuthorization(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, res Result) (Intent, Step, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil || res.Outcome == "" || !inStatus(attempt, attemptAuthorizing, attemptAuthorizationUnknown) {
		return s.finish(ctx, q, row, err)
	}
	now := s.cfg.Now().UTC()
	step := StepDone
	switch res.Outcome {
	case Approved:
		step, err = s.authorized(ctx, tx, &row, &attempt, res)
		if err == nil {
			err = s.recordOutcome(ctx, tx, attempt.ID, true)
		}
	case Declined:
		if isPix(attempt.PaymentMethod) {
			attempt.Status, attempt.DeclineCode = string(attemptDeclined), res.DeclineCode
			err = s.fail(ctx, tx, &row, "payment_intent_payment_attempt_failed", res.DeclineCode, "The bank did not make the Pix charge.")
			break
		}
		attempt.Status, attempt.RailReference, attempt.DeclineCode = string(attemptDeclined), res.Reference, res.DeclineCode
		err = s.fail(ctx, tx, &row, "card_declined", res.DeclineCode, "The card was declined.")
		if err == nil {
			err = s.recordOutcome(ctx, tx, attempt.ID, false)
		}
	case ActionRequired:
		if res.Pix != nil {
			err = s.awaitPix(ctx, tx, &row, &attempt, *res.Pix)
			break
		}
		attempt.Status, attempt.RailReference = string(attemptRequiresAction), res.Reference
		row.NextAction = "use_test_authentication"
		err = s.setStatus(ctx, tx, &row, RequiresAction)
	default:
		attempt.Status = string(attemptAuthorizationUnknown)
		if !attempt.UnknownSince.Valid {
			attempt.UnknownSince = ts(now)
		}
	}
	if err != nil {
		return Intent{}, StepDone, err
	}
	attempt.UpdatedAt = ts(now)
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, StepDone, err
	}
	it, err := s.save(ctx, q, row)
	return it, step, err
}

// authorized places the hold on the ledger that mirrors the authorization.
func (s *Service) authorized(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, res Result) (Step, error) {
	owner, err := ownerOf(*row)
	if err != nil {
		return StepDone, err
	}
	currency := mustCurrency(row.Currency)
	accts, err := s.ledgerAccounts(ctx, tx, owner, currency)
	if err != nil {
		return StepDone, err
	}
	amount, err := money.New(attempt.Amount, currency)
	if err != nil {
		return StepDone, err
	}
	now := s.cfg.Now().UTC()
	scheme, err := s.schemeOf(ctx, tx, owner, attempt.PaymentMethod)
	if err != nil {
		return StepDone, err
	}
	validity := rules.AuthorizationValidity(rules.Authorization{
		Scheme: scheme, Presence: rules.CardNotPresent, Initiator: rules.Initiator(attempt.Initiator), Kind: rules.Final,
	}, now)
	expires := now.Add(validity.Duration)
	// The ledger expires the hold a day after the authorization, as a backstop: the
	// expiry job voids it on the rail and the ledger together before then.
	hold, err := s.cfg.Ledger.Hold(ctx, tx, ledger.Hold{
		Description: "authorization " + attempt.ID, Debit: accts.networkReceivable, Credit: accts.merchantBalance,
		Amount: amount, ExpiresAt: expires.Add(backstop),
	})
	if err != nil {
		return StepDone, fmt.Errorf("placing ledger hold: %w", err)
	}
	attempt.Status, attempt.RailReference = string(attemptAuthorized), res.Reference
	attempt.NetworkTransactionID = res.NetworkTransactionID
	if attempt.StoresCredential && res.NetworkTransactionID != "" {
		if err := db.New(tx).SetPaymentMethodNetworkTransaction(ctx, db.SetPaymentMethodNetworkTransactionParams{
			ID: attempt.PaymentMethod, NetworkTransactionID: res.NetworkTransactionID,
		}); err != nil {
			return StepDone, err
		}
	}
	attempt.LedgerHold = text(hold.ID.String())
	attempt.AuthorizationExpiresAt = ts(expires)
	attempt.UnknownSince = pgtype.Timestamptz{}
	if CaptureMethod(row.CaptureMethod) == CaptureAutomatic {
		attempt.Status = string(attemptCapturing)
		attempt.CaptureAmount = pgtype.Int8{Int64: attempt.Amount, Valid: true}
		return StepCapture, nil
	}
	row.AmountCapturable = attempt.Amount
	return StepDone, s.setStatus(ctx, tx, row, RequiresCapture)
}

// CompleteAction answers a requires_action intent in test mode, as a cardholder passing
// or failing authentication would.
func (s *Service) CompleteAction(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, succeeded bool) (Intent, Step, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	if Status(row.Status) != RequiresAction || !inStatus(attempt, attemptRequiresAction) {
		return Intent{}, StepDone, fmt.Errorf("%w: the payment intent is not waiting for an action", ErrInvalidState)
	}
	if isPix(attempt.PaymentMethod) {
		return Intent{}, StepDone, fmt.Errorf("%w: a Pix payment completes when its Pix arrives, not with the test helper", ErrInvalidState)
	}
	row.NextAction = ""
	step := StepDone
	if succeeded {
		attempt.Status, attempt.Authenticated = string(attemptAuthorizing), true
		// The authorization starts now; the time spent authenticating must not count
		// towards giving up on it.
		attempt.UnknownSince = ts(s.cfg.Now().UTC())
		err = s.setStatus(ctx, tx, &row, Processing)
		step = StepConfirm
	} else {
		attempt.Status, attempt.DeclineCode = string(attemptFailed), "authentication_failed"
		err = s.fail(ctx, tx, &row, "payment_intent_authentication_failure", "", "The cardholder failed authentication.")
	}
	if err != nil {
		return Intent{}, StepDone, err
	}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, StepDone, err
	}
	it, err := s.save(ctx, q, row)
	return it, step, err
}

// checkPixRow applies Pix's rules to an intent as it stands.
func (s *Service) checkPixRow(row db.PaymentsIntent) error {
	amount, err := money.New(row.Amount, mustCurrency(row.Currency))
	if err != nil {
		return err
	}
	pix, err := pixOptionsOf(row)
	if err != nil {
		return err
	}
	return s.checkPix(row.PaymentMethod, pix, amount, CaptureMethod(row.CaptureMethod), installmentsOf(row), row.SetupFutureUsage)
}

// checkRail checks the mode has the rail the payment method needs.
func (s *Service) checkRail(owner Owner, pm string, offSession bool) error {
	if pm != PaymentMethodPix {
		_, err := s.rail(owner.Livemode)
		return err
	}
	if offSession {
		return fmt.Errorf("%w: a Pix payment is made by the customer; off_session does not apply", ErrInvalid)
	}
	_, err := s.pixRail(owner.Livemode)
	return err
}

// checkPix applies Pix's rules to an intent paid, or to be paid, by Pix; Pix options on
// an intent paid otherwise are refused.
func (s *Service) checkPix(pm string, o *PixOptions, amount money.Amount, method CaptureMethod, installments *Installments, setup string) error {
	if pm == PaymentMethodPixAutomatico || (o != nil && o.Recurring != nil) {
		return fmt.Errorf("%w: a Pix Automático payment is charged by its subscription; to collect it now, use another payment method", ErrInvalid)
	}
	if pm != PaymentMethodPix {
		if o != nil {
			return fmt.Errorf("%w: pix options are for the payment method pix", ErrInvalid)
		}
		return nil
	}
	return validatePix(o, amount, method, installments, setup, s.cfg.Now())
}

// fail returns the intent to requires_payment_method with the reason, keeping the
// failed attempt as history.
func (s *Service) fail(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, code, declineCode, message string) error {
	row.LastErrorCode, row.LastDeclineCode, row.LastErrorMessage = code, declineCode, message
	row.NextAction = ""
	return s.setStatus(ctx, tx, row, RequiresPaymentMethod)
}

var statusEvents = map[Status]string{
	Processing:      events.TypePaymentIntentProcessing,
	RequiresAction:  events.TypePaymentIntentRequiresAction,
	RequiresCapture: events.TypePaymentIntentCapturable,
	Succeeded:       events.TypePaymentIntentSucceeded,
	Canceled:        events.TypePaymentIntentCanceled,
}

// setStatus is the only place an intent's status changes: it enforces the state
// machine and publishes the matching event in the same transaction.
func (s *Service) setStatus(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, to Status) error {
	from := Status(row.Status)
	if err := checkTransition(from, to); err != nil {
		return err
	}
	row.Status = string(to)
	eventType := statusEvents[to]
	if to == RequiresPaymentMethod && (from == Processing || from == RequiresAction) {
		eventType = events.TypePaymentIntentPaymentFailed
	}
	if eventType == "" {
		return nil
	}
	owner, err := ownerOf(*row)
	if err != nil {
		return err
	}
	return s.publish(ctx, tx, owner, eventType, row.ID)
}

func (s *Service) publish(ctx context.Context, tx pgx.Tx, owner Owner, eventType, intentID string) error {
	objectType := "payment_intent"
	switch eventType {
	case events.TypeRefundCreated, events.TypeRefundUpdated:
		objectType = "refund"
	case events.TypePayoutCreated, events.TypePayoutPaid, events.TypePayoutFailed:
		objectType = "payout"
	}
	_, err := s.cfg.Events.Publish(ctx, tx, events.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}, eventType,
		events.ObjectRef{ID: intentID, Type: objectType})
	return err
}

func (s *Service) save(ctx context.Context, q *db.Queries, row db.PaymentsIntent) (Intent, error) {
	row.UpdatedAt = ts(s.cfg.Now().UTC())
	if err := saveIntent(ctx, q, row); err != nil {
		return Intent{}, fmt.Errorf("saving payment intent: %w", err)
	}
	return intentFromRow(row)
}

func (s *Service) finish(_ context.Context, _ *db.Queries, row db.PaymentsIntent, err error) (Intent, Step, error) {
	if err != nil {
		return Intent{}, StepDone, err
	}
	it, err := intentFromRow(row)
	return it, StepDone, err
}

func lockIntent(ctx context.Context, q *db.Queries, owner Owner, intentID id.ID) (db.PaymentsIntent, error) {
	row, err := q.LockIntent(ctx, db.LockIntentParams{ID: intentID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, fmt.Errorf("%w: %s", ErrNotFound, intentID)
	}
	return row, err
}

// current reads an intent and its latest attempt without locking them.
func (s *Service) current(ctx context.Context, q *db.Queries, owner Owner, intentID id.ID) (db.PaymentsIntent, db.PaymentsAttempt, error) {
	row, err := q.GetIntent(ctx, db.GetIntentParams{ID: intentID.String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, db.PaymentsAttempt{}, fmt.Errorf("%w: %s", ErrNotFound, intentID)
	}
	if err != nil || !row.LatestAttempt.Valid {
		return row, db.PaymentsAttempt{}, errors.Join(err, errAttemptNotLatest)
	}
	attempt, err := q.GetAttempt(ctx, row.LatestAttempt.String)
	return row, attempt, err
}

// lockCurrent locks an intent and then its latest attempt, always in that order.
func (s *Service) lockCurrent(ctx context.Context, q *db.Queries, owner Owner, intentID id.ID) (db.PaymentsIntent, db.PaymentsAttempt, error) {
	row, err := lockIntent(ctx, q, owner, intentID)
	if err != nil {
		return row, db.PaymentsAttempt{}, err
	}
	if !row.LatestAttempt.Valid {
		return row, db.PaymentsAttempt{}, nil
	}
	attempt, err := q.LockAttempt(ctx, row.LatestAttempt.String)
	return row, attempt, err
}

func inStatus(a db.PaymentsAttempt, statuses ...attemptStatus) bool {
	for _, s := range statuses {
		if a.Status == string(s) {
			return true
		}
	}
	return false
}

func ownerOf(row db.PaymentsIntent) (Owner, error) {
	merchantID, err := id.Parse(row.MerchantID)
	return Owner{Merchant: merchantID, Livemode: row.Livemode}, err
}

// mustCurrency reads a currency the database constrained and this package wrote.
func mustCurrency(code string) money.Currency {
	c, err := money.CurrencyByCode(code)
	if err != nil {
		panic(fmt.Sprintf("payments: stored currency %q: %v", code, err))
	}
	return c
}

func installmentColumns(i *Installments) (pgtype.Int4, pgtype.Text) {
	if i == nil {
		return pgtype.Int4{}, pgtype.Text{}
	}
	return pgtype.Int4{Int32: int32(i.Count), Valid: true}, text(string(i.FinancedBy)) //nolint:gosec // validated to be at most 12
}

func installmentsOf(row db.PaymentsIntent) *Installments {
	if !row.Installments.Valid {
		return nil
	}
	return &Installments{Count: int(row.Installments.Int32), FinancedBy: Financing(row.InstallmentsFinancedBy.String)}
}
