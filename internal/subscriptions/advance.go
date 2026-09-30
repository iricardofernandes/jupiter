package subscriptions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/subscriptions/db"
)

// How cycles are charged: each charge is sent to the bank chargeAhead days before its
// due date, inside the window in which the bank sends it to the customer's bank (from 10
// days before); one that would be sent later than minimumAhead days before is postponed,
// as the Manual de Padrões allows (Anexo IV §4.3.2).
const (
	chargeAhead  = 8
	minimumAhead = 3
	maxRetries   = 3
	retryWindow  = 7
	advanceBatch = 100
	// advanceEvery is how long a subscription waits between the worker's looks at it;
	// notifications bring it forward.
	advanceEvery = time.Hour
)

// Advance looks at the subscriptions due a look: it finishes their setup at the bank,
// reads their recurrence, charges the cycles coming due, and follows the charges in
// flight, asking for retries where allowed. It returns how many it looked at.
func (s *Service) Advance(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	due, err := db.New(pool).SubscriptionsToAdvance(ctx, db.SubscriptionsToAdvanceParams{
		Before: ts(s.cfg.Now().UTC().Add(-advanceEvery)), MaxCount: advanceBatch,
	})
	if err != nil {
		return 0, fmt.Errorf("finding subscriptions to advance: %w", err)
	}
	var failures []error
	for _, subscriptionID := range due {
		if err := s.advance(ctx, pool, subscriptionID); err != nil {
			failures = append(failures, fmt.Errorf("advancing %s: %w", subscriptionID, err))
		}
		if err := db.New(pool).TouchSubscription(ctx, db.TouchSubscriptionParams{ID: subscriptionID, UpdatedAt: ts(s.cfg.Now().UTC())}); err != nil {
			failures = append(failures, err)
		}
	}
	return len(due), errors.Join(failures...)
}

// SyncRecurrence brings forward the subscription a recurrence belongs to: the bank said
// something changed, in the recurrence or a charge under it.
func (s *Service) SyncRecurrence(ctx context.Context, pool *pgxpool.Pool, livemode bool, recurrenceID string) error {
	row, err := db.New(pool).SubscriptionByRecurrence(ctx, db.SubscriptionByRecurrenceParams{RecurrenceID: recurrenceID, Livemode: livemode})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not Jupiter's, or not recorded yet: the setup will find it
	}
	if err != nil {
		return err
	}
	return s.advance(ctx, pool, row.ID)
}

// exclusively runs f while holding the subscription's lock, on a connection of its own
// for as long as f talks to the bank. Someone else holding it is doing the same work:
// f is skipped.
func (s *Service) exclusively(ctx context.Context, pool *pgxpool.Pool, subscriptionID string, f func() error) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	q := db.New(conn)
	locked, err := q.TryLockSubscription(ctx, subscriptionID)
	if err != nil || !locked {
		return err
	}
	defer func() { _ = q.UnlockSubscription(context.WithoutCancel(ctx), subscriptionID) }()
	return f()
}

func (s *Service) advance(ctx context.Context, pool *pgxpool.Pool, subscriptionID string) error {
	return s.exclusively(ctx, pool, subscriptionID, func() error { return s.advanceLocked(ctx, pool, subscriptionID) })
}

func (s *Service) advanceLocked(ctx context.Context, pool *pgxpool.Pool, subscriptionID string) error {
	row, err := db.New(pool).GetSubscriptionByID(ctx, subscriptionID)
	if err != nil {
		return err
	}
	owner, err := ownerOf(row)
	if err != nil {
		return err
	}
	subID, err := Prefix.Parse(row.ID)
	if err != nil {
		return err
	}
	if !row.SetUp {
		if Status(row.Status) != Incomplete {
			return nil // rejected by the bank before it made anything
		}
		rec, err := s.setUp(ctx, pool, owner, subID)
		if err != nil {
			return err
		}
		return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, err := s.FinishSetUp(ctx, tx, owner, subID, rec)
			return err
		})
	}
	bank, err := s.bank(row.Livemode)
	if err != nil {
		return err
	}
	rec, err := bank.Recurrence(ctx, row.RecurrenceID)
	if err != nil {
		return err
	}
	var sub Subscription
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		sub, err = s.ApplyRecurrence(ctx, tx, owner, subID, rec)
		return err
	})
	if err != nil {
		return err
	}
	var failures []error
	for _, c := range sub.Cycles {
		if c.Status == CyclePending {
			failures = append(failures, s.followCycle(ctx, pool, sub, c))
		}
	}
	if sub.Status == Active || sub.Status == PastDue {
		failures = append(failures, s.chargeDueCycles(ctx, pool, sub))
	}
	failures = append(failures, s.settleStatus(ctx, pool, owner, subID))
	return errors.Join(failures...)
}

