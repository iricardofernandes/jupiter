package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

const backstop = 24 * time.Hour

// StartCapture records the amount to capture, at most what is capturable, before the
// rail is called. Nil captures everything.
func (s *Service) StartCapture(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, amount *money.Amount) (Intent, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, err
	}
	if Status(row.Status) != RequiresCapture || !inStatus(attempt, attemptAuthorized) {
		return Intent{}, fmt.Errorf("%w: a %s payment intent cannot be captured", ErrInvalidState, row.Status)
	}
	capture := row.AmountCapturable
	if amount != nil {
		if amount.Currency() != mustCurrency(row.Currency) || !amount.IsPositive() {
			return Intent{}, fmt.Errorf("%w: amount_to_capture must be positive and in %s", ErrInvalid, row.Currency)
		}
		if amount.Minor() > row.AmountCapturable {
			return Intent{}, fmt.Errorf("%w: amount_to_capture %d is more than the %d capturable", ErrAmountTooLarge, amount.Minor(), row.AmountCapturable)
		}
		capture = amount.Minor()
	}
	attempt.Status = string(attemptCapturing)
	attempt.CaptureAmount = pgtype.Int8{Int64: capture, Valid: true}
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	row.AmountCapturable = 0
	if err := s.setStatus(ctx, tx, &row, Processing); err != nil {
		return Intent{}, err
	}
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, err
	}
	return s.save(ctx, q, row)
}

func (s *Service) Capture(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (Result, error) {
	intent, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return Result{}, err
	}
	if !inStatus(attempt, attemptCapturing, attemptCaptureUnknown) {
		return Result{}, nil
	}
	return s.captureOnRail(ctx, intent, attempt)
}

func (s *Service) captureOnRail(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.rail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	amount, err := money.New(attempt.CaptureAmount.Int64, mustCurrency(intent.Currency))
	if err != nil {
		return Result{}, err
	}
	return rail.Capture(ctx, OperationRequest{
		Key: captureKey(attempt.ID), AuthorizationKey: attempt.ID, Reference: attempt.RailReference, Amount: amount,
	}), nil
}

// FinishCapture posts the captured amount on the ledger and releases the rest of the hold.
func (s *Service) FinishCapture(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, res Result) (Intent, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil || res.Outcome == "" || !inStatus(attempt, attemptCapturing, attemptCaptureUnknown) {
		it, _, err := s.finish(ctx, q, row, err)
		return it, err
	}
	now := s.cfg.Now().UTC()
	switch res.Outcome {
	case Approved:
		err = s.captured(ctx, tx, &row, &attempt)
	case Declined:
		// The rail refused to capture, so the authorization is gone: release the hold.
		if err = s.releaseHold(ctx, tx, attempt); err == nil {
			attempt.Status, attempt.DeclineCode = string(attemptFailed), res.DeclineCode
			row.CancellationReason = "capture_failed"
			err = s.setStatus(ctx, tx, &row, Canceled)
		}
	default:
		attempt.Status = string(attemptCaptureUnknown)
		if !attempt.UnknownSince.Valid {
			attempt.UnknownSince = ts(now)
		}
	}
	if err != nil {
		return Intent{}, err
	}
	attempt.UpdatedAt = ts(now)
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, err
	}
	return s.save(ctx, q, row)
}

func (s *Service) captured(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, attempt *db.PaymentsAttempt) error {
	holdID, err := ledger.TransactionPrefix.Parse(attempt.LedgerHold.String)
	if err != nil {
		return err
	}
	amount, err := money.New(attempt.CaptureAmount.Int64, mustCurrency(row.Currency))
	if err != nil {
		return err
	}
	if err := s.postCapture(ctx, tx, row, holdID, amount); err != nil {
		return err
	}
	if err := s.chargeFee(ctx, tx, row, attempt, amount); err != nil {
		return err
	}
	attempt.Status = string(attemptCaptured)
	attempt.AmountCaptured = amount.Minor()
	attempt.UnknownSince = pgtype.Timestamptz{}
	row.AmountReceived = amount.Minor()
	row.AmountCapturable = 0
	return s.setStatus(ctx, tx, row, Succeeded)
}

// StartCancel cancels an intent. One with an authorization is moved to processing while
// the rail voids it; StepVoid says so.
func (s *Service) StartCancel(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, reason string) (Intent, Step, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil {
		return Intent{}, StepDone, err
	}
	now := s.cfg.Now().UTC()
	row.CancellationReason = reason
	step := StepDone
	switch Status(row.Status) {
	case RequiresCapture:
		attempt.Status = string(attemptVoiding)
		row.AmountCapturable = 0
		err = s.setStatus(ctx, tx, &row, Processing)
		step = StepVoid
	case RequiresAction:
		if attempt.PaymentMethod == PaymentMethodPix || attempt.PaymentMethod == PaymentMethodBoleto {
			// The charge must be removed at the bank first: until it is, the customer may
			// still pay it.
			attempt.Status = string(attemptVoiding)
			err = s.setStatus(ctx, tx, &row, Processing)
			step = StepVoid
			break
		}
		attempt.Status = string(attemptFailed)
		row.NextAction = ""
		err = s.setStatus(ctx, tx, &row, Canceled)
	case RequiresPaymentMethod, RequiresConfirmation:
		err = s.setStatus(ctx, tx, &row, Canceled)
	default:
		return Intent{}, StepDone, fmt.Errorf("%w: a %s payment intent cannot be canceled", ErrInvalidState, row.Status)
	}
	if err != nil {
		return Intent{}, StepDone, err
	}
	if row.LatestAttempt.Valid {
		attempt.UpdatedAt = ts(now)
		if err := saveAttempt(ctx, q, attempt); err != nil {
			return Intent{}, StepDone, err
		}
	}
	it, err := s.save(ctx, q, row)
	return it, step, err
}

