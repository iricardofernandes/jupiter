package payments

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

const resolveBatch = 100

// Resolve drives every rail operation left in flight or unknown towards a final state
// and returns how many it moved. For each, it first asks the rail what became of the
// request; a request the rail never received is sent again with the same idempotency
// key. An authorization that stays unknown past GiveUpAfter is reversed and failed,
// so none stays unknown for longer. Captures, voids and refunds are never abandoned:
// repeating them is safe, and the money must land.
func (s *Service) Resolve(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	before := ts(s.cfg.Now().UTC().Add(-s.cfg.ResolveAfter))
	q := db.New(pool)
	attempts, err := q.AttemptsToResolve(ctx, db.AttemptsToResolveParams{Before: before, MaxCount: resolveBatch})
	if err != nil {
		return 0, fmt.Errorf("finding attempts to resolve: %w", err)
	}
	refunds, err := q.RefundsToResolve(ctx, db.RefundsToResolveParams{Before: before, MaxCount: resolveBatch})
	if err != nil {
		return 0, fmt.Errorf("finding refunds to resolve: %w", err)
	}
	resolved := 0
	var failures []error
	for _, attemptID := range attempts {
		moved, err := s.resolveAttempt(ctx, pool, attemptID)
		if err != nil {
			// Move it to the back of the queue, so one that keeps failing cannot hold up
			// the rest.
			failures = append(failures, fmt.Errorf("resolving %s: %w", attemptID, err),
				postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.countResolution(ctx, db.New(tx), attemptID) }))
		}
		if moved {
			resolved++
		}
	}
	for _, refundID := range refunds {
		moved, err := s.resolveRefund(ctx, pool, refundID)
		if err != nil {
			failures = append(failures, fmt.Errorf("resolving %s: %w", refundID, err),
				db.New(pool).TouchRefund(ctx, db.TouchRefundParams{ID: refundID, UpdatedAt: ts(s.cfg.Now().UTC())}))
		}
		if moved {
			resolved++
		}
	}
	return resolved, errors.Join(failures...)
}

func (s *Service) resolveAttempt(ctx context.Context, pool *pgxpool.Pool, attemptID string) (bool, error) {
	q := db.New(pool)
	attempt, err := q.GetAttempt(ctx, attemptID)
	if err != nil {
		return false, err
	}
	intent, err := q.GetIntentByID(ctx, attempt.IntentID)
	if err != nil {
		return false, err
	}
	owner, err := ownerOf(intent)
	if err != nil {
		return false, err
	}
	intentID, err := IntentPrefix.Parse(intent.ID)
	if err != nil {
		return false, err
	}
	rail, err := s.rail(owner.Livemode)
	if err != nil {
		return false, err
	}
	op, ok := s.pendingOperation(ctx, pool, owner, intentID, intent, attempt)
	if !ok {
		return false, nil
	}
	res := rail.Query(ctx, op.key)
	// A requires_action answer to an attempt already authenticated is the rail's memory of
	// the request before authentication; the authenticated one was never received.
	stale := res.Outcome == ActionRequired && attempt.Authenticated
	if res.Outcome == NotFound || stale {
		if res, err = op.retry(); err != nil {
			return false, err
		}
	}
	if !definitive(res) && op.authorization && s.overdue(attempt) {
		return true, s.giveUp(ctx, pool, owner, intentID, intent, attempt)
	}
	if !definitive(res) {
		res = Result{Outcome: Unknown}
	}
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		if err := op.apply(tx, res); err != nil {
			return err
		}
		return s.countResolution(ctx, db.New(tx), attempt.ID)
	})
	return definitive(res), err
}

// pendingOperation describes the rail operation an attempt is waiting on: the key to
// ask the rail about, how to send it again, and how to apply the answer.
type pendingOperation struct {
	key           string
	authorization bool
	retry         func() (Result, error)
	apply         func(pgx.Tx, Result) error
}

