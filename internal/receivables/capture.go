package receivables

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/receivables/rules"
	"github.com/iricardofernandes/jupiter/pkg/bizday"
)

var _ payments.Receivables = (*Service)(nil)

// Captured constitutes a card payment's receivables: one installment per settlement
// date, each the same share of the amount and of the fee (the earliest taking the
// centavos left over), added to the merchant's unit for its arrangement and date.
func (s *Service) Captured(ctx context.Context, tx pgx.Tx, c payments.CardCapture) error {
	arrangement := arrangements[c.Scheme]
	if arrangement == "" {
		s.cfg.Logger.WarnContext(ctx, "no arrangement for the scheme: its receivables are not kept", "scheme", c.Scheme, "attempt", c.Attempt)
		return nil
	}
	plan, err := planOf(c)
	if err != nil {
		return err
	}
	q := db.New(tx)
	for i, inst := range plan {
		u, err := s.unitFor(ctx, q, c.Owner, arrangement, inst.date, c.Amount.Currency(), dayOf(c.At))
		if err != nil {
			return err
		}
		net := inst.gross - inst.fee
		if err := q.InsertInstallment(ctx, db.InsertInstallmentParams{
			AttemptID: c.Attempt, Number: int32(i + 1), PaymentIntent: c.Intent.String(), UnitID: u.ID,
			Gross: inst.gross, Fee: inst.fee, Net: net,
		}); err != nil {
			return fmt.Errorf("recording installment %d of %s: %w", i+1, c.Attempt, err)
		}
		if err := s.change(ctx, q, u.ID, net, "constituted", c.Attempt, dayOf(c.At)); err != nil {
			return err
		}
	}
	return nil
}

type installment struct {
	date       time.Time
	gross, fee int64
}

// planOf divides a capture into its installments. Installments financed by the issuer
// are paid to the merchant in one go.
func planOf(c payments.CardCapture) ([]installment, error) {
	count, financedBy := 1, ""
	if c.Installments != nil {
		financedBy = string(c.Installments.FinancedBy)
		if c.Installments.FinancedBy == payments.FinancedByMerchant {
			count = c.Installments.Count
		}
	}
	term, ok := rules.TermFor(c.Scheme, financedBy, c.At)
	if !ok {
		return nil, fmt.Errorf("receivables: no settlement term for %s financed by %q", c.Scheme, financedBy)
	}
	weights := slices.Repeat([]int64{1}, count)
	gross, err := c.Amount.Allocate(weights...)
	if err != nil {
		return nil, err
	}
	fees, err := c.Fee.Allocate(weights...)
	if err != nil {
		return nil, err
	}
	captured := dayOf(c.At)
	out := make([]installment, count)
	for i := range out {
		date := bizday.Next(captured.AddDate(0, 0, term.FirstDays+i*term.IntervalDays))
		out[i] = installment{date: date, gross: gross[i].Minor(), fee: fees[i].Minor()}
	}
	return out, nil
}

// unitFor finds or creates the merchant's unit for an arrangement and date, locked.
func (s *Service) unitFor(ctx context.Context, q *db.Queries, owner payments.Owner, arrangement string, date time.Time, currency money.Currency, constituted time.Time) (db.ReceivablesUnit, error) {
	key := db.LockUnitParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Arrangement: arrangement, SettlementDate: dateOf(date)}
	if err := q.InsertUnit(ctx, db.InsertUnitParams{
		ID: UnitPrefix.New().String(), MerchantID: key.MerchantID, Livemode: key.Livemode, Arrangement: arrangement,
		SettlementDate: key.SettlementDate, Currency: currency.Code(), ConstitutedOn: dateOf(constituted), Now: ts(s.cfg.Now()),
	}); err != nil {
		return db.ReceivablesUnit{}, err
	}
	u, err := q.LockUnit(ctx, key)
	if err != nil {
		return db.ReceivablesUnit{}, err
	}
	if u.SettledOn.Valid {
		return db.ReceivablesUnit{}, fmt.Errorf("receivables: unit %s settled already", u.ID)
	}
	return u, nil
}

// change adds delta to a unit's value, as a new version, and records why.
func (s *Service) change(ctx context.Context, q *db.Queries, unitID string, delta int64, kind, reference string, constituted time.Time) error {
	now := s.cfg.Now()
	if err := q.ChangeUnitValue(ctx, db.ChangeUnitValueParams{ID: unitID, Delta: delta, ConstitutedOn: dateOf(constituted), Now: ts(now)}); err != nil {
		return fmt.Errorf("changing unit %s: %w", unitID, err)
	}
	return q.InsertUnitEvent(ctx, db.InsertUnitEventParams{UnitID: unitID, At: ts(now), Kind: kind, Amount: delta, Reference: reference})
}

// Refunded reduces a payment's receivables by what the merchant gives back net of the
// fee returned: across its installments not yet settled, in proportion to what each
// still holds. What they cannot cover is recorded as uncovered, for disputes and
// reserves to recover.
func (s *Service) Refunded(ctx context.Context, tx pgx.Tx, r payments.CardRefund) error {
	reduction, err := r.Amount.Sub(r.FeeReturned)
	if err != nil {
		return err
	}
	q := db.New(tx)
	installments, err := q.InstallmentsOfIntent(ctx, r.Intent.String())
	if err != nil || len(installments) == 0 {
		return err // a payment captured without receivables
	}
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
	if covered > 0 {
		amount, err := money.New(covered, reduction.Currency())
		if err != nil {
			return err
		}
		shares, err := amount.Allocate(weights...)
		if err != nil {
			return err
		}
		for i, inst := range open {
			if err := s.reduce(ctx, q, inst, shares[i].Minor(), r.Refund); err != nil {
				return err
			}
		}
	}
	if uncovered := reduction.Minor() - covered; uncovered > 0 {
		last := installments[len(installments)-1]
		s.cfg.Logger.WarnContext(ctx, "a refund more than the receivables left", "refund", r.Refund, "uncovered", uncovered)
		return q.InsertUnitEvent(ctx, db.InsertUnitEventParams{UnitID: last.UnitID, At: ts(s.cfg.Now()), Kind: "uncovered", Amount: -uncovered, Reference: r.Refund})
	}
	return nil
}

func (s *Service) reduce(ctx context.Context, q *db.Queries, inst db.InstallmentsOfIntentRow, amount int64, reference string) error {
	if amount == 0 {
		return nil
	}
	if err := q.ReduceInstallment(ctx, db.ReduceInstallmentParams{AttemptID: inst.AttemptID, Number: inst.Number, Amount: amount}); err != nil {
		return err
	}
	// The registry must have the reduction by the business day after it.
	return s.change(ctx, q, inst.UnitID, -amount, "reduced", reference, dayOf(s.cfg.Now()))
}

func dateOf(t time.Time) pgtype.Date {
	return pgtype.Date{Time: time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
}

func ts(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t.UTC(), Valid: !t.IsZero()}
}

func notFoundOr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return err
}