// chargeDueCycles charges the next cycle once its charge is due to be sent.
func (s *Service) chargeDueCycles(ctx context.Context, pool *pgxpool.Pool, sub Subscription) error {
	today := s.today()
	number := len(sub.Cycles)
	start, _ := time.Parse(time.DateOnly, sub.StartDate)
	due := cycleStart(start, sub.Interval, number)
	switch {
	case sub.EndDate != "" && due.Format(time.DateOnly) > sub.EndDate:
		return nil
	case today.Before(due.AddDate(0, 0, -chargeAhead)):
		return nil
	case due.Before(today.AddDate(0, 0, minimumAhead)):
		// Authorized late: the charge is due as soon as it can be, within its cycle and
		// the subscription.
		due = today.AddDate(0, 0, minimumAhead)
		if !due.Before(cycleStart(start, sub.Interval, number+1)) || (sub.EndDate != "" && due.Format(time.DateOnly) > sub.EndDate) {
			return s.skipCycle(ctx, pool, sub, number)
		}
	}
	var intentID id.ID
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockSubscription(ctx, sub.ID.String()); err != nil {
			return err
		}
		cycles, err := q.Cycles(ctx, sub.ID.String())
		if err != nil || len(cycles) != number {
			return err // charged meanwhile
		}
		intent, err := s.cfg.Payments.CreateRecurringPayment(ctx, tx, sub.Owner, payments.RecurringPayment{
			Amount: sub.Amount, Description: sub.Description, Subscription: sub.ID.String(), RecurrenceID: sub.RecurrenceID,
			DueDate: due.Format(time.DateOnly),
		})
		if err != nil {
			return err
		}
		intentID = intent.ID
		return q.InsertCycle(ctx, db.InsertCycleParams{
			SubscriptionID: sub.ID.String(), Number: int32(number), DueDate: date(due.Format(time.DateOnly)), //nolint:gosec // cycles of a subscription
			PaymentIntent: intent.ID.String(), Now: ts(s.cfg.Now().UTC()),
		})
	})
	if err != nil || intentID.IsZero() {
		return err
	}
	return s.sendCharge(ctx, pool, sub.Owner, intentID)
}

// skipCycle records a cycle authorized too late to be charged within itself.
func (s *Service) skipCycle(ctx context.Context, pool *pgxpool.Pool, sub Subscription, number int) error {
	s.cfg.Logger.WarnContext(ctx, "a cycle authorized too late to charge", "subscription", sub.ID, "cycle", number)
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if _, err := q.LockSubscription(ctx, sub.ID.String()); err != nil {
			return err
		}
		cycles, err := q.Cycles(ctx, sub.ID.String())
		if err != nil || len(cycles) != number {
			return err // recorded meanwhile
		}
		start, _ := time.Parse(time.DateOnly, sub.StartDate)
		if err := q.InsertCycle(ctx, db.InsertCycleParams{
			SubscriptionID: sub.ID.String(), Number: int32(number), //nolint:gosec // cycles of a subscription
			DueDate: date(cycleStart(start, sub.Interval, number).Format(time.DateOnly)), Now: ts(s.cfg.Now().UTC()),
		}); err != nil {
			return err
		}
		return q.SetCycleStatus(ctx, db.SetCycleStatusParams{SubscriptionID: sub.ID.String(), Number: int32(number), Status: string(CycleCanceled), UpdatedAt: ts(s.cfg.Now().UTC())}) //nolint:gosec // cycles of a subscription
	})
}

// sendCharge asks the bank for a cycle's charge, as confirming a payment does.
func (s *Service) sendCharge(ctx context.Context, pool *pgxpool.Pool, owner Owner, intentID id.ID) error {
	res, err := s.cfg.Payments.Authorize(ctx, pool, owner, intentID)
	if err != nil {
		return err
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, _, err := s.cfg.Payments.FinishAuthorization(ctx, tx, owner, intentID, res)
		return err
	})
}