func (s *Service) pendingOperation(ctx context.Context, q db.DBTX, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (pendingOperation, bool) {
	switch attemptStatus(attempt.Status) {
	case attemptAuthorizing, attemptAuthorizationUnknown:
		return pendingOperation{
			key: attempt.ID, authorization: true,
			retry: func() (Result, error) { return s.authorizeOnRail(ctx, q, owner, intent, attempt) },
			apply: func(tx pgx.Tx, r Result) error {
				_, _, err := s.FinishAuthorization(ctx, tx, owner, intentID, r)
				return err
			},
		}, true
	case attemptCapturing, attemptCaptureUnknown:
		return pendingOperation{
			key:   captureKey(attempt.ID),
			retry: func() (Result, error) { return s.captureOnRail(ctx, intent, attempt) },
			apply: func(tx pgx.Tx, r Result) error {
				_, err := s.FinishCapture(ctx, tx, owner, intentID, r)
				return err
			},
		}, true
	case attemptVoiding, attemptVoidUnknown:
		return pendingOperation{
			key:   voidKey(attempt.ID),
			retry: func() (Result, error) { return s.voidOnRail(ctx, intent, attempt) },
			apply: func(tx pgx.Tx, r Result) error {
				_, err := s.FinishCancel(ctx, tx, owner, intentID, r)
				return err
			},
		}, true
	default:
		return pendingOperation{}, false
	}
}

// giveUp reverses an authorization whose outcome stayed unknown too long. The void names
// the authorization's key, so the rail reverses it whether it approved it, is still
// processing it, or has not received it yet; a late approval can then never happen.
// Only once the rail confirms the reversal is the attempt failed, so the customer can
// pay again without being charged twice.
func (s *Service) giveUp(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt) error {
	void, err := s.voidOnRail(ctx, intent, attempt)
	if err != nil {
		return err
	}
	if void.Outcome != Approved {
		return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			return s.countResolution(ctx, db.New(tx), attempt.ID)
		})
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, locked, err := s.lockCurrent(ctx, q, owner, intentID)
		if err != nil {
			return err
		}
		if locked.ID != attempt.ID || !inStatus(locked, attemptAuthorizing, attemptAuthorizationUnknown) {
			return nil
		}
		locked.Status, locked.DeclineCode = string(attemptFailed), "authorization_unresolved"
		locked.UpdatedAt = ts(s.cfg.Now().UTC())
		if err := s.fail(ctx, tx, &row, "authorization_unresolved", "",
			"The card network did not confirm the authorization in time; it was reversed. Try again."); err != nil {
			return err
		}
		if err := saveAttempt(ctx, q, locked); err != nil {
			return err
		}
		_, err = s.save(ctx, q, row)
		return err
	})
}

func (s *Service) resolveRefund(ctx context.Context, pool *pgxpool.Pool, refundID string) (bool, error) {
	q := db.New(pool)
	refund, err := q.GetRefundByID(ctx, refundID)
	if err != nil {
		return false, err
	}
	merchantID, err := id.Parse(refund.MerchantID)
	if err != nil {
		return false, err
	}
	owner := Owner{Merchant: merchantID, Livemode: refund.Livemode}
	rail, err := s.rail(owner.Livemode)
	if err != nil {
		return false, err
	}
	res := rail.Query(ctx, refund.ID)
	if res.Outcome == NotFound {
		if res, err = s.refundOnRail(ctx, pool, refund); err != nil {
			return false, err
		}
	}
	if !definitive(res) {
		res = Result{Outcome: Unknown}
	}
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := s.FinishRefund(ctx, tx, owner, mustRefundID(refund.ID), res)
		return err
	})
	return definitive(res), err
}

// ExpireAuthorizations voids authorizations nobody captured within their validity.
func (s *Service) ExpireAuthorizations(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	due, err := db.New(pool).AttemptsToExpire(ctx, db.AttemptsToExpireParams{Now: ts(s.cfg.Now().UTC()), MaxCount: resolveBatch})
	if err != nil {
		return 0, fmt.Errorf("finding expired authorizations: %w", err)
	}
	expired := 0
	var failures []error
	for _, attemptID := range due {
		if err := s.expire(ctx, pool, attemptID); err != nil {
			failures = append(failures, fmt.Errorf("expiring %s: %w", attemptID, err))
			continue
		}
		expired++
	}
	return expired, errors.Join(failures...)
}

func (s *Service) expire(ctx context.Context, pool *pgxpool.Pool, attemptID string) error {
	attempt, err := db.New(pool).GetAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	intent, err := db.New(pool).GetIntentByID(ctx, attempt.IntentID)
	if err != nil {
		return err
	}
	owner, err := ownerOf(intent)
	if err != nil {
		return err
	}
	intentID, err := IntentPrefix.Parse(intent.ID)
	if err != nil {
		return err
	}
	var step Step
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, step, err = s.StartCancel(ctx, tx, owner, intentID, "expired")
		return err
	})
	if errors.Is(err, ErrInvalidState) {
		return nil // captured or canceled since it was found due
	}
	if err != nil || step != StepVoid {
		return err
	}
	res, err := s.Void(ctx, pool, owner, intentID)
	if err != nil {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := s.FinishCancel(ctx, tx, owner, intentID, res)
		return err
	})
}

func (s *Service) countResolution(ctx context.Context, q *db.Queries, attemptID string) error {
	attempt, err := q.LockAttempt(ctx, attemptID)
	if err != nil {
		return err
	}
	attempt.Resolutions++
	attempt.UpdatedAt = ts(s.cfg.Now().UTC())
	return saveAttempt(ctx, q, attempt)
}

func (s *Service) overdue(attempt db.PaymentsAttempt) bool {
	since := attempt.UnknownSince
	if !since.Valid {
		since = attempt.CreatedAt
	}
	return s.cfg.Now().Sub(since.Time) >= s.cfg.GiveUpAfter
}

func definitive(r Result) bool {
	return r.Outcome == Approved || r.Outcome == Declined || r.Outcome == ActionRequired
}
