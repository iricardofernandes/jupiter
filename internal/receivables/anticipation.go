package receivables

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/receivables/rules"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

var (
	QuotePrefix        = id.MustPrefix("aq")
	AnticipationPrefix = id.MustPrefix("ant")
)

// quoteLifetime is how long a simulated price holds.
const quoteLifetime = 10 * time.Minute

// Anticipated is one unit an anticipation buys: Amount of it, for Price, Days before it
// settles.
type Anticipated struct {
	Unit           string `json:"unit"`
	SettlementDate string `json:"settlement_date"`
	Amount         int64  `json:"amount"`
	Price          int64  `json:"price"`
	Days           int    `json:"days"`
}

// Quote is what anticipating units would pay a recipient now.
type Quote struct {
	ID          string
	Recipient   string
	Currency    money.Currency
	MonthlyRate string
	Units       []Anticipated
	Amount      int64
	Price       int64
	ExpiresAt   time.Time
}

// Anticipation is units Jupiter bought from a recipient.
type Anticipation struct {
	ID          string
	Recipient   string
	Currency    money.Currency
	MonthlyRate string
	Units       []Anticipated
	Amount      int64
	Price       int64
	Automatic   bool
	CreatedAt   time.Time
}

// Fee is what Jupiter earns: the amount less the price.
func (a Anticipation) Fee() int64 { return a.Amount - a.Price }

// price is amount discounted at the monthly rate over days, as simple discount, rounded
// down: amount / (1 + rate × days / 30).
func price(amount int64, monthlyRate string, days int) (int64, error) {
	rate, ok := new(big.Rat).SetString(monthlyRate)
	if !ok {
		return 0, fmt.Errorf("receivables: monthly rate %q", monthlyRate)
	}
	divisor := new(big.Rat).Add(big.NewRat(1, 1), new(big.Rat).Mul(rate, big.NewRat(int64(days), 30)))
	p := new(big.Rat).Quo(new(big.Rat).SetInt64(amount), divisor)
	return new(big.Int).Quo(p.Num(), p.Denom()).Int64(), nil
}

// RegistryFree is what the registry holds free on a holder's unsettled units.
type RegistryFree map[key]int64

// FreeAtRegistry asks the registry what it holds free on a verified recipient's units. It
// is read outside any transaction, so that a slow registry holds no connection.
func (s *Service) FreeAtRegistry(ctx context.Context, q db.DBTX, owner payments.Owner, recipientID string) (RegistryFree, error) {
	rid, err := recipients.Prefix.Parse(recipientID)
	if err != nil {
		return nil, fmt.Errorf("%w: no recipient %s", ErrNotFound, recipientID)
	}
	rec, err := s.cfg.Recipients.Get(ctx, q, recipients.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode}, rid)
	if err != nil {
		return nil, fmt.Errorf("%w: no recipient %s", ErrNotFound, recipientID)
	}
	if rec.Status != recipients.Verified {
		return nil, fmt.Errorf("%w: recipient %s is %s, not verified", ErrInvalid, rec.ID, rec.Status)
	}
	reg, err := s.registry(owner.Livemode)
	if err != nil {
		return nil, err
	}
	return s.registryFree(ctx, reg, rec.TaxID, "")
}

// Simulate prices anticipating a verified recipient's units: those named, or all it has
// that can be. A unit can be anticipated for what of it the recipient still has pending
// and the registry holds free, as FreeAtRegistry read it, and only once registered as it
// is and before it settles.
func (s *Service) Simulate(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string, unitIDs []string, free RegistryFree) (Quote, error) {
	rec, _, err := s.anticipator(ctx, tx, owner, recipientID)
	if err != nil {
		return Quote{}, err
	}
	items, currency, err := s.anticipable(ctx, tx, free, owner, rec, unitIDs, s.cfg.Now(), false)
	if err != nil {
		return Quote{}, err
	}
	rate := rules.RateFor(s.cfg.Now())
	q := Quote{ID: QuotePrefix.New().String(), Recipient: rec.ID.String(), Currency: currency, MonthlyRate: rate.Monthly, ExpiresAt: s.cfg.Now().Add(quoteLifetime)}
	if q.Units, q.Amount, q.Price, err = priced(items, rate.Monthly); err != nil {
		return Quote{}, err
	}
	raw, err := json.Marshal(q.Units)
	if err != nil {
		return Quote{}, err
	}
	err = db.New(tx).InsertQuote(ctx, db.InsertQuoteParams{
		ID: q.ID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, RecipientID: q.Recipient, MonthlyRate: q.MonthlyRate,
		Units: raw, Amount: q.Amount, Price: q.Price, ExpiresAt: ts(q.ExpiresAt), CreatedAt: ts(s.cfg.Now()),
	})
	return q, err
}

