package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// Pix Automático: each charge of a subscription is a payment intent paid by the method
// pix_automatico. Its attempt's charge is a recurring charge (cobr) at the bank, under
// the recurrence the payer authorized; it waits, scheduled, for the payer's bank to debit
// it on its date. Only subscriptions make these intents; the bank's word, read by
// SyncRecurringPayment, settles or fails them.

const PaymentMethodPixAutomatico = "pix_automatico"

// isPix: both Pix methods are paid, refunded and resolved through the Pix rail.
func isPix(pm string) bool { return pm == PaymentMethodPix || pm == PaymentMethodPixAutomatico }

// PixRecurring is what a Pix Automático payment is charged under.
type PixRecurring struct {
	Subscription string `json:"subscription"`
	RecurrenceID string `json:"recurrence_id"`
	DueDate      string `json:"due_date"`
}

type RecurringChargeRequest struct {
	TxID         string
	RecurrenceID string
	DueDate      string
	Amount       money.Amount
	Description  string
}

// Recurring charge statuses, and those of its attempts, as the API Pix names them.
const (
	RecurringCreated   = "CRIADA"
	RecurringActive    = "ATIVA"
	RecurringCompleted = "CONCLUIDA"
	RecurringExpired   = "EXPIRADA"
	RecurringRejected  = "REJEITADA"
	RecurringCanceled  = "CANCELADA"

	AttemptRequested = "SOLICITADA"
	AttemptScheduled = "AGENDADA"
)

type RecurringCharge struct {
	TxID     string
	Status   string
	Attempts []RecurringAttempt
	// Payments are the endToEndIds of the Pix that paid it.
	Payments []string
	// EndedBy and EndCode say who ended a canceled or rejected charge, and why.
	EndedBy string
	EndCode string
}

type RecurringAttempt struct {
	Date   string
	Kind   string
	Status string
	E2EID  string
}

// Pending reports whether an attempt is still to be debited.
func (c RecurringCharge) Pending() bool {
	for _, a := range c.Attempts {
		if a.Status == AttemptRequested || a.Status == AttemptScheduled {
			return true
		}
	}
	return false
}

// FirstDate is the date of the charge's first attempt (AGND), from which its retries
// count.
func (c RecurringCharge) FirstDate() string {
	first := ""
	for _, a := range c.Attempts {
		if a.Kind == "AGND" && (first == "" || a.Date < first) {
			first = a.Date
		}
	}
	return first
}

// Retries counts the attempts after the first.
func (c RecurringCharge) Retries() int {
	return max(len(c.Attempts)-1, 0)
}

// RecurringPayment is a charge of a subscription, to be paid by Pix Automático.
type RecurringPayment struct {
	Amount       money.Amount
	Description  string
	Subscription string
	RecurrenceID string
	DueDate      string
}

// CreateRecurringPayment records a subscription's payment intent and its attempt, before
// the bank is asked for the charge (Authorize then does, with FinishAuthorization).
func (s *Service) CreateRecurringPayment(ctx context.Context, tx pgx.Tx, owner Owner, p RecurringPayment) (Intent, error) {
	if p.Amount.Currency() != money.BRL || !p.Amount.IsPositive() {
		return Intent{}, fmt.Errorf("%w: Pix Automático charges are positive amounts in BRL", ErrInvalid)
	}
	if _, err := s.pixRail(owner.Livemode); err != nil {
		return Intent{}, err
	}
	opts := &PixOptions{Recurring: &PixRecurring{Subscription: p.Subscription, RecurrenceID: p.RecurrenceID, DueDate: p.DueDate}}
	now := s.cfg.Now().UTC()
	row := db.PaymentsIntent{
		ID: IntentPrefix.New().String(), MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Amount: p.Amount.Minor(),
		Currency: p.Amount.Currency().Code(), CaptureMethod: string(CaptureAutomatic), Status: string(RequiresConfirmation),
		PaymentMethod: PaymentMethodPixAutomatico, Description: p.Description, RequestThreeDSecure: ThreeDSAutomatic,
		PixOptions: pixOptionsColumn(opts), CreatedAt: ts(now),
	}
	q := db.New(tx)
	if err := q.InsertIntent(ctx, db.InsertIntentParams{
		ID: row.ID, MerchantID: row.MerchantID, Livemode: row.Livemode, Amount: row.Amount, Currency: row.Currency,
		CaptureMethod: row.CaptureMethod, Status: row.Status, PaymentMethod: row.PaymentMethod, Description: row.Description,
		RequestThreeDSecure: row.RequestThreeDSecure, PixOptions: row.PixOptions, CreatedAt: ts(now),
	}); err != nil {
		return Intent{}, fmt.Errorf("creating payment intent: %w", err)
	}
	if err := s.publish(ctx, tx, owner, events.TypePaymentIntentCreated, row.ID); err != nil {
		return Intent{}, err
	}
	attemptID, err := s.newAttempt(ctx, q, row, ConfirmParams{})
	if err != nil {
		return Intent{}, err
	}
	startAttempt(&row, attemptID)
	if err := s.setStatus(ctx, tx, &row, Processing); err != nil {
		return Intent{}, err
	}
	return s.save(ctx, q, row)
}

