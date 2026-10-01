package receivables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

const registerBatch = 500

// Register sends the registry every unit changed since it last took it, in each mode
// that has one. A unit is sent whole (its value and block, absolute), so sending it
// again is harmless. Units of a merchant without a tax id wait for one.
func (s *Service) Register(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	sent := 0
	var failures []error
	for _, livemode := range []bool{false, true} {
		reg, err := s.registry(livemode)
		if errors.Is(err, ErrNoRegistry) {
			continue
		}
		n, err := s.register(ctx, pool, reg, livemode)
		sent += n
		failures = append(failures, err)
	}
	return sent, errors.Join(failures...)
}

func (s *Service) register(ctx context.Context, pool *pgxpool.Pool, reg Registry, livemode bool) (int, error) {
	q := db.New(pool)
	units, err := q.UnitsToRegister(ctx, db.UnitsToRegisterParams{Livemode: livemode, Now: ts(s.cfg.Now()), MaxCount: registerBatch})
	if err != nil || len(units) == 0 {
		return 0, err
	}
	holders := map[string]string{}
	var batch []registryapi.Unit
	var sending []db.ReceivablesUnit
	for _, u := range units {
		holder, err := s.holderOf(ctx, pool, holders, u)
		if err != nil {
			return 0, err
		}
		if holder == "" {
			if err := q.MarkRegisterError(ctx, db.MarkRegisterErrorParams{ID: u.ID, Error: "the merchant has no tax id", RetryAt: ts(s.cfg.Now().Add(retryAfter))}); err != nil {
				return 0, err
			}
			continue
		}
		batch = append(batch, s.wireUnit(u, holder))
		sending = append(sending, u)
	}
	if len(batch) == 0 {
		return 0, nil
	}
	results, err := reg.SetUnits(ctx, batch)
	if err != nil {
		return 0, err
	}
	if len(results) != len(batch) {
		return 0, fmt.Errorf("receivables: the registry answered %d units of %d", len(results), len(batch))
	}
	sent := 0
	for i, r := range results {
		u := sending[i]
		if r.Error != "" {
			s.cfg.Logger.WarnContext(ctx, "the registry refused a unit", "unit", u.ID, "error", truncate(r.Error))
			err = q.MarkRegisterError(ctx, db.MarkRegisterErrorParams{ID: u.ID, Error: truncate(r.Error), RetryAt: ts(s.cfg.Now().Add(retryAfter))})
		} else {
			sent++
			err = q.MarkRegistered(ctx, db.MarkRegisteredParams{ID: u.ID, Version: u.Version, Now: ts(s.cfg.Now())})
		}
		if err != nil {
			return sent, err
		}
	}
	return sent, nil
}

// holderOf is the CPF or CNPJ a recipient's units are registered under.
func (s *Service) holderOf(ctx context.Context, q db.DBTX, cache map[string]string, u db.ReceivablesUnit) (string, error) {
	if holder, ok := cache[u.RecipientID]; ok {
		return holder, nil
	}
	rec, err := s.cfg.Recipients.ByID(ctx, q, u.RecipientID)
	if err != nil {
		return "", err
	}
	cache[u.RecipientID] = rec.TaxID
	return rec.TaxID, nil
}

func (s *Service) merchantTaxID(ctx context.Context, q db.DBTX, cache map[string]string, merchantID string) (string, error) {
	if holder, ok := cache[merchantID]; ok {
		return holder, nil
	}
	mid, err := merchant.MerchantPrefix.Parse(merchantID)
	if err != nil {
		return "", err
	}
	m, err := s.cfg.Merchants.Get(ctx, q, mid)
	if err != nil {
		return "", err
	}
	cache[merchantID] = m.TaxID
	return m.TaxID, nil
}

func (s *Service) wireUnit(u db.ReceivablesUnit, holder string) registryapi.Unit {
	domicile := s.cfg.Domicile
	domicile.Account = u.RecipientID
	return registryapi.Unit{
		Holder: holder, Arrangement: u.Arrangement, SettlementDate: u.SettlementDate.Time.Format(time.DateOnly),
		Value: u.Value, Blocked: u.Blocked, Domicile: domicile, ConstitutedOn: u.ConstitutedOn.Time.Format(time.DateOnly),
	}
}

// Instructions are how the registry says a unit settles: to whom, how much, where.
func (s *Service) Instructions(ctx context.Context, pool *pgxpool.Pool, unitID string) ([]registryapi.Payment, error) {
	u, holder, reg, err := s.registered(ctx, pool, unitID)
	if err != nil {
		return nil, err
	}
	return reg.Instructions(ctx, holder, u.Arrangement, u.SettlementDate.Time.Format(time.DateOnly))
}

