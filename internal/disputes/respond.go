package disputes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/rules"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// What the merchant does with a dispute: save evidence and submit it, or accept it. A
// submission is what Jupiter sends the network (a representment, or an escalation to
// arbitration), what an operator weighs for a MED claim, or a contestation of a MED
// return. What is to be sent is recorded with the change and sent after it commits.

// Update saves evidence on a dispute the merchant can still answer, and submits it if
// submit is set.
func (s *Service) Update(ctx context.Context, tx pgx.Tx, owner payments.Owner, disputeID id.ID, evidence Evidence, submit bool) (Dispute, error) {
	if err := evidence.valid(); err != nil {
		return Dispute{}, err
	}
	q := db.New(tx)
	row, err := s.lockOwn(ctx, q, owner, disputeID)
	if err != nil {
		return Dispute{}, err
	}
	contesting := s.canContest(row)
	if row.Status != NeedsResponse && !contesting {
		return Dispute{}, fmt.Errorf("%w: the dispute is %s and takes no evidence", ErrInvalidState, row.Status)
	}
	var saved Evidence
	if err := json.Unmarshal(row.Evidence, &saved); err != nil {
		return Dispute{}, err
	}
	saved = saved.merged(evidence)
	if err := saved.valid(); err != nil {
		return Dispute{}, err
	}
	if row.Evidence, err = json.Marshal(saved); err != nil {
		return Dispute{}, err
	}
	kind, detail := "evidence", "evidence saved"
	if submit {
		if saved.empty() {
			return Dispute{}, fmt.Errorf("%w: there is no evidence to submit", ErrInvalid)
		}
		kind, detail = s.submit(&row, contesting)
	}
	if err := s.save(ctx, q, row, kind, detail); err != nil {
		return Dispute{}, err
	}
	if err := s.publish(ctx, tx, row, events.TypeDisputeUpdated); err != nil {
		return Dispute{}, err
	}
	return s.Get(ctx, tx, owner, disputeID)
}

// submit moves a dispute on with its evidence submitted.
func (s *Service) submit(row *db.DisputesDispute, contesting bool) (kind, detail string) {
	row.EvidenceSubmittedAt = ts(s.cfg.Now().UTC())
	switch {
	case contesting:
		row.Stage, row.Status, row.PendingAction = StageMEDContestation, UnderReview, actionContest
		return "contested", "the merchant contests the return"
	case row.Kind == KindMED:
		// An operator weighs it, before the block ends.
		row.Status, row.DueBy = UnderReview, pgtype.Timestamptz{}
		return "submitted", "the merchant answered; an operator decides"
	case row.Stage == StagePreArbitration:
		row.Status, row.DueBy, row.PendingAction = UnderReview, pgtype.Timestamptz{}, actionEscalate
		return "submitted", "escalated to arbitration"
	default:
		row.Status, row.DueBy, row.PendingAction = UnderReview, pgtype.Timestamptz{}, actionRepresent
		return "submitted", "represented to the network"
	}
}

// canContest: a MED claim lost to a return may be contested, once, within the window
// from when it was lost.
func (s *Service) canContest(row db.DisputesDispute) bool {
	if row.Kind != KindMED || row.Status != Lost || row.Stage != StageMEDAnalysis || row.Funds != FundsWithdrawn || !row.ClosedAt.Valid {
		return false
	}
	closed := row.ClosedAt.Time
	return !s.cfg.Now().After(rules.For(rules.Pix, rules.MEDContestation, closed).Due(closed))
}

// Close accepts a dispute the merchant can still answer: lost for a card, whose amount
// was withdrawn already; for a MED claim, the money is returned to the payer.
func (s *Service) Close(ctx context.Context, tx pgx.Tx, owner payments.Owner, disputeID id.ID) (Dispute, error) {
	q := db.New(tx)
	row, err := s.lockOwn(ctx, q, owner, disputeID)
	if err != nil {
		return Dispute{}, err
	}
	if row.Status != NeedsResponse {
		return Dispute{}, fmt.Errorf("%w: the dispute is %s", ErrInvalidState, row.Status)
	}
	if err := s.accept(ctx, tx, &row, "merchant_accepted", "the merchant"); err != nil {
		return Dispute{}, err
	}
	return s.Get(ctx, tx, owner, disputeID)
}

// accept gives up a dispute, for who: Jupiter tells the network it accepts, or the bank
// it agrees.
func (s *Service) accept(ctx context.Context, tx pgx.Tx, row *db.DisputesDispute, outcome, who string) error {
	q := db.New(tx)
	if row.Kind == KindMED {
		row.Status, row.DueBy, row.Outcome, row.PendingAction = UnderReview, pgtype.Timestamptz{}, outcome, actionAgree
		if err := s.save(ctx, q, *row, "agreed", who+" agrees with the claim: "+outcome); err != nil {
			return err
		}
		return s.publish(ctx, tx, *row, events.TypeDisputeUpdated)
	}
	if row.Stage == StagePreArbitration {
		outcome = "pre_arbitration_" + outcome
	}
	row.PendingAction = actionAccept
	s.close(row, Lost, outcome)
	if err := s.save(ctx, q, *row, Lost, who+" accepted: "+outcome); err != nil {
		return err
	}
	return s.publish(ctx, tx, *row, events.TypeDisputeClosed)
}

// Decide is an operator's decision on a MED claim the merchant answered: agreed, and the
// money is returned; or not, and the hold ends. The history names the operator.
func (s *Service) Decide(ctx context.Context, pool *pgxpool.Pool, disputeID id.ID, agree bool, operator string) error {
	if strings.TrimSpace(operator) == "" || len(operator) > 100 {
		return fmt.Errorf("%w: the operator who decides is named", ErrInvalid)
	}
	return postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		row, err := q.LockDispute(ctx, disputeID.String())
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrNotFound, disputeID)
		}
		if err != nil {
			return err
		}
		if row.Kind != KindMED || row.Stage != StageMEDAnalysis || row.Status != UnderReview || row.PendingAction != "" {
			return fmt.Errorf("%w: only a MED claim the merchant answered waits for a decision", ErrInvalidState)
		}
		if agree {
			return s.accept(ctx, tx, &row, "operator_agreed", operator)
		}
		row.Outcome, row.PendingAction = "operator_disagreed", actionDisagree
		return s.save(ctx, q, row, "disagreed", operator+" disagrees with the claim")
	})
}

func (s *Service) lockOwn(ctx context.Context, q *db.Queries, owner payments.Owner, disputeID id.ID) (db.DisputesDispute, error) {
	row, err := q.LockDispute(ctx, disputeID.String())
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && (row.MerchantID != owner.Merchant.String() || row.Livemode != owner.Livemode)) {
		return db.DisputesDispute{}, fmt.Errorf("%w: %s", ErrNotFound, disputeID)
	}
	return row, err
}

// Send sends a dispute's pending action, if it has one, and applies the answer. It runs
// outside any transaction; an action that fails is tried again by Advance.
func (s *Service) Send(ctx context.Context, pool *pgxpool.Pool, disputeID string) error {
	row, err := db.New(pool).GetDisputeByID(ctx, disputeID)
	if err != nil || row.PendingAction == "" {
		return err
	}
	if row.Kind == KindMED {
		err = s.sendMED(ctx, pool, row)
	} else {
		err = s.sendCard(ctx, pool, row)
	}
	if err == nil {
		return nil
	}
	return errors.Join(err, s.retryLater(ctx, pool, row, err))
}
