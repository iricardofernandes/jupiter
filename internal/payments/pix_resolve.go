package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// What becomes of Pix attempts nobody finished: charges whose creation went unanswered,
// cancellations whose removal did, charges that expired unpaid, and Pix that arrived
// without a notification. The bank's word decides each: the charge as GET /cob says, and
// each Pix that paid it as GET /pix/{e2eid} says.

// resolvePix drives a Pix attempt the resolver found in flight.
func (s *Service) resolvePix(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (bool, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return false, err
	}
	opts, err := pixOptionsOf(intent)
	if err != nil {
		return false, err
	}
	due := opts != nil && opts.Due != nil
	switch attemptStatus(attempt.Status) {
	case attemptAuthorizing, attemptAuthorizationUnknown:
		charge, err := rail.Charge(ctx, PixTxID(attempt.ID), due)
		res := pixChargeResult(charge, err)
		if errors.Is(err, ErrPixNotFound) {
			if res, err = s.chargePix(ctx, intent, attempt); err != nil {
				return false, err
			}
		}
		if res.Outcome == Unknown && s.overdue(attempt) {
			return s.abandonPixCharge(ctx, pool, owner, intentID, intent, attempt, due)
		}
		err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, _, err := s.FinishAuthorization(ctx, tx, owner, intentID, res)
			return err
		})
		if err != nil || res.Pix == nil {
			return definitive(res), err
		}
		return true, s.receiveChargePayments(ctx, pool, intent.Livemode, *res.Pix)
	case attemptVoiding, attemptVoidUnknown:
		res, err := s.removePixCharge(ctx, intent, attempt)
		if err != nil {
			return false, err
		}
		if res.Pix != nil && res.Pix.Status == PixChargeCompleted {
			return true, s.receiveChargePayments(ctx, pool, intent.Livemode, *res.Pix)
		}
		err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := s.FinishCancel(ctx, tx, owner, intentID, res)
			return err
		})
		return res.Outcome == Approved, err
	case attemptRequiresAction:
		return s.expirePix(ctx, pool, owner, intentID, intent, attempt, due)
	default:
		return false, nil
	}
}

// abandonPixCharge gives up on a charge whose creation stayed unanswered past
// GiveUpAfter: once it is removed, or the bank shows it never had it, nobody can pay it.
func (s *Service) abandonPixCharge(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt, due bool) (bool, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return false, err
	}
	charge, err := rail.RemoveCharge(ctx, PixTxID(attempt.ID), due)
	switch {
	case err == nil && charge.Status == PixChargeCompleted:
		return true, s.receiveChargePayments(ctx, pool, intent.Livemode, charge)
	case err != nil && !errors.Is(err, ErrPixNotFound):
		return false, postgres.InTx(ctx, pool, func(tx pgx.Tx) error { return s.countResolution(ctx, db.New(tx), attempt.ID) })
	}
	return true, s.failPixAttempt(ctx, pool, owner, intentID, attempt.ID, attemptAuthorizing, "pix_charge_unresolved",
		"payment_intent_payment_attempt_failed", "The bank did not confirm the Pix charge in time; try again.")
}

// expirePix ends an attempt whose charge expired unpaid, after a grace period for Pix
// in flight. A charge that turns out paid settles the attempt instead.
func (s *Service) expirePix(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, intent db.PaymentsIntent, attempt db.PaymentsAttempt, due bool) (bool, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return false, err
	}
	stored, err := db.New(pool).PixChargeByAttempt(ctx, attempt.ID)
	if err != nil {
		return false, err
	}
	charge, err := rail.Charge(ctx, stored.Txid, due)
	if err != nil {
		return false, err
	}
	if charge.Status == PixChargeCompleted {
		if err := s.receiveChargePayments(ctx, pool, intent.Livemode, charge); err != nil {
			return false, err
		}
		// The Pix settled the attempt, unless it went elsewhere, to be returned: then the
		// attempt ends as if expired, not asked about again.
		if current, err := db.New(pool).GetAttempt(ctx, attempt.ID); err != nil || !inStatus(current, attemptRequiresAction) {
			return err == nil, err
		}
	} else if s.cfg.Now().Before(stored.ExpiresAt.Time.Add(pixExpiryGrace)) {
		return false, nil
	}
	return true, s.failPixAttempt(ctx, pool, owner, intentID, attempt.ID, attemptRequiresAction, "pix_expired",
		"payment_intent_payment_attempt_expired", "The Pix charge expired before it was paid.")
}