func (s *Service) Void(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (Result, error) {
	intent, attempt, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return Result{}, err
	}
	if !inStatus(attempt, attemptVoiding, attemptVoidUnknown) {
		return Result{}, nil
	}
	return s.voidOnRail(ctx, intent, attempt)
}

func (s *Service) voidOnRail(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	if attempt.PaymentMethod == PaymentMethodPix {
		return s.removePixCharge(ctx, intent, attempt)
	}
	if attempt.PaymentMethod == PaymentMethodBoleto {
		return s.writeOffBoleto(ctx, intent, attempt)
	}
	rail, err := s.rail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	return rail.Void(ctx, OperationRequest{Key: voidKey(attempt.ID), AuthorizationKey: attempt.ID, Reference: attempt.RailReference}), nil
}

// FinishCancel releases the ledger hold once the rail has voided the authorization.
func (s *Service) FinishCancel(ctx context.Context, tx pgx.Tx, owner Owner, intentID id.ID, res Result) (Intent, error) {
	q := db.New(tx)
	row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
	if err != nil || res.Outcome == "" || !inStatus(attempt, attemptVoiding, attemptVoidUnknown) {
		it, _, err := s.finish(ctx, q, row, err)
		return it, err
	}
	now := s.cfg.Now().UTC()
	switch res.Outcome {
	case Approved:
		if attempt.LedgerHold.Valid {
			err = s.releaseHold(ctx, tx, attempt)
		}
		if err == nil {
			attempt.Status = string(attemptVoided)
			attempt.UnknownSince = pgtype.Timestamptz{}
			row.NextAction, row.NextActionData, row.NextActionExpiresAt = "", "", pgtype.Timestamptz{}
			err = s.setStatus(ctx, tx, &row, Canceled)
		}
	default:
		// Including a refusal: the rail only refuses to void an authorization it has
		// captured, which Jupiter never asked for, so the hold stays until someone looks.
		attempt.Status = string(attemptVoidUnknown)
		if !attempt.UnknownSince.Valid {
			attempt.UnknownSince = ts(now)
		}
	}
	if err != nil {
		return Intent{}, err
	}
	attempt.UpdatedAt = ts(now)
	if err := saveAttempt(ctx, q, attempt); err != nil {
		return Intent{}, err
	}
	return s.save(ctx, q, row)
}

// releaseHold voids the attempt's ledger hold. A hold the ledger has already expired is
// released already.
func (s *Service) releaseHold(ctx context.Context, tx pgx.Tx, attempt db.PaymentsAttempt) error {
	holdID, err := ledger.TransactionPrefix.Parse(attempt.LedgerHold.String)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.Void(ctx, tx, holdID); err != nil && !errors.Is(err, ledger.ErrAlreadyResolved) {
		return fmt.Errorf("voiding ledger hold: %w", err)
	}
	return nil
}

// postCapture posts a capture against its hold. A capture whose answer arrived after
// the hold expired or was voided is posted on its own: the rail took the money, so the
// ledger must show it. A hold that was already posted is an error, never a second post.
func (s *Service) postCapture(ctx context.Context, tx pgx.Tx, row *db.PaymentsIntent, holdID id.ID, amount money.Amount) error {
	_, err := s.cfg.Ledger.PostPending(ctx, tx, holdID, amount)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ledger.ErrAlreadyResolved) && !errors.Is(err, ledger.ErrPendingExpired) {
		return fmt.Errorf("posting capture to the ledger: %w", err)
	}
	if errors.Is(err, ledger.ErrAlreadyResolved) {
		resolution, err := s.cfg.Ledger.Resolution(ctx, tx, holdID)
		if err != nil {
			return err
		}
		if resolution.Kind == ledger.KindPostPending {
			return fmt.Errorf("posting capture to the ledger: hold %s was already posted by %s", holdID, resolution.ID)
		}
	} else if err := s.releaseExpired(ctx, tx, holdID); err != nil {
		return err
	}
	owner, err := ownerOf(*row)
	if err != nil {
		return err
	}
	accts, err := s.ledgerAccounts(ctx, tx, owner, amount.Currency())
	if err != nil {
		return err
	}
	_, err = s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "capture after its hold " + holdID.String() + " lapsed",
		Legs:        []ledger.Leg{ledger.Debit(accts.networkReceivable, amount), ledger.Credit(accts.merchantBalance, amount)},
	})
	if err != nil {
		return fmt.Errorf("posting capture to the ledger: %w", err)
	}
	return nil
}

// releaseExpired voids a hold past its expiry that the expiry job has not reached yet.
func (s *Service) releaseExpired(ctx context.Context, tx pgx.Tx, holdID id.ID) error {
	if _, err := s.cfg.Ledger.Void(ctx, tx, holdID); err != nil && !errors.Is(err, ledger.ErrAlreadyResolved) {
		return fmt.Errorf("releasing lapsed hold: %w", err)
	}
	return nil
}
