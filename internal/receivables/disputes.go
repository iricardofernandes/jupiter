package receivables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// A card dispute is borne by the payment's liable recipient: its whole amount comes out
// of the recipient's available balance, to the merchant's balance the network took it
// from. What the balance does not have it owes, and that is recovered from its future
// units: each is reduced, nearest settlement first, as the registry will reduce it
// (Convenção 3.14): what is free first, then the contracts of other financiers from the
// latest accepted back. Jupiter stops short of its own contracts: reducing a unit it
// bought would only move the loss to itself.

// Disputed takes a card dispute from the payment's liable recipient to the merchant's
// balance, and answers how much: nothing for a payment without receivables.
func (s *Service) Disputed(ctx context.Context, tx pgx.Tx, d payments.CardDispute) (int64, error) {
	q := db.New(tx)
	lines, err := q.SplitLinesOfIntent(ctx, d.Intent.String())
	if err != nil || len(lines) == 0 {
		return 0, err
	}
	liable := lines[0].RecipientID
	for _, l := range lines {
		if l.Liable {
			liable = l.RecipientID
		}
	}
	inserted, err := q.InsertDispute(ctx, db.InsertDisputeParams{
		Reference: d.Reference, RecipientID: liable, Livemode: d.Owner.Livemode, Currency: d.Amount.Currency().Code(),
		Amount: d.Amount.Minor(), Now: ts(s.cfg.Now()),
	})
	if err != nil || inserted == 0 {
		return 0, errors.Join(err, fmt.Errorf("receivables: dispute %s was taken already", d.Reference))
	}
	if err := s.moveDisputed(ctx, tx, liable, d, false); err != nil {
		return 0, err
	}
	return d.Amount.Minor(), nil
}

// DisputeReinstated gives a won dispute back to the recipient it was taken from.
func (s *Service) DisputeReinstated(ctx context.Context, tx pgx.Tx, d payments.CardDispute) (int64, error) {
	q := db.New(tx)
	taken, err := q.LockDispute(ctx, d.Reference)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && taken.Reinstated) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	amount, err := money.New(taken.Amount, d.Amount.Currency())
	if err != nil {
		return 0, err
	}
	d.Amount = amount
	if err := s.moveDisputed(ctx, tx, taken.RecipientID, d, true); err != nil {
		return 0, err
	}
	return taken.Amount, q.MarkDisputeReinstated(ctx, d.Reference)
}

// moveDisputed moves a dispute's amount between the recipient's available balance and the
// merchant's: out of the recipient, or back to it.
func (s *Service) moveDisputed(ctx context.Context, tx pgx.Tx, recipientID string, d payments.CardDispute, back bool) error {
	available, err := s.account(ctx, tx, recipientID, d.Owner.Livemode, d.Amount.Currency(), Available)
	if err != nil {
		return err
	}
	legs, signed, description := []ledger.Leg{ledger.Debit(available, d.Amount), ledger.Credit(d.Balance, d.Amount)}, -d.Amount.Minor(), "dispute "
	if back {
		legs, signed, description = []ledger.Leg{ledger.Debit(d.Balance, d.Amount), ledger.Credit(available, d.Amount)}, d.Amount.Minor(), "reinstated dispute "
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: description + d.Reference, Legs: legs}); err != nil {
		return fmt.Errorf("posting a dispute's split: %w", err)
	}
	return s.movement(ctx, db.New(tx), recipientID, d.Owner.Livemode, d.Amount.Currency(), Available, "dispute", signed, d.Reference)
}

// Recovery is one unit reduced to recover what a recipient owed, as Jupiter planned it
// from the registry's position: the registry takes its own order when the unit is next
// registered, which a contract accepted in between can change.
type Recovery struct {
	Unit     string
	Amount   int64
	FromFree int64
	// FromContracts is what each other financier's contract gave up, by contract.
	FromContracts map[string]int64
}

