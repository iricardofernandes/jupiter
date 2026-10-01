package receivables

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
)

// taken is what a refund takes from one recipient: from its pending balance, and from
// its available one, for amounts Jupiter had already bought or that no unit holds.
type taken struct {
	pending, available int64
}

// Refunded takes a refund back from the payment's recipients, net of the fee returned,
// to the merchant's balance it is refunded from. It reduces the payment's installments
// not yet settled, in proportion to what each still holds. A reduction comes out of what
// the recipient still has pending on the unit; past that, out of what Jupiter bought,
// which the recipient owes back from its available balance. What no installment holds
// any more is taken from the liable recipient's available balance. It answers how much
// came back to the merchant's balance.
func (s *Service) Refunded(ctx context.Context, tx pgx.Tx, r payments.CardRefund) (int64, error) {
	q := db.New(tx)
	lines, err := q.SplitLinesOfIntent(ctx, r.Intent.String())
	if err != nil || len(lines) == 0 {
		return 0, err // a payment captured without receivables
	}
	reduction, err := r.Amount.Sub(r.FeeReturned)
	if err != nil || !reduction.IsPositive() {
		return 0, err
	}
	installments, err := q.InstallmentsOfIntent(ctx, r.Intent.String())
	if err != nil {
		return 0, err
	}
	from := map[string]*taken{}
	covered, err := s.reduceInstallments(ctx, q, installments, reduction, r.Refund, from)
	if err != nil {
		return 0, err
	}
	if uncovered := reduction.Minor() - covered; uncovered > 0 {
		liable := lines[0].RecipientID
		for _, l := range lines {
			if l.Liable {
				liable = l.RecipientID
			}
		}
		if from[liable] == nil {
			from[liable] = &taken{}
		}
		from[liable].available += uncovered
		s.cfg.Logger.WarnContext(ctx, "a refund more than the receivables left, taken from the liable recipient", "refund", r.Refund, "uncovered", uncovered)
	}
	return reduction.Minor(), s.postRefund(ctx, tx, r, reduction, from)
}

// reduceInstallments takes up to reduction from the open installments, in proportion to
// what each holds, noting what each recipient gives up; it answers how much they held.
func (s *Service) reduceInstallments(ctx context.Context, q *db.Queries, installments []db.InstallmentsOfIntentRow, reduction money.Amount, reference string, from map[string]*taken) (int64, error) {
	var open []db.InstallmentsOfIntentRow
	var weights []int64
	var total int64
	for _, inst := range installments {
		if left := inst.Net - inst.Reduced; !inst.UnitSettled && left > 0 {
			open = append(open, inst)
			weights = append(weights, left)
			total += left
		}
	}
	covered := min(reduction.Minor(), total)
	if covered == 0 {
		return 0, nil
	}
	amount, _ := money.New(covered, reduction.Currency())
	shares, err := amount.Allocate(weights...)
	if err != nil {
		return 0, err
	}
	for i, inst := range open {
		x := shares[i].Minor()
		if x == 0 {
			continue
		}
		u, err := q.GetUnit(ctx, inst.UnitID) // locked with the installments
		if err != nil {
			return 0, err
		}
		fromPending := min(x, u.Value-u.Anticipated)
		if from[inst.RecipientID] == nil {
			from[inst.RecipientID] = &taken{}
		}
		from[inst.RecipientID].pending += fromPending
		from[inst.RecipientID].available += x - fromPending
		if err := q.ReduceInstallment(ctx, db.ReduceInstallmentParams{AttemptID: inst.AttemptID, RecipientID: inst.RecipientID, Number: inst.Number, Amount: x}); err != nil {
			return 0, err
		}
		// The registry must have the reduction by the business day after it.
		if err := s.change(ctx, q, inst.UnitID, -x, -(x - fromPending), "reduced", reference, dayOf(s.cfg.Now())); err != nil {
			return 0, err
		}
	}
	return covered, nil
}

// postRefund moves what each recipient gives up back to the merchant's balance.
func (s *Service) postRefund(ctx context.Context, tx pgx.Tx, r payments.CardRefund, reduction money.Amount, from map[string]*taken) error {
	q := db.New(tx)
	currency := reduction.Currency()
	legs := []ledger.Leg{ledger.Credit(r.Balance, reduction)}
	for recipientID, t := range from {
		for _, part := range []struct {
			bucket string
			amount int64
		}{{Pending, t.pending}, {Available, t.available}} {
			if part.amount == 0 {
				continue
			}
			accountID, err := s.account(ctx, tx, recipientID, r.Owner.Livemode, currency, part.bucket)
			if err != nil {
				return err
			}
			amount, _ := money.New(part.amount, currency)
			legs = append(legs, ledger.Debit(accountID, amount))
			if err := s.movement(ctx, q, recipientID, r.Owner.Livemode, currency, part.bucket, "refund", -part.amount, r.Refund); err != nil {
				return err
			}
		}
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: "split back for " + r.Refund, Legs: legs}); err != nil {
		return fmt.Errorf("posting the refund's split: %w", err)
	}
	return nil
}