// anticipator is a verified recipient of the merchant's, and its mode's registry.
func (s *Service) anticipator(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string) (recipients.Recipient, Registry, error) {
	rec, err := s.recipient(ctx, tx, owner, recipientID)
	if err != nil {
		return rec, nil, err
	}
	if rec.Status != recipients.Verified {
		return rec, nil, fmt.Errorf("%w: recipient %s is %s, not verified", ErrInvalid, rec.ID, rec.Status)
	}
	reg, err := s.registry(owner.Livemode)
	return rec, reg, err
}

// anticipable reads the recipient's units that can be anticipated, constituted by
// before, locked when they are to be bought, with what of each can be: its pending
// amount, up to what the registry holds free.
func (s *Service) anticipable(ctx context.Context, tx pgx.Tx, free RegistryFree, owner payments.Owner, rec recipients.Recipient, unitIDs []string, before time.Time, lock bool) ([]Anticipated, money.Currency, error) {
	if unitIDs == nil {
		unitIDs = []string{}
	}
	params := db.AnticipableUnitsParams{
		RecipientID: rec.ID.String(), Livemode: owner.Livemode, Today: dateOf(dayOf(s.cfg.Now())), Ids: unitIDs, ConstitutedBefore: ts(before),
	}
	var units []db.ReceivablesUnit
	var err error
	if lock {
		units, err = db.New(tx).AnticipableUnits(ctx, params)
	} else {
		units, err = db.New(tx).PeekAnticipableUnits(ctx, db.PeekAnticipableUnitsParams(params))
	}
	if err != nil {
		return nil, money.Currency{}, err
	}
	if len(units) == 0 || (len(unitIDs) > 0 && len(units) != len(unitIDs)) {
		return nil, money.Currency{}, fmt.Errorf("%w: no units, or some named, that can be anticipated: registered, unsettled, not yet bought", ErrInvalid)
	}
	currency, err := money.CurrencyByCode(units[0].Currency)
	if err != nil {
		return nil, money.Currency{}, err
	}
	today := dayOf(s.cfg.Now())
	var out []Anticipated
	for _, u := range units {
		date := u.SettlementDate.Time.Format(time.DateOnly)
		amount := min(u.Value-u.Anticipated, free[key{rec.TaxID, u.Arrangement, date}])
		if amount <= 0 {
			if len(unitIDs) > 0 {
				return nil, money.Currency{}, fmt.Errorf("%w: unit %s has nothing free to anticipate", ErrInvalid, u.ID)
			}
			continue
		}
		out = append(out, Anticipated{Unit: u.ID, SettlementDate: date, Amount: amount, Days: int(u.SettlementDate.Time.Sub(today).Hours() / 24)})
	}
	if len(out) == 0 {
		return nil, money.Currency{}, fmt.Errorf("%w: nothing free to anticipate", ErrInvalid)
	}
	return out, currency, nil
}

// registryFree is what the registry holds free on each of a holder's unsettled units.
// Contracts of Jupiter's named under own (a quote that was begun, its answer lost) count
// as free: carrying the quote out again places them again, the same.
func (s *Service) registryFree(ctx context.Context, reg Registry, holder, own string) (RegistryFree, error) {
	unsettled := false
	positions, err := reg.Units(ctx, &unsettled, holder, "", "")
	if err != nil {
		return nil, err
	}
	free := RegistryFree{}
	for _, p := range positions {
		k := key{p.Holder, p.Arrangement, p.SettlementDate}
		free[k] = p.Free
		for _, c := range p.Committed {
			if own != "" && c.Beneficiary == s.cfg.TaxID && strings.HasPrefix(c.Contract, own+"/") {
				free[k] += c.Amount
			}
		}
	}
	return free, nil
}

