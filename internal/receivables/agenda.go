package receivables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
	"github.com/iricardofernandes/jupiter/pkg/taxid"
)

// Entry is one unit of a merchant's agenda as it stood at a moment: what it was worth,
// what was committed to contracts at the registry, what was blocked, and what was free.
type Entry struct {
	Unit           string
	Arrangement    string
	SettlementDate string
	Currency       string
	Value          int64
	// Anticipated is what Jupiter bought of the unit, as it is now.
	Anticipated int64
	Blocked     int64
	Committed   []registryapi.Commitment
	Free        int64
	Settled     int64
	SettledOn   string
	Registered  bool
}

// AgendaQuery bounds an agenda by settlement date, inclusive, and reads it as of AsOf.
// Recipient names whose agenda: the merchant's own when empty.
type AgendaQuery struct {
	Recipient string
	From, To  time.Time
	AsOf      time.Time
}

var earliestAsOf = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

const (
	maxAgendaDays = 400
	maxOptIns     = 50
	// retryAfter is how long something the registry refused, or that could not be sent,
	// waits before it is tried again.
	retryAfter = time.Hour
	// maxErrorText is how much of an error is kept or logged.
	maxErrorText = 200
)

func truncate(s string) string {
	if len(s) > maxErrorText {
		return s[:maxErrorText]
	}
	return s
}

// Agenda is the merchant's units with a settlement date in the query's range, each as it
// stood at AsOf: its value from what was constituted and reduced until then, its
// commitments as the registry last reported them before then.
func (s *Service) Agenda(ctx context.Context, q db.DBTX, owner payments.Owner, a AgendaQuery) ([]Entry, error) {
	if a.To.Before(a.From) || a.To.Sub(a.From) > maxAgendaDays*24*time.Hour {
		return nil, fmt.Errorf("%w: an agenda covers up to %d days, from a date to a later one", ErrInvalid, maxAgendaDays)
	}
	now := s.cfg.Now()
	if a.AsOf.IsZero() {
		a.AsOf = now
	}
	if a.AsOf.Before(earliestAsOf) || a.AsOf.After(now.Add(time.Hour)) {
		return nil, fmt.Errorf("%w: as_of must be from 2000 to now", ErrInvalid)
	}
	recipientID, err := s.recipientOf(ctx, q, owner, a.Recipient)
	if err != nil {
		return nil, err
	}
	rows, err := db.New(q).AgendaAsOf(ctx, db.AgendaAsOfParams{
		MerchantID: owner.Merchant.String(), RecipientID: recipientID, Livemode: owner.Livemode, FromDate: dateOf(a.From), ToDate: dateOf(a.To), At: ts(a.AsOf),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(rows))
	for _, u := range rows {
		e := Entry{
			Unit: u.ID, Arrangement: u.Arrangement, SettlementDate: u.SettlementDate.Time.Format(time.DateOnly), Currency: u.Currency,
			Value: u.Value, Anticipated: u.Anticipated, Blocked: u.Blocked, Settled: u.Settled, Registered: u.RegisteredVersion > 0,
			Committed: []registryapi.Commitment{},
		}
		if u.Commitments != nil {
			if err := json.Unmarshal(u.Commitments, &e.Committed); err != nil {
				return nil, err
			}
		}
		if u.IsSettled && u.SettledOn.Valid {
			e.SettledOn = u.SettledOn.Time.Format(time.DateOnly)
		} else {
			e.Free = e.Value - e.Blocked
			for _, c := range e.Committed {
				e.Free -= c.Amount
			}
			e.Free = max(e.Free, 0)
		}
		out = append(out, e)
	}
	return out, nil
}

// OptIn is a merchant's authorization for a financier to see its agenda at the
// registry.
type OptIn struct {
	Financier string
	Active    bool
	Synced    bool
	UpdatedAt time.Time
}

// SetOptIn grants or revokes a financier's view of the merchant's agenda. The registry
// learns of it on the next pass, if not at once.
func (s *Service) SetOptIn(ctx context.Context, tx pgx.Tx, owner payments.Owner, financier string, active bool) (OptIn, error) {
	if len(financier) != 14 || !taxid.Valid(financier) {
		return OptIn{}, fmt.Errorf("%w: a financier is named by its CNPJ", ErrInvalid)
	}
	m, err := s.cfg.Merchants.Get(ctx, tx, owner.Merchant)
	if err != nil {
		return OptIn{}, err
	}
	if m.TaxID == "" {
		return OptIn{}, fmt.Errorf("%w: the merchant has no CPF or CNPJ yet, so the registry has no agenda of it", ErrInvalid)
	}
	q := db.New(tx)
	if n, err := q.CountOptIns(ctx, db.CountOptInsParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Financier: financier}); err != nil {
		return OptIn{}, err
	} else if n >= maxOptIns {
		return OptIn{}, fmt.Errorf("%w: a merchant authorizes up to %d financiers", ErrInvalid, maxOptIns)
	}
	now := s.cfg.Now()
	err = q.SetOptIn(ctx, db.SetOptInParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, Financier: financier, Active: active, Now: ts(now)})
	return OptIn{Financier: financier, Active: active, UpdatedAt: now}, err
}

