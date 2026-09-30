package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
)

// ErrClearingMismatch says the network cleared something Jupiter did not capture, or not
// for that amount. Reconciliation (phase 13) works these out; clearing only records them.
var ErrClearingMismatch = errors.New("payments: clearing does not match the capture")

// MarkCleared records that the network cleared an attempt's capture on day. Clearing the
// same capture again is a no-op.
func (s *Service) MarkCleared(ctx context.Context, tx pgx.Tx, attemptID string, amount int64, day time.Time) error {
	q := db.New(tx)
	a, err := q.LockAttempt(ctx, attemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, attemptID)
	}
	if err != nil {
		return err
	}
	switch {
	case a.ClearedOn.Valid && a.AmountCleared.Int64 == amount:
		return nil
	case a.ClearedOn.Valid:
		return fmt.Errorf("%w: %s was cleared for %d, now for %d", ErrClearingMismatch, attemptID, a.AmountCleared.Int64, amount)
	case a.Status != string(attemptCaptured):
		return fmt.Errorf("%w: %s is %s, not captured", ErrClearingMismatch, attemptID, a.Status)
	case a.AmountCaptured != amount:
		return fmt.Errorf("%w: %s captured %d, cleared %d", ErrClearingMismatch, attemptID, a.AmountCaptured, amount)
	}
	a.ClearedOn = pgtype.Date{Time: day, Valid: true}
	a.AmountCleared = pgtype.Int8{Int64: amount, Valid: true}
	a.UpdatedAt = ts(s.cfg.Now().UTC())
	return saveAttempt(ctx, q, a)
}

// MarkRefundCleared records that the network cleared a refund on day.
func (s *Service) MarkRefundCleared(ctx context.Context, tx pgx.Tx, refundID string, amount int64, day time.Time) error {
	q := db.New(tx)
	n, err := q.ClearRefund(ctx, db.ClearRefundParams{ID: refundID, Amount: amount, ClearedOn: pgtype.Date{Time: day, Valid: true}})
	if err != nil || n == 1 {
		return err
	}
	r, err := q.GetRefundByID(ctx, refundID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return fmt.Errorf("%w: %s", ErrNotFound, refundID)
	case err != nil:
		return err
	case r.ClearedOn.Valid && r.Amount == amount:
		return nil
	}
	return fmt.Errorf("%w: refund %s is %s for %d, cleared for %d", ErrClearingMismatch, refundID, r.Status, r.Amount, amount)
}

// Attempt is what an attempt shows beyond its intent.
type Attempt struct {
	ID                     id.ID
	Status                 string
	Initiator              string
	Installments           *Installments
	NetworkTransactionID   string
	AuthorizationExpiresAt time.Time
	ClearedOn              time.Time
	AmountCleared          int64
}

// LatestAttempt describes an intent's latest attempt.
func (s *Service) LatestAttempt(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID) (Attempt, error) {
	_, a, err := s.current(ctx, db.New(q), owner, intentID)
	if err != nil {
		return Attempt{}, err
	}
	attemptID, err := AttemptPrefix.Parse(a.ID)
	if err != nil {
		return Attempt{}, err
	}
	out := Attempt{
		ID: attemptID, Status: a.Status, Initiator: a.Initiator, NetworkTransactionID: a.NetworkTransactionID,
		AuthorizationExpiresAt: a.AuthorizationExpiresAt.Time, ClearedOn: a.ClearedOn.Time, AmountCleared: a.AmountCleared.Int64,
	}
	if a.Installments.Valid {
		out.Installments = &Installments{Count: int(a.Installments.Int32), FinancedBy: Financing(a.InstallmentsFinancedBy.String)}
	}
	return out, nil
}