func (s *Service) chargeRecurring(ctx context.Context, rail PixRail, intent db.PaymentsIntent, attempt db.PaymentsAttempt, r *PixRecurring) (Result, error) {
	amount, err := money.New(attempt.Amount, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	charge, err := rail.CreateRecurringCharge(ctx, RecurringChargeRequest{
		TxID: PixTxID(attempt.ID), RecurrenceID: r.RecurrenceID, DueDate: r.DueDate, Amount: amount, Description: truncate(intent.Description, pixDescriptionMax),
	})
	return recurringChargeResult(charge, err), nil
}

func recurringChargeResult(charge RecurringCharge, err error) Result {
	switch {
	case errors.Is(err, ErrPixRefused):
		return Result{Outcome: Declined, DeclineCode: "pix_charge_refused"}
	case err != nil:
		return Result{Outcome: Unknown}
	case charge.Status == RecurringCreated || charge.Status == RecurringActive || charge.Status == RecurringCompleted:
		return Result{Outcome: ActionRequired, Reference: charge.TxID, Pix: &PixCharge{TxID: charge.TxID, Status: charge.Status, Recurring: true, Payments: charge.Payments}}
	}
	return Result{Outcome: Declined, DeclineCode: "pix_charge_" + lower(charge.Status)}
}

// awaitRecurring records a scheduled recurring charge: the intent stays processing,
// waiting for the payer's bank.
func (s *Service) awaitRecurring(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt, charge PixCharge) error {
	opts, err := pixOptionsOf(*row)
	if err != nil {
		return err
	}
	due, err := time.ParseInLocation(time.DateOnly, opts.Recurring.DueDate, brasilia)
	if err != nil {
		return fmt.Errorf("payments: the due date of %s: %w", row.ID, err)
	}
	if err := db.New(tx).InsertPixCharge(ctx, db.InsertPixChargeParams{
		Txid: charge.TxID, AttemptID: attempt.ID, Livemode: row.Livemode,
		// The last day it can be paid: the due date, a weekend, and seven days of retries.
		ExpiresAt: ts(due.AddDate(0, 0, 10)), CreatedAt: ts(s.cfg.Now().UTC()),
	}); err != nil {
		return err
	}
	attempt.Status, attempt.RailReference, attempt.UnknownSince = string(attemptScheduled), charge.TxID, ts(time.Time{})
	return nil
}

// SyncRecurringPayment reads a subscription payment's charge from the bank and applies
// it: paid, it settles the intent with the Pix that paid it; expired or rejected, it
// fails it; canceled, it cancels it. It returns the charge, for the subscription to
// decide on retries.
// RecurringOutcome is where a subscription payment stands after a sync: its intent's
// status, and the charge as the bank described it, when it was read.
type RecurringOutcome struct {
	Status Status
	Charge RecurringCharge
}

func (s *Service) SyncRecurringPayment(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID) (RecurringOutcome, error) {
	q := db.New(pool)
	intent, attempt, err := s.current(ctx, q, owner, intentID)
	if err != nil {
		return RecurringOutcome{}, err
	}
	if intent.PaymentMethod != PaymentMethodPixAutomatico && attempt.PaymentMethod != PaymentMethodPixAutomatico {
		return RecurringOutcome{}, fmt.Errorf("%w: %s is not a Pix Automático payment", ErrInvalid, intentID)
	}
	var out RecurringOutcome
	// Only a scheduled charge is the bank's to settle; one whose creation is unanswered is
	// the resolver's, and a finished one is finished.
	if inStatus(attempt, attemptScheduled) {
		if out.Charge, err = s.syncCharge(ctx, pool, owner, intentID, intent, attempt); err != nil {
			return RecurringOutcome{}, err
		}
	}
	it, err := s.Intent(ctx, pool, owner, intentID)
	out.Status = it.Status
	return out, err
}

func (s *Service) syncCharge(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (RecurringCharge, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return RecurringCharge{}, err
	}
	charge, err := rail.RecurringCharge(ctx, PixTxID(attempt.ID))
	if err != nil {
		return RecurringCharge{}, err
	}
	switch charge.Status {
	case RecurringCompleted:
		if err := s.receiveChargePayments(ctx, pool, intent.Livemode, PixCharge{TxID: charge.TxID, Payments: charge.Payments}); err != nil {
			return charge, err
		}
		// The Pix settles the attempt, unless it paid something else and was held apart to
		// be returned: then the charge is not paid.
		current, err := db.New(pool).GetAttempt(ctx, attempt.ID)
		if err != nil || !inStatus(current, attemptScheduled) {
			return charge, err
		}
		charge.EndCode = "pix_unmatched"
		err = s.endRecurring(ctx, pool, owner, intentID, attempt.ID, charge, false)
		return charge, err
	case RecurringExpired, RecurringRejected:
		err = s.endRecurring(ctx, pool, owner, intentID, attempt.ID, charge, false)
	case RecurringCanceled:
		err = s.endRecurring(ctx, pool, owner, intentID, attempt.ID, charge, true)
	}
	return charge, err
}

// endRecurring ends a payment whose charge will not be paid: back to
// requires_payment_method with why, or canceled when the charge was.
func (s *Service) endRecurring(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, attemptID string, charge RecurringCharge, canceled bool) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
		if err != nil || attempt.ID != attemptID || !inStatus(attempt, attemptScheduled, attemptAuthorizing, attemptAuthorizationUnknown) {
			return err
		}
		attempt.Status, attempt.DeclineCode = string(attemptFailed), "pix_charge_"+lower(charge.Status)
		attempt.UpdatedAt = ts(s.cfg.Now().UTC())
		if canceled {
			row.CancellationReason = "charge_canceled"
			err = s.setStatus(ctx, tx, &row, Canceled)
		} else {
			message := "The payer's bank did not debit the Pix Automático charge."
			if charge.EndCode != "" {
				message += " Code " + charge.EndCode + "."
			}
			err = s.fail(ctx, tx, &row, "payment_intent_payment_attempt_failed", attempt.DeclineCode, message)
		}
		if err != nil {
			return err
		}
		if err := saveAttempt(ctx, q, attempt); err != nil {
			return err
		}
		_, err = s.save(ctx, q, row)
		return err
	})
}

// RetryRecurringPayment asks the bank for another attempt at a subscription payment's
// charge, on date.
func (s *Service) RetryRecurringPayment(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID, date string) (RecurringCharge, error) {
	intent, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return RecurringCharge{}, err
	}
	if attempt.PaymentMethod != PaymentMethodPixAutomatico || !inStatus(attempt, attemptScheduled) {
		return RecurringCharge{}, fmt.Errorf("%w: %s is not waiting on a Pix Automático charge", ErrInvalidState, intentID)
	}
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return RecurringCharge{}, err
	}
	return rail.RetryRecurringCharge(ctx, PixTxID(attempt.ID), date)
}

func lower(s string) string { return strings.ToLower(s) }

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
