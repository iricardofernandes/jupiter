package ledger

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger/db"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

type draft struct {
	kind        Kind
	resolves    id.ID
	reverses    id.ID
	expiresAt   pgtype.Timestamptz
	description string
	entries     []draftEntry
}

type draftEntry struct {
	account Account
	layer   Layer
	amount  int64
}

func (l *Ledger) Post(ctx context.Context, tx pgx.Tx, p Posting) (Transaction, error) {
	if len(p.Legs) < 2 {
		return Transaction{}, fmt.Errorf("%w: a posting needs at least two legs", ErrInvalid)
	}
	q := db.New(tx)
	ids := make([]id.ID, 0, len(p.Legs))
	for _, leg := range p.Legs {
		ids = append(ids, leg.Account)
	}
	accounts, err := loadAccounts(ctx, q, ids)
	if err != nil {
		return Transaction{}, err
	}
	d := draft{kind: KindPosted, description: p.Description}
	for _, leg := range p.Legs {
		e, err := legEntry(leg, accounts[leg.Account])
		if err != nil {
			return Transaction{}, err
		}
		d.entries = append(d.entries, e)
	}
	return l.insert(ctx, q, d)
}

func legEntry(leg Leg, account Account) (draftEntry, error) {
	if !leg.Amount.IsPositive() {
		return draftEntry{}, fmt.Errorf("%w: leg amount %v is not positive", ErrInvalid, leg.Amount)
	}
	if leg.Amount.Currency() != account.Currency {
		return draftEntry{}, fmt.Errorf("%w: leg in %v on account %s in %v",
			ErrInvalid, leg.Amount.Currency(), account.ID, account.Currency)
	}
	amount := leg.Amount.Minor()
	switch leg.side {
	case debit:
	case credit:
		amount = -amount
	default:
		return draftEntry{}, fmt.Errorf("%w: leg built without Debit or Credit", ErrInvalid)
	}
	return draftEntry{account: account, layer: LayerPosted, amount: amount}, nil
}

func (l *Ledger) Hold(ctx context.Context, tx pgx.Tx, h Hold) (Transaction, error) {
	if h.Debit == h.Credit {
		return Transaction{}, fmt.Errorf("%w: a hold needs two different accounts", ErrInvalid)
	}
	if !h.ExpiresAt.IsZero() && !h.ExpiresAt.After(l.now()) {
		return Transaction{}, fmt.Errorf("%w: hold expires at %v, which is not in the future", ErrInvalid, h.ExpiresAt)
	}
	q := db.New(tx)
	accounts, err := loadAccounts(ctx, q, []id.ID{h.Debit, h.Credit})
	if err != nil {
		return Transaction{}, err
	}
	debitEntry, err := legEntry(Debit(h.Debit, h.Amount), accounts[h.Debit])
	if err != nil {
		return Transaction{}, err
	}
	creditEntry, err := legEntry(Credit(h.Credit, h.Amount), accounts[h.Credit])
	if err != nil {
		return Transaction{}, err
	}
	debitEntry.layer, creditEntry.layer = LayerPending, LayerPending
	return l.insert(ctx, q, draft{
		kind:        KindPending,
		expiresAt:   timestamptz(h.ExpiresAt.UTC()),
		description: h.Description,
		entries:     []draftEntry{debitEntry, creditEntry},
	})
}

// PostPending posts amount, at most the held amount, of a pending transfer and releases
// the whole hold.
func (l *Ledger) PostPending(ctx context.Context, tx pgx.Tx, pendingID id.ID, amount money.Amount) (Transaction, error) {
	q := db.New(tx)
	hold, err := l.openHold(ctx, q, pendingID)
	if err != nil {
		return Transaction{}, err
	}
	if hold.expired(l) {
		return Transaction{}, fmt.Errorf("%w: %s", ErrPendingExpired, pendingID)
	}
	if amount.Currency() != hold.debit.account.Currency || !amount.IsPositive() || amount.Minor() > hold.amount {
		return Transaction{}, fmt.Errorf("%w: cannot post %v of a %d hold", ErrInvalid, amount, hold.amount)
	}
	entries := append(hold.release(),
		draftEntry{account: hold.debit.account, layer: LayerPosted, amount: amount.Minor()},
		draftEntry{account: hold.credit.account, layer: LayerPosted, amount: -amount.Minor()},
	)
	return l.insert(ctx, q, draft{kind: KindPostPending, resolves: pendingID, entries: entries})
}