// Recover reduces the future units of every recipient that owes, and answers how much it
// recovered. A recipient whose units cannot be read from the registry waits for the next
// pass; it holds up no other.
func (s *Service) Recover(ctx context.Context, pool *pgxpool.Pool) (int64, error) {
	var recovered int64
	var failures []error
	for after := ""; ; {
		debtors, err := db.New(pool).Debtors(ctx, after)
		if err != nil || len(debtors) == 0 {
			return recovered, errors.Join(append(failures, err)...)
		}
		for _, d := range debtors {
			after = d.Key
			currency, err := money.CurrencyByCode(d.Currency)
			if err != nil {
				return recovered, err
			}
			var got []Recovery
			err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
				got, err = s.recover(ctx, tx, d.RecipientID, d.Livemode, currency)
				return err
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("recovering from %s: %w", d.RecipientID, err))
				continue // rolled back
			}
			for _, r := range got {
				recovered += r.Amount
			}
		}
	}
}

// recover reduces a recipient's units, nearest settlement first, by up to what its
// available balance is below zero.
func (s *Service) recover(ctx context.Context, tx pgx.Tx, recipientID string, livemode bool, currency money.Currency) ([]Recovery, error) {
	q := db.New(tx)
	reg, err := s.registry(livemode)
	if err != nil && !errors.Is(err, ErrNoRegistry) {
		return nil, err
	}
	units, err := q.RecoverableUnits(ctx, db.RecoverableUnitsParams{
		RecipientID: recipientID, Livemode: livemode, Currency: currency.Code(), Today: dateOf(dayOf(s.cfg.Now())), Registered: reg != nil,
	})
	if err != nil || len(units) == 0 {
		return nil, err
	}
	owed, err := s.owed(ctx, tx, recipientID, livemode, currency)
	if err != nil || owed <= 0 {
		return nil, err
	}
	plan, err := s.planRecovery(ctx, tx, reg, units, owed)
	if err != nil {
		return nil, err
	}
	for i, r := range plan {
		if err := s.reduceUnit(ctx, tx, units[r.index], recipientID, currency, r.Recovery); err != nil {
			return recovered(plan[:i]), err
		}
	}
	return recovered(plan), nil
}

type plannedRecovery struct {
	Recovery
	index int
}

func recovered(plan []plannedRecovery) []Recovery {
	out := make([]Recovery, len(plan))
	for i, p := range plan {
		out[i] = p.Recovery
	}
	return out
}

// planRecovery is what reducing each unit, in order, takes until owed is recovered: as
// the registry's waterfall has it where a registry keeps the units, all the recipient
// has on the unit otherwise.
func (s *Service) planRecovery(ctx context.Context, tx pgx.Tx, reg Registry, units []db.ReceivablesUnit, owed int64) ([]plannedRecovery, error) {
	var positions map[key]registryapi.Position
	holder := ""
	if reg != nil {
		var err error
		if holder, err = s.holderOf(ctx, tx, map[string]string{}, units[0]); err != nil {
			return nil, err
		}
		if positions, err = unsettledPositions(ctx, reg, holder); err != nil {
			return nil, err
		}
	}
	var plan []plannedRecovery
	for i, u := range units {
		if owed == 0 {
			break
		}
		want := min(owed, u.Value-u.Anticipated-u.Blocked)
		r := Recovery{Unit: u.ID, FromContracts: map[string]int64{}, Amount: want, FromFree: want}
		if reg != nil {
			p, ok := positions[key{holder, u.Arrangement, u.SettlementDate.Time.Format(time.DateOnly)}]
			if !ok {
				continue
			}
			r.FromFree, r.FromContracts = waterfall(p, s.cfg.TaxID, want)
			r.Amount = r.FromFree
			for _, x := range r.FromContracts {
				r.Amount += x
			}
		}
		if r.Amount > 0 {
			plan = append(plan, plannedRecovery{Recovery: r, index: i})
			owed -= r.Amount
		}
	}
	return plan, nil
}

func unsettledPositions(ctx context.Context, reg Registry, holder string) (map[key]registryapi.Position, error) {
	unsettled := false
	found, err := reg.Units(ctx, &unsettled, holder, "", "")
	if err != nil {
		return nil, err
	}
	positions := make(map[key]registryapi.Position, len(found))
	for _, p := range found {
		positions[key{p.Holder, p.Arrangement, p.SettlementDate}] = p
	}
	return positions, nil
}

