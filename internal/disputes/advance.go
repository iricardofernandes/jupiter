package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

const (
	// retryAfter spaces the tries of an action the network or bank did not take.
	retryAfter = time.Minute
	// quietFor is how long an open dispute goes without news before Jupiter asks.
	quietFor = time.Hour
)

// Report says what a pass of Advance did.
type Report struct {
	Expired   int // deadlines that moved a dispute on
	Sent      int // actions the network or bank took
	Refreshed int // disputes read back from them
}

// Advance moves disputes on without anyone acting: it answers for merchants whose
// deadline passed (submitting the evidence they saved, or accepting), disagrees with MED
// claims still undecided when their block ends, sends what is pending, and asks the
// network or bank about disputes that are quiet or past their deadline. One dispute that
// fails holds up no other.
func (s *Service) Advance(ctx context.Context, pool *pgxpool.Pool) (Report, error) {
	var r Report
	var failures []error
	now := s.cfg.Now().UTC()
	q := db.New(pool)
	overdue, err := q.OverdueResponses(ctx, ts(now))
	failures = append(failures, err)
	for _, disputeID := range overdue {
		moved, err := s.expire(ctx, pool, disputeID)
		failures = append(failures, err)
		if moved {
			r.Expired++
		}
	}
	lapsed, err := q.LapsedAnalyses(ctx, ts(now))
	failures = append(failures, err)
	for _, disputeID := range lapsed {
		moved, err := s.lapse(ctx, pool, disputeID)
		failures = append(failures, err)
		if moved {
			r.Expired++
		}
	}
	pending, err := q.PendingActions(ctx, ts(now))
	failures = append(failures, err)
	for _, disputeID := range pending {
		if err := s.Send(ctx, pool, disputeID); err != nil {
			failures = append(failures, fmt.Errorf("sending %s: %w", disputeID, err))
			continue
		}
		r.Sent++
	}
	quiet, err := q.ToRefresh(ctx, db.ToRefreshParams{Now: ts(now), QuietSince: ts(now.Add(-quietFor))})
	failures = append(failures, err)
	for _, disputeID := range quiet {
		if err := s.refresh(ctx, pool, disputeID); err != nil {
			failures = append(failures, fmt.Errorf("refreshing %s: %w", disputeID, err))
			continue
		}
		r.Refreshed++
	}
	return r, errors.Join(failures...)
}

// expire answers for a merchant whose deadline passed: a chargeback with the evidence it
// saved, if any; anything else by accepting, as a pre-arbitration left unanswered is.
func (s *Service) expire(ctx context.Context, pool *pgxpool.Pool, disputeID string) (bool, error) {
	moved := false
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockDispute(ctx, disputeID)
		if err != nil || row.Status != NeedsResponse || !row.DueBy.Valid || row.DueBy.Time.After(s.cfg.Now()) {
			return err
		}
		moved = true
		var saved Evidence
		if err := json.Unmarshal(row.Evidence, &saved); err != nil {
			return err
		}
		if row.Kind == KindChargeback && row.Stage == StageChargeback && !saved.empty() {
			_, detail := s.submit(&row, false)
			if err := s.save(ctx, q, row, "deadline", "the merchant's deadline passed: "+detail+" with the evidence saved"); err != nil {
				return err
			}
			return s.publish(ctx, tx, row, events.TypeDisputeUpdated)
		}
		return s.accept(ctx, tx, &row, "deadline", "Jupiter, at the deadline,")
	})
	return moved, err
}

// lapse disagrees with a MED claim no one decided before its block ended; one answered
// already, its return still in processing, is left to finish.
func (s *Service) lapse(ctx context.Context, pool *pgxpool.Pool, disputeID string) (bool, error) {
	moved := false
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockDispute(ctx, disputeID)
		if err != nil || row.Status != UnderReview || row.Stage != StageMEDAnalysis || row.PendingAction != "" || row.Outcome != "" ||
			!row.NetworkDueBy.Valid || row.NetworkDueBy.Time.After(s.cfg.Now()) {
			return err
		}
		moved = true
		row.Outcome, row.PendingAction = "block_lapsed", actionDisagree
		return s.save(ctx, q, row, "deadline", "the block ended with no decision: Jupiter disagrees")
	})
	return moved, err
}

func (s *Service) refresh(ctx context.Context, pool *pgxpool.Pool, disputeID string) error {
	row, err := db.New(pool).GetDisputeByID(ctx, disputeID)
	if err != nil {
		return err
	}
	if row.Kind == KindMED {
		err = s.refreshMED(ctx, pool, row)
	} else {
		err = s.refreshCard(ctx, pool, row)
	}
	if errors.Is(err, ErrNoNetwork) {
		return nil // nothing to ask in this mode
	}
	return err
}

// retryLater records why an action was not taken, and when to try it again.
func (s *Service) retryLater(ctx context.Context, pool *pgxpool.Pool, row db.DisputesDispute, cause error) error {
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		current, err := q.LockDispute(ctx, row.ID)
		if err != nil || current.PendingAction != row.PendingAction {
			return err
		}
		current.ActionError, current.ActionRetryAt = truncate(cause.Error()), ts(s.cfg.Now().Add(retryAfter))
		return s.save(ctx, q, current, "", "")
	})
}