func (l *Ledger) Void(ctx context.Context, tx pgx.Tx, pendingID id.ID) (Transaction, error) {
	q := db.New(tx)
	hold, err := l.openHold(ctx, q, pendingID)
	if err != nil {
		return Transaction{}, err
	}
	return l.insert(ctx, q, draft{kind: KindVoidPending, resolves: pendingID, entries: hold.release()})
}

func (l *Ledger) expire(ctx context.Context, tx pgx.Tx, pendingID id.ID) (Transaction, error) {
	q := db.New(tx)
	hold, err := l.openHold(ctx, q, pendingID)
	if err != nil {
		return Transaction{}, err
	}
	if !hold.expired(l) {
		return Transaction{}, fmt.Errorf("%w: %s", ErrNotYetExpired, pendingID)
	}
	return l.insert(ctx, q, draft{kind: KindExpirePending, resolves: pendingID, entries: hold.release()})
}

func (l *Ledger) Reverse(ctx context.Context, tx pgx.Tx, transactionID id.ID, description string) (Transaction, error) {
	q := db.New(tx)
	original, entries, err := loadTransaction(ctx, q, transactionID)
	if err != nil {
		return Transaction{}, err
	}
	if original.Kind != KindPosted && original.Kind != KindPostPending {
		return Transaction{}, fmt.Errorf("%w: %s is a %s transaction", ErrNotReversible, transactionID, original.Kind)
	}
	d := draft{kind: KindReversal, reverses: transactionID, description: description}
	for _, e := range entries {
		if e.layer == LayerPosted {
			d.entries = append(d.entries, draftEntry{account: e.account, layer: LayerPosted, amount: -e.amount})
		}
	}
	return l.insert(ctx, q, d)
}

type openHold struct {
	expiresAt     pgtype.Timestamptz
	debit, credit draftEntry
	amount        int64
}

func (h openHold) expired(l *Ledger) bool {
	return h.expiresAt.Valid && !l.now().Before(h.expiresAt.Time)
}

func (h openHold) release() []draftEntry {
	return []draftEntry{
		{account: h.debit.account, layer: LayerPending, amount: -h.amount},
		{account: h.credit.account, layer: LayerPending, amount: h.amount},
	}
}

func (l *Ledger) openHold(ctx context.Context, q *db.Queries, pendingID id.ID) (openHold, error) {
	pending, entries, err := loadTransaction(ctx, q, pendingID)
	if err != nil {
		return openHold{}, err
	}
	if pending.Kind != KindPending {
		return openHold{}, fmt.Errorf("%w: %s is a %s transaction", ErrNotPending, pendingID, pending.Kind)
	}
	if _, err := q.GetResolution(ctx, optionalID(pendingID)); err == nil {
		return openHold{}, fmt.Errorf("%w: %s", ErrAlreadyResolved, pendingID)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return openHold{}, fmt.Errorf("checking resolution of %s: %w", pendingID, err)
	}
	var hold openHold
	for _, e := range entries {
		if e.amount > 0 {
			hold.debit, hold.amount = e, e.amount
		} else {
			hold.credit = e
		}
	}
	hold.expiresAt = pgtype.Timestamptz{Time: pending.ExpiresAt, Valid: !pending.ExpiresAt.IsZero()}
	return hold, nil
}

