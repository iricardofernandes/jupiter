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
	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

// Step tells the caller what an operation needs next.
type Step string

const (
	StepDone    Step = ""
	StepCapture Step = "capture"
	StepVoid    Step = "void"
	StepConfirm Step = "authorize"
)

type CreateParams struct {
	Amount        money.Amount
	CaptureMethod CaptureMethod
	PaymentMethod string
	Description   string
}

type UpdateParams struct {
	Amount        *money.Amount
	PaymentMethod *string
	Description   *string
}

const maxDescription = 1000

func (s *Service) Create(ctx context.Context, tx pgx.Tx, owner Owner, p CreateParams) (Intent, error) {
	if p.CaptureMethod == "" {
		p.CaptureMethod = CaptureAutomatic
	}
	if err := validateIntent(p.Amount, p.CaptureMethod, p.PaymentMethod, p.Description); err != nil {
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
		Status: string(status), PaymentMethod: p.PaymentMethod, Description: p.Description,
	}
	q := db.New(tx)
	if err := q.InsertIntent(ctx, db.InsertIntentParams{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Amount: row.Amount, Currency: row.Currency,
		CaptureMethod: row.CaptureMethod, Status: row.Status, PaymentMethod: row.PaymentMethod,
		Description: row.Description, CreatedAt: ts(now),
	}); err != nil {
		return Intent{}, fmt.Errorf("creating payment intent: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypePaymentIntentCreated, row.ID); err != nil {
		return Intent{}, err
	}
	row.CreatedAt = ts(now)
	return intentFromRow(row)
}

func validateIntent(amount money.Amount, method CaptureMethod, pm, description string) error {
	switch {
	case !amount.IsPositive():
		return fmt.Errorf("%w: amount must be a positive number of minor units", ErrInvalid)
	case method != CaptureAutomatic && method != CaptureManual:
		return fmt.Errorf("%w: capture_method must be automatic or manual", ErrInvalid)
	case pm != "" && !knownPaymentMethod(pm):
		return fmt.Errorf("%w: unknown payment_method %q", ErrInvalid, pm)
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
	if p.PaymentMethod != nil {
		row.PaymentMethod = *p.PaymentMethod
	}
	if p.Description != nil {
		row.Description = *p.Description
	}
	if err := validateIntent(amount, CaptureMethod(row.CaptureMethod), row.PaymentMethod, row.Description); err != nil {
		return Intent{}, err
	}
	row.Amount = amount.Minor()
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
func (s *Service) StartConfirm(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, paymentMethod string) (Intent, error) {
	q := db.New(tx)
	row, err := lockIntent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, err
	}
	if paymentMethod != "" {
		if !knownPaymentMethod(paymentMethod) {
			return Intent{}, fmt.Errorf("%w: unknown payment_method %q", ErrInvalid, paymentMethod)
		}
		row.PaymentMethod = paymentMethod
	}
	status := Status(row.Status)
	switch {
	case status != RequiresPaymentMethod && status != RequiresConfirmation:
		return Intent{}, fmt.Errorf("%w: a %s payment intent cannot be confirmed", ErrInvalidState, status)
	case row.PaymentMethod == "":
		return Intent{}, fmt.Errorf("%w: a payment_method is needed to confirm", ErrInvalid)
	}
	if _, err := s.rail(owner.Livemode); err != nil {
		return Intent{}, err
	}
	number, err := q.NextAttemptNumber(ctx, row.ID)
	if err != nil {
		return Intent{}, err
	}
	attemptID := AttemptPrefix.New().String()
	if err := q.InsertAttempt(ctx, db.InsertAttemptParams{
		ID: attemptID, IntentID: row.ID, Number: number, PaymentMethod: row.PaymentMethod,
		Amount: row.Amount, Status: string(attemptAuthorizing), CreatedAt: ts(s.cfg.Now().UTC()),
	}); err != nil {
		return Intent{}, fmt.Errorf("recording attempt: %w", err)
	}
	row.LatestAttempt = text(attemptID)
	row.LastErrorCode, row.LastDeclineCode, row.LastErrorMessage = "", "", ""
	if err := s.setStatus(ctx, tx, &row, Processing); err != nil {
		return Intent{}, err
	}
	return s.save(ctx, q, row)
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
	return s.authorizeOnRail(ctx, intent, attempt)
}

func (s *Service) authorizeOnRail(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.rail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	return rail.Authorize(ctx, AuthorizeRequest{
		Key: attempt.ID, Amount: amount, PaymentMethod: attempt.PaymentMethod, Authenticated: attempt.Authenticated,
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
	case Declined:
		attempt.Status, attempt.RailReference, attempt.DeclineCode = string(attemptDeclined), res.Reference, res.DeclineCode
		err = s.fail(ctx, tx, &row, "card_declined", res.DeclineCode, "The card was declined.")
	case ActionRequired:
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
	expires := s.cfg.Now().UTC().Add(s.cfg.AuthorizationValidity)
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
	if eventType == events.TypeRefundCreated || eventType == events.TypeRefundUpdated {
		objectType = "refund"
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