// owed is how far a recipient's available balance is below zero.
func (s *Service) owed(ctx context.Context, tx pgx.Tx, recipientID string, livemode bool, currency money.Currency) (int64, error) {
	available, err := s.account(ctx, tx, recipientID, livemode, currency, Available)
	if err != nil {
		return 0, err
	}
	b, err := s.cfg.Ledger.Balance(ctx, tx, available)
	if err != nil {
		return 0, err
	}
	posted, err := b.Posted()
	if err != nil {
		return 0, err
	}
	return -posted.Minor(), nil
}

// waterfall is what reducing a unit by up to want takes, in the registry's order: its
// free amount, then each contract from the latest accepted back, stopping at the first
// of the accreditor's own (beneficiary jupiter).
func waterfall(p registryapi.Position, jupiter string, want int64) (fromFree int64, fromContracts map[string]int64) {
	fromContracts = map[string]int64{}
	fromFree = min(want, max(p.Free, 0))
	want -= fromFree
	for i := len(p.Committed) - 1; i >= 0 && want > 0; i-- {
		c := p.Committed[i]
		if c.Beneficiary == jupiter {
			break
		}
		take := min(want, c.Amount)
		if take > 0 {
			fromContracts[c.Contract] += take
			want -= take
		}
	}
	return fromFree, fromContracts
}

// reduceUnit takes a recovery out of a unit: from its installments in proportion to what
// each holds, as a new version for the registry, and from the recipient's pending balance
// to its available one.
func (s *Service) reduceUnit(ctx context.Context, tx pgx.Tx, u db.ReceivablesUnit, recipientID string, currency money.Currency, r Recovery) error {
	q := db.New(tx)
	installments, err := q.InstallmentsOfUnit(ctx, u.ID)
	if err != nil {
		return err
	}
	weights := make([]int64, len(installments))
	for i, inst := range installments {
		weights[i] = inst.Net - inst.Reduced
	}
	amount, _ := money.New(r.Amount, currency)
	shares, err := amount.Allocate(weights...)
	if err != nil {
		return fmt.Errorf("recovering from unit %s: %w", u.ID, err)
	}
	for i, inst := range installments {
		if x := shares[i].Minor(); x > 0 {
			if err := q.ReduceInstallment(ctx, db.ReduceInstallmentParams{AttemptID: inst.AttemptID, RecipientID: inst.RecipientID, Number: inst.Number, Amount: x}); err != nil {
				return err
			}
		}
	}
	if err := s.change(ctx, q, u.ID, -r.Amount, 0, "reduced", "recovery", dayOf(s.cfg.Now())); err != nil {
		return err
	}
	pending, err := s.account(ctx, tx, recipientID, u.Livemode, currency, Pending)
	if err != nil {
		return err
	}
	available, err := s.account(ctx, tx, recipientID, u.Livemode, currency, Available)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{
		Description: "recovery from " + u.ID, Legs: []ledger.Leg{ledger.Debit(pending, amount), ledger.Credit(available, amount)},
	}); err != nil {
		return fmt.Errorf("posting a recovery: %w", err)
	}
	if err := s.movement(ctx, q, recipientID, u.Livemode, currency, Pending, "recovery", -r.Amount, u.ID); err != nil {
		return err
	}
	if err := s.movement(ctx, q, recipientID, u.Livemode, currency, Available, "recovery", r.Amount, u.ID); err != nil {
		return err
	}
	contracts, err := json.Marshal(r.FromContracts)
	if err != nil {
		return err
	}
	return q.InsertRecovery(ctx, db.InsertRecoveryParams{
		RecipientID: recipientID, UnitID: u.ID, Amount: r.Amount, FromFree: r.FromFree, FromContracts: contracts, At: ts(s.cfg.Now()),
	})
}

// Recoveries lists the reductions made to recover what a recipient owed.
func (s *Service) Recoveries(ctx context.Context, q db.DBTX, recipientID string) ([]Recovery, error) {
	rows, err := db.New(q).RecoveriesOf(ctx, recipientID)
	if err != nil {
		return nil, err
	}
	out := make([]Recovery, 0, len(rows))
	for _, r := range rows {
		rec := Recovery{Unit: r.UnitID, Amount: r.Amount, FromFree: r.FromFree}
		if err := json.Unmarshal(r.FromContracts, &rec.FromContracts); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, nil
}