func (l *Ledger) insert(ctx context.Context, q *db.Queries, d draft) (Transaction, error) {
	if err := checkBalanced(d.entries); err != nil {
		return Transaction{}, err
	}
	sortForLocking(d.entries)

	t := Transaction{
		ID:          TransactionPrefix.New(),
		Kind:        d.kind,
		Resolves:    d.resolves,
		Reverses:    d.reverses,
		ExpiresAt:   d.expiresAt.Time,
		Description: d.description,
		CreatedAt:   l.now().UTC(),
	}
	err := q.InsertTransaction(ctx, db.InsertTransactionParams{
		ID:          t.ID.String(),
		Kind:        string(t.Kind),
		ResolvesID:  optionalID(t.Resolves),
		ReversesID:  optionalID(t.Reverses),
		ExpiresAt:   d.expiresAt,
		EntryCount:  int32(len(d.entries)), //nolint:gosec // a transaction has a handful of entries
		Description: t.Description,
		CreatedAt:   timestamptz(t.CreatedAt),
	})
	if err != nil {
		return Transaction{}, translate(fmt.Errorf("inserting transaction: %w", err))
	}

	params := db.InsertEntriesParams{TransactionID: t.ID.String(), TransactionKind: string(t.Kind)}
	for _, e := range d.entries {
		params.AccountIds = append(params.AccountIds, e.account.ID.String())
		params.Books = append(params.Books, string(e.account.Book))
		params.Currencies = append(params.Currencies, e.account.Currency.Code())
		params.Layers = append(params.Layers, string(e.layer))
		params.Amounts = append(params.Amounts, e.amount)
		amount, err := money.New(e.amount, e.account.Currency)
		if err != nil {
			return Transaction{}, err
		}
		t.Entries = append(t.Entries, Entry{Account: e.account.ID, Layer: e.layer, Amount: amount})
	}
	if err := q.InsertEntries(ctx, params); err != nil {
		return Transaction{}, translate(fmt.Errorf("inserting entries of %s: %w", t.ID, err))
	}
	return t, nil
}

// checkBalanced is the database's zero-sum rule, checked first for a clear error. The
// database's own check runs at commit, outside any ledger call, as a backstop.
func checkBalanced(entries []draftEntry) error {
	type key struct {
		book     Book
		currency money.Currency
		layer    Layer
	}
	sums := map[key]money.Amount{}
	for _, e := range entries {
		k := key{e.account.Book, e.account.Currency, e.layer}
		amount, err := money.New(e.amount, e.account.Currency)
		if err != nil {
			return err
		}
		sum, ok := sums[k]
		if !ok {
			sums[k] = amount
			continue
		}
		if sums[k], err = sum.Add(amount); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}
	for k, sum := range sums {
		if !sum.IsZero() {
			return fmt.Errorf("%w: %s %s entries in %s sum to %v", ErrUnbalanced, k.layer, k.book, k.currency, sum)
		}
	}
	return nil
}

// sortForLocking orders entries so that every transaction locks balance rows in the
// same order (by account), which rules out deadlocks between ledger writes. Within one
// account, pending entries come before posted ones and entries that raise the normal
// balance before those that lower it, so the non-negative check never sees an
// intermediate state that the whole transaction would not produce.
func sortForLocking(entries []draftEntry) {
	slices.SortStableFunc(entries, func(a, b draftEntry) int {
		if c := cmp.Compare(a.account.ID.String(), b.account.ID.String()); c != 0 {
			return c
		}
		if a.layer != b.layer {
			if a.layer == LayerPending {
				return -1
			}
			return 1
		}
		return cmp.Compare(lowersNormal(a), lowersNormal(b))
	})
}

func lowersNormal(e draftEntry) int {
	raisesNormal := (e.amount > 0) == (e.account.Normal == DebitNormal)
	if raisesNormal {
		return 0
	}
	return 1
}

func optionalID(i id.ID) pgtype.Text {
	return pgtype.Text{String: i.String(), Valid: !i.IsZero()}
}

func translate(err error) error {
	switch postgres.ErrorCode(err) {
	case "23514":
		if postgres.ConstraintName(err) == "balances_non_negative" {
			return fmt.Errorf("%w: %w", ErrInsufficientBalance, err)
		}
	case "23505":
		switch postgres.ConstraintName(err) {
		case "transactions_resolves_once":
			return fmt.Errorf("%w: %w", ErrAlreadyResolved, err)
		case "transactions_reversed_once":
			return fmt.Errorf("%w: %w", ErrAlreadyReversed, err)
		}
	case "JL001":
		return fmt.Errorf("%w: %w", ErrBalanceDrift, err)
	}
	return err
}