// followCycle brings a cycle up to date: payments reads its charge from the bank and
// applies it, and the cycle ends as its payment intent did, paid, failed or canceled. A
// charge the customer's bank could not debit is retried when the subscription allows.
func (s *Service) followCycle(ctx context.Context, pool *pgxpool.Pool, sub Subscription, c Cycle) error {
	intentID, err := payments.IntentPrefix.Parse(c.PaymentIntent)
	if err != nil {
		return err
	}
	out, err := s.cfg.Payments.SyncRecurringPayment(ctx, pool, sub.Owner, intentID)
	if err != nil {
		return err
	}
	var status CycleStatus
	switch out.Status {
	case payments.Succeeded:
		status = CyclePaid
	case payments.RequiresPaymentMethod:
		status = CycleFailed
	case payments.Canceled:
		status = CycleCanceled
	default:
		charge := out.Charge
		if charge.Status == payments.RecurringActive && sub.Retries && !charge.Pending() && charge.Retries() < maxRetries {
			return s.retry(ctx, pool, sub, intentID, charge)
		}
		return nil
	}
	return db.New(pool).SetCycleStatus(ctx, db.SetCycleStatusParams{
		SubscriptionID: sub.ID.String(), Number: int32(c.Number), Status: string(status), UpdatedAt: ts(s.cfg.Now().UTC()), //nolint:gosec // cycles of a subscription
	})
}

// retry asks for the next attempt, tomorrow: the earliest the customer's bank can take
// it, within seven days of the first.
func (s *Service) retry(ctx context.Context, pool *pgxpool.Pool, sub Subscription, intentID id.ID, charge payments.RecurringCharge) error {
	first, err := time.Parse(time.DateOnly, charge.FirstDate())
	if err != nil {
		return nil //nolint:nilerr // no first attempt yet: nothing to retry
	}
	tomorrow := s.today().AddDate(0, 0, 1)
	if tomorrow.After(first.AddDate(0, 0, retryWindow)) {
		return nil // the bank expires it
	}
	_, err = s.cfg.Payments.RetryRecurringPayment(ctx, pool, sub.Owner, intentID, tomorrow.Format(time.DateOnly))
	if errors.Is(err, payments.ErrPixRefused) {
		// Another look asked for it first, or the bank will not take more.
		s.cfg.Logger.InfoContext(ctx, "a retry the bank refused", "subscription", sub.ID, "payment_intent", intentID, "error", err)
		return nil
	}
	return err
}

// settleStatus: an active subscription whose latest finished cycle failed is past due,
// and back to active once one is paid.
func (s *Service) settleStatus(ctx context.Context, pool *pgxpool.Pool, owner Owner, subID id.ID) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := lockOwned(ctx, q, owner, subID)
		if err != nil || (Status(row.Status) != Active && Status(row.Status) != PastDue) {
			return err
		}
		cycles, err := q.Cycles(ctx, row.ID)
		if err != nil {
			return err
		}
		want := Active
		for i := len(cycles) - 1; i >= 0; i-- {
			if st := CycleStatus(cycles[i].Status); st == CyclePaid || st == CycleFailed {
				if st == CycleFailed {
					want = PastDue
				}
				break
			}
		}
		if Status(row.Status) == want {
			return nil
		}
		row.Status = string(want)
		if err := s.save(ctx, q, row); err != nil {
			return err
		}
		return s.publish(ctx, tx, owner, events.TypeSubscriptionUpdated, row.ID)
	})
}

// cycleStart is when cycle n of a subscription starts: the start date moved by n
// intervals. A day the month lacks becomes the last it has (Anexo IV §4.3.1: a monthly
// subscription from January 31 is due on February 28, or 29).
func cycleStart(start time.Time, interval Interval, n int) time.Time {
	months := map[Interval]int{Month: 1, Quarter: 3, HalfYear: 6, Year: 12}[interval]
	if months == 0 {
		return start.AddDate(0, 0, 7*n)
	}
	y, m := start.Year(), int(start.Month())-1+months*n
	y, m = y+m/12, m%12
	last := time.Date(y, time.Month(m+2), 0, 0, 0, 0, 0, time.UTC).Day()
	return time.Date(y, time.Month(m+1), min(start.Day(), last), 0, 0, 0, 0, time.UTC)
}