func priced(items []Anticipated, rate string) ([]Anticipated, int64, int64, error) {
	var amount, total int64
	for i := range items {
		p, err := price(items[i].Amount, rate, items[i].Days)
		if err != nil {
			return nil, 0, 0, err
		}
		if p <= 0 {
			return nil, 0, 0, fmt.Errorf("%w: unit %s is too small to anticipate", ErrInvalid, items[i].Unit)
		}
		items[i].Price = p
		amount += items[i].Amount
		total += p
	}
	return items, amount, total, nil
}

// Anticipate carries out a quote: the units are bought at the quoted price if each still
// has, here and at the registry, what the quote counted on. The registry records the
// purchase as an ownership transfer to Jupiter; the ledger moves the amount out of the
// recipient's pending balance, the price into its available balance, and the difference
// to Jupiter's anticipation fees. A quote is used once.
func (s *Service) Anticipate(ctx context.Context, pool *pgxpool.Pool, owner payments.Owner, quoteID string) (Anticipation, error) {
	var out Anticipation
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		quote, err := q.LockQuote(ctx, db.LockQuoteParams{ID: quoteID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
		if err != nil {
			return notFoundOr(err, quoteID)
		}
		if quote.Anticipation.Valid {
			out, err = s.anticipation(ctx, tx, owner, quote.Anticipation.String)
			return err
		}
		rec, reg, items, err := s.stillGood(ctx, tx, owner, quote)
		if err != nil {
			return err
		}
		out, err = s.execute(ctx, tx, reg, owner, rec, quoteID, quote.MonthlyRate, items, false)
		if err != nil {
			return err
		}
		return q.UseQuote(ctx, db.UseQuoteParams{ID: quoteID, Anticipation: pgtype.Text{String: out.ID, Valid: true}})
	})
	return out, err
}

// stillGood checks that a quote can be carried out: not expired, and each of its units,
// locked, still with what the quote counted on, here and at the registry.
func (s *Service) stillGood(ctx context.Context, tx pgx.Tx, owner payments.Owner, quote db.ReceivablesAnticipationQuote) (recipients.Recipient, Registry, []Anticipated, error) {
	if s.cfg.Now().After(quote.ExpiresAt.Time) {
		return recipients.Recipient{}, nil, nil, fmt.Errorf("%w: quote %s expired at %s", ErrInvalid, quote.ID, quote.ExpiresAt.Time.Format(time.RFC3339))
	}
	var items []Anticipated
	if err := json.Unmarshal(quote.Units, &items); err != nil {
		return recipients.Recipient{}, nil, nil, err
	}
	rec, reg, err := s.anticipator(ctx, tx, owner, quote.RecipientID)
	if err != nil {
		return rec, nil, nil, err
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.Unit)
	}
	free, err := s.registryFree(ctx, reg, rec.TaxID, quote.ID)
	if err != nil {
		return rec, nil, nil, err
	}
	now, _, err := s.anticipable(ctx, tx, free, owner, rec, ids, s.cfg.Now(), true)
	if err != nil {
		return rec, nil, nil, err
	}
	for i, it := range items {
		if now[i].Unit != it.Unit || now[i].Amount < it.Amount {
			return rec, nil, nil, fmt.Errorf("%w: unit %s no longer has what quote %s counted on", ErrInvalid, it.Unit, quote.ID)
		}
	}
	return rec, reg, items, nil
}

