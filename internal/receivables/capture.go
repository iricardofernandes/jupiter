package receivables

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/receivables/rules"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/pkg/bizday"
)

var _ payments.Receivables = (*Service)(nil)

// Carries refuses a card of a scheme without an arrangement, whose receivables have no
// unit to be kept in, and in live mode a card of a merchant without a CPF or CNPJ, which
// has no recipient to register them under.
func (s *Service) Carries(ctx context.Context, tx pgx.Tx, owner payments.Owner, scheme string) error {
	if arrangements[scheme] == "" {
		return fmt.Errorf("%w: cards of this brand are not accepted", payments.ErrInvalid)
	}
	if !owner.Livemode {
		return nil
	}
	_, err := s.cfg.Recipients.Default(ctx, tx, recipients.Owner{Merchant: owner.Merchant, Livemode: true})
	if errors.Is(err, recipients.ErrInvalid) {
		return fmt.Errorf("%w: a live card payment needs the account's CPF or CNPJ, which its receivables are registered under", payments.ErrInvalid)
	}
	return err
}

// Captured divides a card capture among its recipients, by its split or, with none, all
// to the merchant's own recipient. One ledger transaction moves each recipient's net
// share from the merchant's balance to the recipient's pending balance; the typed lines
// say why. Each share then constitutes the recipient's units: one installment per
// settlement date, each the same part of the share and of its fee, the earliest taking
// the centavos left over. It answers how much left the merchant's balance.
func (s *Service) Captured(ctx context.Context, tx pgx.Tx, c payments.CardCapture) (int64, error) {
	arrangement := arrangements[c.Scheme]
	if arrangement == "" {
		s.cfg.Logger.WarnContext(ctx, "no arrangement for the scheme: its receivables are not kept", "scheme", c.Scheme, "attempt", c.Attempt)
		return 0, nil
	}
	own, err := s.cfg.Recipients.Default(ctx, tx, recipients.Owner{Merchant: c.Owner.Merchant, Livemode: c.Owner.Livemode})
	if errors.Is(err, recipients.ErrInvalid) && len(c.Split) == 0 {
		// A merchant without a CPF or CNPJ has no recipient: its payment stays in its
		// balance, with no receivables.
		s.cfg.Logger.WarnContext(ctx, "a merchant without a tax id: its receivables are not kept", "merchant", c.Owner.Merchant, "attempt", c.Attempt)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	split := c.Split
	if len(split) == 0 {
		split = []payments.SplitRule{{Recipient: own.ID.String(), Percentage: "100.00", Liable: true, Remainder: true, ChargeFee: true}}
	}
	shares, err := divide(c.Amount, c.Fee, split, own.ID.String())
	if err != nil {
		return 0, err
	}
	// Units first, by recipient and date, then the ledger: the order refunds,
	// anticipations and settlements take their locks in.
	ordered := slices.Clone(shares)
	slices.SortFunc(ordered, func(a, b share) int { return strings.Compare(a.recipient, b.recipient) })
	for _, sh := range ordered {
		if err := s.constitute(ctx, tx, c, arrangement, sh); err != nil {
			return 0, err
		}
	}
	return s.postSplit(ctx, tx, c, ordered)
}

// postSplit moves each share's net amount to its recipient's pending balance, and records
// the typed lines.
func (s *Service) postSplit(ctx context.Context, tx pgx.Tx, c payments.CardCapture, shares []share) (int64, error) {
	currency := c.Amount.Currency()
	var total int64
	var legs []ledger.Leg
	for _, sh := range shares {
		if sh.net() == 0 {
			continue
		}
		pending, err := s.account(ctx, tx, sh.recipient, c.Owner.Livemode, currency, Pending)
		if err != nil {
			return 0, err
		}
		net, _ := money.New(sh.net(), currency)
		legs = append(legs, ledger.Credit(pending, net))
		total += sh.net()
	}
	if total == 0 {
		return 0, nil
	}
	all, _ := money.New(total, currency)
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "split of " + c.Attempt, Legs: append([]ledger.Leg{ledger.Debit(c.Balance, all)}, legs...),
	})
	if err != nil {
		return 0, fmt.Errorf("posting the split: %w", err)
	}
	q := db.New(tx)
	for _, sh := range shares {
		for _, l := range sh.lines() {
			if err := q.InsertSplitLine(ctx, db.InsertSplitLineParams{
				AttemptID: c.Attempt, PaymentIntent: c.Intent.String(), RecipientID: sh.recipient, Type: l.kind, Amount: l.amount,
				Liable: sh.liable, LedgerTxn: txn.ID.String(),
			}); err != nil {
				return 0, err
			}
		}
		if err := s.movement(ctx, q, sh.recipient, c.Owner.Livemode, currency, Pending, "split", sh.net(), c.Attempt); err != nil {
			return 0, err
		}
	}
	return total, nil
}