// failPixAttempt fails an attempt still in status from, and returns its intent to
// requires_payment_method. A Pix that pays its charge after this is returned.
func (s *Service) failPixAttempt(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID, attemptID string, from attemptStatus, declineCode, code, message string) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, attempt, err := s.lockCurrent(ctx, q, owner, intentID)
		if err != nil {
			return err
		}
		stillFrom := inStatus(attempt, from) || (from == attemptAuthorizing && inStatus(attempt, attemptAuthorizationUnknown))
		if attempt.ID != attemptID || !stillFrom {
			return nil
		}
		attempt.Status, attempt.DeclineCode = string(attemptFailed), declineCode
		attempt.UpdatedAt = ts(s.cfg.Now().UTC())
		if err := s.fail(ctx, tx, &row, code, "", message); err != nil {
			return err
		}
		row.NextActionData, row.NextActionExpiresAt = "", pgtype.Timestamptz{}
		if err := saveAttempt(ctx, q, attempt); err != nil {
			return err
		}
		_, err = s.save(ctx, q, row)
		return err
	})
}

// ExpirePixCharges ends the Pix attempts whose charges expired unpaid, and settles those
// whose payment was never notified.
func (s *Service) ExpirePixCharges(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	due, err := db.New(pool).PixAttemptsDue(ctx, db.PixAttemptsDueParams{Before: ts(s.cfg.Now().UTC().Add(-pixExpiryGrace)), MaxCount: resolveBatch})
	if err != nil {
		return 0, fmt.Errorf("finding expired Pix charges: %w", err)
	}
	moved := 0
	var failures []error
	for _, attemptID := range due {
		ok, err := s.resolveAttempt(ctx, pool, attemptID)
		if err != nil {
			failures = append(failures, fmt.Errorf("expiring %s: %w", attemptID, err))
		}
		if ok {
			moved++
		}
	}
	return moved, errors.Join(failures...)
}

func (s *Service) removePixCharge(ctx context.Context, intent db.PaymentsIntent, attempt db.PaymentsAttempt) (Result, error) {
	rail, err := s.pixRail(intent.Livemode)
	if err != nil {
		return Result{}, err
	}
	opts, err := pixOptionsOf(intent)
	if err != nil {
		return Result{}, err
	}
	charge, err := rail.RemoveCharge(ctx, PixTxID(attempt.ID), opts != nil && opts.Due != nil)
	return removalResult(charge, err), nil
}

// removalResult is what removing a charge came to: removed, or never created, approves
// the cancellation; paid refuses it, with the charge to settle the attempt from.
func removalResult(charge PixCharge, err error) Result {
	switch {
	case errors.Is(err, ErrPixNotFound):
		return Result{Outcome: Approved}
	case err != nil:
		return Result{Outcome: Unknown}
	case charge.Status == PixChargeCompleted:
		return Result{Outcome: Declined, DeclineCode: "pix_already_paid", Pix: &charge}
	}
	return Result{Outcome: Approved, Reference: charge.TxID}
}

// receiveChargePayments applies the Pix that paid a charge, each as the bank describes
// it when asked by its endToEndId.
func (s *Service) receiveChargePayments(ctx context.Context, pool *pgxpool.Pool, livemode bool, charge PixCharge) error {
	rail, err := s.pixRail(livemode)
	if err != nil {
		return err
	}
	for _, e2eID := range charge.Payments {
		p, err := rail.Payment(ctx, e2eID)
		if err != nil {
			return fmt.Errorf("reading Pix %s: %w", e2eID, err)
		}
		if err := s.ReceivePix(ctx, pool, livemode, p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) returnPix(ctx context.Context, livemode bool, e2eID, returnID string, amount money.Amount) (Result, error) {
	rail, err := s.pixRail(livemode)
	if err != nil {
		return Result{}, err
	}
	ret, err := rail.Return(ctx, PixReturnRequest{E2EID: e2eID, ID: returnID, Amount: amount})
	return pixReturnResult(ret, err), nil
}

func (s *Service) queryPixReturn(ctx context.Context, livemode bool, e2eID, returnID string) (Result, error) {
	rail, err := s.pixRail(livemode)
	if err != nil {
		return Result{}, err
	}
	ret, err := rail.ReturnStatus(ctx, e2eID, returnID)
	return pixReturnResult(ret, err), nil
}

func pixReturnResult(ret PixReturn, err error) Result {
	switch {
	case errors.Is(err, ErrPixNotFound):
		return Result{Outcome: NotFound}
	case errors.Is(err, ErrPixRefused):
		return Result{Outcome: Declined, DeclineCode: "pix_return_refused"}
	case err != nil:
		return Result{Outcome: Unknown}
	}
	switch ret.Status {
	case PixReturnReturned:
		return Result{Outcome: Approved, Reference: ret.ID}
	case PixReturnFailed:
		return Result{Outcome: Declined, DeclineCode: "pix_return_failed", Reference: ret.ID}
	}
	return Result{Outcome: Pending, Reference: ret.ID}
}

// ApplyPixReturn applies how the bank says a return ended: a refund's, or that of a Pix
// that paid nothing.
func (s *Service) ApplyPixReturn(ctx context.Context, pool *pgxpool.Pool, livemode bool, e2eID string, r PixReturn) error {
	res := pixReturnResult(r, nil)
	if !definitive(res) {
		return nil
	}
	if r.ID == unmatchedReturnID(e2eID) {
		return s.finishUnmatchedReturn(ctx, pool, livemode, e2eID, res)
	}
	rest, ok := strings.CutPrefix(r.ID, "re")
	if !ok {
		return nil // not a return Jupiter asked for
	}
	refundID, err := RefundPrefix.Parse(string(RefundPrefix) + "_" + rest)
	if err != nil {
		return nil //nolint:nilerr // an id that is not a refund's is not Jupiter's to apply
	}
	q := db.New(pool)
	refund, err := q.GetRefundByID(ctx, refundID.String())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && refund.Livemode != livemode) {
		return nil
	}
	if err != nil {
		return err
	}
	// A return counts only under the Pix it returns.
	if attempt, err := q.GetAttempt(ctx, refund.AttemptID); err != nil || attempt.NetworkTransactionID != e2eID {
		return err
	}
	merchant, err := id.Parse(refund.MerchantID)
	if err != nil {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, err := s.FinishRefund(ctx, tx, Owner{Merchant: merchant, Livemode: livemode}, refundID, res)
		return err
	})
}