// execute buys the units: contracts at the registry first, named by the quote so that a
// repeat places the same ones, then the ledger and the units.
func (s *Service) execute(ctx context.Context, tx pgx.Tx, reg Registry, owner payments.Owner, rec recipients.Recipient, quoteID, rate string, items []Anticipated, automatic bool) (Anticipation, error) {
	a := Anticipation{ID: AnticipationPrefix.New().String(), Recipient: rec.ID.String(), MonthlyRate: rate, Units: items, Automatic: automatic, CreatedAt: s.cfg.Now()}
	q := db.New(tx)
	for _, it := range items {
		a.Amount += it.Amount
		a.Price += it.Price
		u, err := q.GetUnit(ctx, it.Unit)
		if err != nil {
			return a, err
		}
		if a.Currency, err = money.CurrencyByCode(u.Currency); err != nil {
			return a, err
		}
		if err := reg.AcceptContract(ctx, registryapi.Contract{
			ID: quoteID + "/" + it.Unit, Holder: rec.TaxID, Effect: "ownership_transfer", Rule: "fixed", Amount: it.Amount,
			Arrangements: []string{u.Arrangement}, Accreditors: []string{s.cfg.TaxID}, From: it.SettlementDate, To: it.SettlementDate,
			Domicile: s.cfg.Domicile,
		}); err != nil {
			return a, fmt.Errorf("registering the anticipation of %s: %w", it.Unit, err)
		}
	}
	txn, err := s.postAnticipation(ctx, tx, owner, rec.ID.String(), a)
	if err != nil {
		return a, err
	}
	if err := q.InsertAnticipation(ctx, db.InsertAnticipationParams{
		ID: a.ID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode, RecipientID: a.Recipient, Currency: a.Currency.Code(),
		Amount: a.Amount, Price: a.Price, MonthlyRate: rate, Automatic: automatic, LedgerTxn: txn.String(), CreatedAt: ts(a.CreatedAt),
	}); err != nil {
		return a, err
	}
	for _, it := range items {
		if err := q.InsertAnticipationUnit(ctx, db.InsertAnticipationUnitParams{
			AnticipationID: a.ID, UnitID: it.Unit, Amount: it.Amount, Price: it.Price, Days: int32(it.Days), Contract: quoteID + "/" + it.Unit, //nolint:gosec // days to a settlement date
		}); err != nil {
			return a, err
		}
		if err := s.change(ctx, q, it.Unit, 0, it.Amount, "anticipated", a.ID, dayOf(s.cfg.Now())); err != nil {
			return a, err
		}
	}
	if s.cfg.Events != nil {
		if _, err := s.cfg.Events.Publish(ctx, tx, events.Owner{Merchant: owner.Merchant, Livemode: owner.Livemode},
			events.TypeAnticipationCreated, events.ObjectRef{ID: a.ID, Type: "anticipation"}); err != nil {
			return a, err
		}
	}
	return a, nil
}

func (s *Service) postAnticipation(ctx context.Context, tx pgx.Tx, owner payments.Owner, recipientID string, a Anticipation) (id.ID, error) {
	pending, err := s.account(ctx, tx, recipientID, owner.Livemode, a.Currency, Pending)
	if err != nil {
		return id.ID{}, err
	}
	available, err := s.account(ctx, tx, recipientID, owner.Livemode, a.Currency, Available)
	if err != nil {
		return id.ID{}, err
	}
	fees, err := s.account(ctx, tx, "", owner.Livemode, a.Currency, roleAnticipationFees)
	if err != nil {
		return id.ID{}, err
	}
	amount, _ := money.New(a.Amount, a.Currency)
	price, _ := money.New(a.Price, a.Currency)
	legs := []ledger.Leg{ledger.Debit(pending, amount), ledger.Credit(available, price)}
	if a.Fee() > 0 {
		fee, _ := money.New(a.Fee(), a.Currency)
		legs = append(legs, ledger.Credit(fees, fee))
	}
	txn, err := s.cfg.Ledger.Post(ctx, tx, ledger.Posting{Description: "anticipation " + a.ID, Legs: legs})
	if err != nil {
		return id.ID{}, fmt.Errorf("posting the anticipation: %w", err)
	}
	q := db.New(tx)
	if err := s.movement(ctx, q, recipientID, owner.Livemode, a.Currency, Pending, "anticipation", -a.Amount, a.ID); err != nil {
		return id.ID{}, err
	}
	return txn.ID, s.movement(ctx, q, recipientID, owner.Livemode, a.Currency, Available, "anticipation", a.Price, a.ID)
}