func (s *Service) OptIns(ctx context.Context, q db.DBTX, owner payments.Owner) ([]OptIn, error) {
	rows, err := db.New(q).OptInsOf(ctx, db.OptInsOfParams{MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return nil, err
	}
	out := make([]OptIn, 0, len(rows))
	for _, r := range rows {
		out = append(out, OptIn{Financier: r.Financier, Active: r.Active, Synced: r.Synced, UpdatedAt: r.UpdatedAt.Time})
	}
	return out, nil
}

// SyncOptIns passes the opt-ins not yet at the registry on to it. One that fails waits
// to be tried again and holds up no other.
func (s *Service) SyncOptIns(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	synced := 0
	var failures []error
	for _, livemode := range []bool{false, true} {
		reg, err := s.registry(livemode)
		if errors.Is(err, ErrNoRegistry) {
			continue
		}
		q := db.New(pool)
		rows, err := q.OptInsToSync(ctx, db.OptInsToSyncParams{Livemode: livemode, Now: ts(s.cfg.Now())})
		if err != nil {
			return synced, err
		}
		holders := map[string]string{}
		for _, o := range rows {
			err := s.syncOptIn(ctx, pool, reg, holders, o)
			if err == nil {
				synced++
				continue
			}
			failures = append(failures, err)
			if err := q.MarkOptInError(ctx, db.MarkOptInErrorParams{
				MerchantID: o.MerchantID, Livemode: livemode, Financier: o.Financier, Error: truncate(err.Error()), RetryAt: ts(s.cfg.Now().Add(retryAfter)),
			}); err != nil {
				return synced, err
			}
		}
	}
	return synced, errors.Join(failures...)
}

func (s *Service) syncOptIn(ctx context.Context, pool *pgxpool.Pool, reg Registry, holders map[string]string, o db.ReceivablesOptIn) error {
	holder, err := s.merchantTaxID(ctx, pool, holders, o.MerchantID)
	if err != nil {
		return err
	}
	if holder == "" {
		return fmt.Errorf("receivables: merchant %s has no tax id", o.MerchantID)
	}
	if err := reg.SetOptIn(ctx, registryapi.OptIn{Holder: holder, Financier: o.Financier}, o.Active); err != nil {
		return err
	}
	return db.New(pool).MarkOptInSynced(ctx, db.MarkOptInSyncedParams{MerchantID: o.MerchantID, Livemode: o.Livemode, Financier: o.Financier, UpdatedAt: o.UpdatedAt})
}

// Advance is the worker's pass: units to the registry, opt-ins, the reconciliations that
// are due, automatic anticipations and their reports, the day's settlement, and the
// recipients' scheduled payouts.
func (s *Service) Advance(ctx context.Context, pool *pgxpool.Pool) error {
	_, err1 := s.Register(ctx, pool)
	_, err2 := s.SyncOptIns(ctx, pool)
	_, err3 := s.Reconcile(ctx, pool)
	_, err4 := s.AnticipateAutomatically(ctx, pool)
	_, err5 := s.ReportAnticipations(ctx, pool)
	var err6, err7 error
	if s.cfg.Payments != nil {
		err6 = s.SettleDay(ctx, pool)
		_, err7 = s.SchedulePayouts(ctx, pool)
	}
	return errors.Join(err1, err2, err3, err4, err5, err6, err7)
}
