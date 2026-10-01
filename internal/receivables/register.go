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

	"github.com/iricardofernandes/jupiter/internal/merchant"
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
		holder, err := s.holderOf(ctx, pool, holders, u.MerchantID)
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

func (s *Service) holderOf(ctx context.Context, q db.DBTX, cache map[string]string, merchantID string) (string, error) {
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
	domicile.Account = u.MerchantID
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
	holder, err := s.holderOf(ctx, q, map[string]string{}, u.MerchantID)
	return holder, reg, err
}