// Anticipation reads one of the merchant's anticipations.
func (s *Service) Anticipation(ctx context.Context, q db.DBTX, owner payments.Owner, anticipationID string) (Anticipation, error) {
	return s.anticipation(ctx, q, owner, anticipationID)
}

func (s *Service) anticipation(ctx context.Context, q db.DBTX, owner payments.Owner, anticipationID string) (Anticipation, error) {
	qs := db.New(q)
	row, err := qs.GetAnticipation(ctx, db.GetAnticipationParams{ID: anticipationID, MerchantID: owner.Merchant.String(), Livemode: owner.Livemode})
	if err != nil {
		return Anticipation{}, notFoundOr(err, anticipationID)
	}
	units, err := qs.AnticipationUnits(ctx, anticipationID)
	if err != nil {
		return Anticipation{}, err
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Anticipation{}, err
	}
	a := Anticipation{
		ID: row.ID, Recipient: row.RecipientID, Currency: currency, MonthlyRate: row.MonthlyRate, Amount: row.Amount, Price: row.Price,
		Automatic: row.Automatic, CreatedAt: row.CreatedAt.Time,
	}
	for _, u := range units {
		unit, err := qs.GetUnit(ctx, u.UnitID)
		if err != nil {
			return Anticipation{}, err
		}
		a.Units = append(a.Units, Anticipated{Unit: u.UnitID, SettlementDate: unit.SettlementDate.Time.Format(time.DateOnly), Amount: u.Amount, Price: u.Price, Days: int(u.Days)})
	}
	return a, nil
}

// AnticipateAutomatically anticipates, for every verified recipient that asks for it,
// every unit it has free that is at least its delay old.
func (s *Service) AnticipateAutomatically(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	done := 0
	for _, livemode := range []bool{false, true} {
		if _, err := s.registry(livemode); err != nil {
			continue
		}
		n, err := s.anticipateMode(ctx, pool, livemode)
		done += n
		if err != nil {
			return done, err
		}
	}
	return done, nil
}

func (s *Service) anticipateMode(ctx context.Context, pool *pgxpool.Pool, livemode bool) (int, error) {
	done := 0
	for after := ""; ; {
		recs, err := s.cfg.Recipients.AutoAnticipating(ctx, pool, livemode, after)
		if err != nil || len(recs) == 0 {
			return done, err
		}
		for _, rec := range recs {
			ok, err := s.anticipateAll(ctx, pool, rec)
			if err != nil {
				s.cfg.Logger.WarnContext(ctx, "an automatic anticipation failed", "recipient", rec.ID, "error", truncate(err.Error()))
			}
			if ok {
				done++
			}
			after = rec.ID.String()
		}
	}
}

func (s *Service) anticipateAll(ctx context.Context, pool *pgxpool.Pool, rec recipients.Recipient) (bool, error) {
	owner := payments.Owner{Merchant: rec.Owner.Merchant, Livemode: rec.Owner.Livemode}
	before := s.cfg.Now().AddDate(0, 0, -rec.AutoAnticipation.DelayDays)
	done := false
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		reg, err := s.registry(owner.Livemode)
		if err != nil {
			return err
		}
		free, err := s.registryFree(ctx, reg, rec.TaxID, "")
		if err != nil {
			return err
		}
		items, _, err := s.anticipable(ctx, tx, free, owner, rec, nil, before, true)
		if errors.Is(err, ErrInvalid) {
			return nil // nothing to anticipate yet
		}
		if err != nil {
			return err
		}
		rate := rules.RateFor(s.cfg.Now())
		if items, _, _, err = priced(items, rate.Monthly); err != nil {
			return err
		}
		// Named for this attempt alone: should it be undone after the registry took its
		// contracts, the daily reconciliation ends them.
		_, err = s.execute(ctx, tx, reg, owner, rec, "auto-"+AnticipationPrefix.New().String(), rate.Monthly, items, true)
		done = err == nil
		return err
	})
	return done, err
}