// constitute adds a recipient's share of a capture to its units, installment by
// installment.
func (s *Service) constitute(ctx context.Context, tx pgx.Tx, c payments.CardCapture, arrangement string, sh share) error {
	gross, _ := money.New(sh.gross+sh.remainder, c.Amount.Currency())
	fee, _ := money.New(sh.fee, c.Amount.Currency())
	plan, err := planOf(c, gross, fee)
	if err != nil {
		return err
	}
	q := db.New(tx)
	for i, inst := range plan {
		u, err := s.unitFor(ctx, q, c.Owner, sh.recipient, arrangement, inst.date, c.Amount.Currency(), dayOf(c.At))
		if err != nil {
			return err
		}
		net := inst.gross - inst.fee
		if err := q.InsertInstallment(ctx, db.InsertInstallmentParams{
			AttemptID: c.Attempt, RecipientID: sh.recipient, Number: int32(i + 1), PaymentIntent: c.Intent.String(), UnitID: u.ID,
			Gross: inst.gross, Fee: inst.fee, Net: net,
		}); err != nil {
			return fmt.Errorf("recording installment %d of %s for %s: %w", i+1, c.Attempt, sh.recipient, err)
		}
		if err := s.change(ctx, q, u.ID, net, 0, "constituted", c.Attempt, dayOf(c.At)); err != nil {
			return err
		}
	}
	return nil
}

type installment struct {
	date       time.Time
	gross, fee int64
}

// planOf divides a recipient's share of a capture into its installments. Installments
// financed by the issuer are paid to the merchant in one go.
func planOf(c payments.CardCapture, amount, fee money.Amount) ([]installment, error) {
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
	gross, err := amount.Allocate(weights...)
	if err != nil {
		return nil, err
	}
	fees, err := fee.Allocate(weights...)
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

// unitFor finds or creates a recipient's unit for an arrangement and date, locked.
func (s *Service) unitFor(ctx context.Context, q *db.Queries, owner payments.Owner, recipientID, arrangement string, date time.Time, currency money.Currency, constituted time.Time) (db.ReceivablesUnit, error) {
	key := db.LockUnitParams{RecipientID: recipientID, Livemode: owner.Livemode, Arrangement: arrangement, SettlementDate: dateOf(date)}
	if err := q.InsertUnit(ctx, db.InsertUnitParams{
		ID: UnitPrefix.New().String(), MerchantID: owner.Merchant.String(), RecipientID: recipientID, Livemode: key.Livemode,
		Arrangement: arrangement, SettlementDate: key.SettlementDate, Currency: currency.Code(), ConstitutedOn: dateOf(constituted),
		Now: ts(s.cfg.Now()),
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

// change adds delta to a unit's value, and anticipatedDelta to what Jupiter bought of it,
// as a new version, and records why.
func (s *Service) change(ctx context.Context, q *db.Queries, unitID string, delta, anticipatedDelta int64, kind, reference string, changed time.Time) error {
	now := s.cfg.Now()
	if err := q.ChangeUnitValue(ctx, db.ChangeUnitValueParams{
		ID: unitID, Delta: delta, AnticipatedDelta: anticipatedDelta, ConstitutedOn: dateOf(changed), Now: ts(now),
	}); err != nil {
		return fmt.Errorf("changing unit %s: %w", unitID, err)
	}
	if delta == 0 {
		return nil
	}
	return q.InsertUnitEvent(ctx, db.InsertUnitEventParams{UnitID: unitID, At: ts(now), Kind: kind, Amount: delta, Reference: reference})
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