// Settle tells the registry a unit was settled on settledOn, for its value less its
// block, and records how the registry split it. The unit stays locked while the registry
// is told, so no capture or refund can change it between the two. Telling it again
// changes nothing.
func (s *Service) Settle(ctx context.Context, pool *pgxpool.Pool, unitID string, settledOn time.Time) ([]registryapi.Payment, error) {
	// A calendar date, taken as given.
	day := time.Date(settledOn.Year(), settledOn.Month(), settledOn.Day(), 0, 0, 0, 0, time.UTC)
	var paid []registryapi.Payment
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		u, err := q.LockUnitByID(ctx, unitID)
		if err != nil {
			return notFoundOr(err, unitID)
		}
		if u.SettledOn.Valid {
			return json.Unmarshal(u.Payments, &paid)
		}
		if day.Before(u.SettlementDate.Time) || day.After(dayOf(s.cfg.Now())) {
			return fmt.Errorf("%w: unit %s settles on %s, from its settlement date to today", ErrInvalid, unitID, u.SettlementDate.Time.Format(time.DateOnly))
		}
		holder, reg, err := s.registeredAs(ctx, tx, u)
		if err != nil {
			return err
		}
		if err := s.checkInstructions(ctx, reg, u, holder); err != nil {
			return err
		}
		amount := u.Value - u.Blocked
		paid, err = reg.Settle(ctx, registryapi.Settlement{
			Holder: holder, Arrangement: u.Arrangement, SettlementDate: u.SettlementDate.Time.Format(time.DateOnly),
			Amount: amount, SettledOn: day.Format(time.DateOnly),
		})
		if err != nil {
			return err
		}
		raw, err := json.Marshal(paid)
		if err != nil {
			return err
		}
		now := ts(s.cfg.Now())
		if err := q.MarkSettled(ctx, db.MarkSettledParams{ID: unitID, SettledOn: dateOf(day), Amount: pgtype.Int8{Int64: amount, Valid: true}, Payments: raw, Now: now}); err != nil {
			return err
		}
		if err := s.postSettlement(ctx, tx, u, holder, paid); err != nil {
			return err
		}
		return q.InsertUnitEvent(ctx, db.InsertUnitEventParams{UnitID: unitID, At: now, Kind: "settled", Amount: amount, Reference: day.Format(time.DateOnly)})
	})
	return paid, err
}

// registered reads a unit the registry has in its current version.
func (s *Service) registered(ctx context.Context, pool *pgxpool.Pool, unitID string) (db.ReceivablesUnit, string, Registry, error) {
	u, err := db.New(pool).GetUnit(ctx, unitID)
	if err != nil {
		return u, "", nil, notFoundOr(err, unitID)
	}
	holder, reg, err := s.registeredAs(ctx, pool, u)
	return u, holder, reg, err
}

// registeredAs is the holder a unit is registered under, and its mode's registry; an
// error when the registry does not have the unit as it is.
func (s *Service) registeredAs(ctx context.Context, q db.DBTX, u db.ReceivablesUnit) (string, Registry, error) {
	reg, err := s.registry(u.Livemode)
	if err != nil {
		return "", nil, err
	}
	if u.RegisteredVersion < u.Version {
		return "", nil, fmt.Errorf("%w: unit %s is not registered as it is yet", ErrInvalid, u.ID)
	}
	holder, err := s.holderOf(ctx, q, map[string]string{}, u)
	return holder, reg, err
}

// checkInstructions: the split the registry will make must give Jupiter what it bought,
// no more and no less, or the recipient's balance cannot follow it.
func (s *Service) checkInstructions(ctx context.Context, reg Registry, u db.ReceivablesUnit, holder string) error {
	planned, err := reg.Instructions(ctx, holder, u.Arrangement, u.SettlementDate.Time.Format(time.DateOnly))
	if err != nil {
		return err
	}
	if jupiters := paidTo(planned, s.cfg.TaxID); jupiters != u.Anticipated {
		return fmt.Errorf("%w: the registry would pay Jupiter %d of unit %s, which it bought %d of", ErrInvalid, jupiters, u.ID, u.Anticipated)
	}
	return nil
}

func paidTo(payments []registryapi.Payment, beneficiary string) int64 {
	var total int64
	for _, p := range payments {
		if p.To == beneficiary && p.Contract != "" {
			total += p.Amount
		}
	}
	return total
}

// postSettlement moves what the recipient had pending on a settled unit: what the registry
// paid the holder becomes available to it, and what it paid other financiers is owed to
// them. What Jupiter bought was never pending.
func (s *Service) postSettlement(ctx context.Context, tx pgx.Tx, u db.ReceivablesUnit, holder string, paid []registryapi.Payment) error {
	var toHolder, toOthers int64
	for _, p := range paid {
		switch {
		case p.Contract == "" && p.To == holder:
			toHolder += p.Amount
		case p.Contract != "" && p.To != s.cfg.TaxID:
			toOthers += p.Amount
		}
	}
	if toHolder+toOthers != u.Value-u.Anticipated-u.Blocked {
		return fmt.Errorf("receivables: unit %s settled %d to its holder and %d to others, of %d pending", u.ID, toHolder, toOthers, u.Value-u.Anticipated-u.Blocked)
	}
	currency, err := money.CurrencyByCode(u.Currency)
	if err != nil {
		return err
	}
	pending, err := s.account(ctx, tx, u.RecipientID, u.Livemode, currency, Pending)
	if err != nil {
		return err
	}
	all, _ := money.New(toHolder+toOthers, currency)
	legs := []ledger.Leg{ledger.Debit(pending, all)}
	if toHolder > 0 {
		available, err := s.account(ctx, tx, u.RecipientID, u.Livemode, currency, Available)
		if err != nil {
			return err
		}
		amount, _ := money.New(toHolder, currency)
		legs = append(legs, ledger.Credit(available, amount))
	}
	if toOthers > 0 {
		financiers, err := s.account(ctx, tx, "", u.Livemode, currency, roleFinanciers)
		if err != nil {
			return err
		}
		amount, _ := money.New(toOthers, currency)
		legs = append(legs, ledger.Credit(financiers, amount))
	}
	if all.IsZero() {
		return nil
	}
	if _, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: "settlement of " + u.ID, Legs: legs}); err != nil {
		return fmt.Errorf("posting the settlement: %w", err)
	}
	q := db.New(tx)
	if err := s.movement(ctx, q, u.RecipientID, u.Livemode, currency, Pending, "settlement", -(toHolder + toOthers), u.ID); err != nil {
		return err
	}
	return s.movement(ctx, q, u.RecipientID, u.Livemode, currency, Available, "settlement", toHolder, u.ID)
}