// ReturnUnmatchedPix returns to their payers the Pix that paid nothing Jupiter wanted.
func (s *Service) ReturnUnmatchedPix(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	rows, err := db.New(pool).PixReceivedToReturn(ctx, db.PixReceivedToReturnParams{
		Before: ts(s.cfg.Now().UTC().Add(-s.cfg.ResolveAfter)), MaxCount: resolveBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("finding Pix to return: %w", err)
	}
	returned := 0
	var failures []error
	for _, row := range rows {
		ok, err := s.returnUnmatched(ctx, pool, row.Livemode, row.E2eID)
		if err != nil {
			failures = append(failures, fmt.Errorf("returning Pix %s: %w", row.E2eID, err))
		}
		if ok {
			returned++
		}
	}
	return returned, errors.Join(failures...)
}

func (s *Service) returnUnmatched(ctx context.Context, pool *pgxpool.Pool, livemode bool, e2eID string) (bool, error) {
	var amount money.Amount
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		rec, err := q.LockPixReceived(ctx, db.LockPixReceivedParams{Livemode: livemode, E2eID: e2eID})
		if err != nil {
			return err
		}
		if rec.Status != "unmatched" && rec.Status != "returning" {
			return errFinished // returned since it was listed
		}
		if amount, err = money.New(rec.Amount, mustCurrency(rec.Currency)); err != nil {
			return err
		}
		rec.Status, rec.UpdatedAt = "returning", ts(s.cfg.Now().UTC())
		return savePixReceived(ctx, q, rec)
	})
	if errors.Is(err, errFinished) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	res, err := s.queryPixReturn(ctx, livemode, e2eID, unmatchedReturnID(e2eID))
	if err != nil {
		return false, err
	}
	if res.Outcome == NotFound {
		if res, err = s.returnPix(ctx, livemode, e2eID, unmatchedReturnID(e2eID), amount); err != nil {
			return false, err
		}
	}
	if !definitive(res) {
		return false, nil
	}
	return true, s.finishUnmatchedReturn(ctx, pool, livemode, e2eID, res)
}

// finishUnmatchedReturn moves a returned Pix out of the held account, or marks a return
// that failed for someone to look at: the money stays held.
func (s *Service) finishUnmatchedReturn(ctx context.Context, pool *pgxpool.Pool, livemode bool, e2eID string, res Result) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		rec, err := q.LockPixReceived(ctx, db.LockPixReceivedParams{Livemode: livemode, E2eID: e2eID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || (rec.Status != "unmatched" && rec.Status != "returning") {
			return err
		}
		rec.UpdatedAt = ts(s.cfg.Now().UTC())
		if res.Outcome != Approved {
			rec.Status, rec.Reason = "return_failed", rec.Reason+"; the return failed: "+res.DeclineCode
			return savePixReceived(ctx, q, rec)
		}
		amount, err := money.New(rec.Amount, mustCurrency(rec.Currency))
		if err != nil {
			return err
		}
		accts, err := s.pixAccounts(ctx, tx, livemode, amount.Currency())
		if err != nil {
			return err
		}
		txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
			Description: "Pix " + e2eID + " returned",
			Legs:        []ledger.Leg{ledger.Debit(accts.unmatched, amount), ledger.Credit(accts.settlement, amount)},
		})
		if err != nil {
			return fmt.Errorf("posting a returned Pix: %w", err)
		}
		rec.Status, rec.ReturnTxn = "returned", text(txn.ID.String())
		return savePixReceived(ctx, q, rec)
	})
}

var errFinished = errors.New("payments: already finished")

func savePixReceived(ctx context.Context, q *db.Queries, rec db.PaymentsPixReceived) error {
	return q.SavePixReceived(ctx, db.SavePixReceivedParams{
		Livemode: rec.Livemode, E2eID: rec.E2eID, Status: rec.Status, AttemptID: rec.AttemptID, LedgerTxn: rec.LedgerTxn,
		ReturnTxn: rec.ReturnTxn, Reason: rec.Reason, UpdatedAt: rec.UpdatedAt,
	})
}
