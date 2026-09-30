package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger/db"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

type Balance struct {
	Account        id.ID
	Normal         Normal
	PostedDebits   money.Amount
	PostedCredits  money.Amount
	PendingDebits  money.Amount
	PendingCredits money.Amount
}

// Posted is the posted balance in the account's normal direction.
func (b Balance) Posted() (money.Amount, error) {
	if b.Normal == DebitNormal {
		return b.PostedDebits.Sub(b.PostedCredits)
	}
	return b.PostedCredits.Sub(b.PostedDebits)
}

// Available is Posted less the pending holds against the normal direction.
func (b Balance) Available() (money.Amount, error) {
	posted, err := b.Posted()
	if err != nil {
		return money.Amount{}, err
	}
	if b.Normal == DebitNormal {
		return posted.Sub(b.PendingCredits)
	}
	return posted.Sub(b.PendingDebits)
}

// Balance reads an account's balance, including batched deltas not yet applied. It
// refuses an account the invariant checker found drifted.
func (l *Ledger) Balance(ctx context.Context, q db.DBTX, account id.ID) (Balance, error) {
	row, err := db.New(q).GetBalance(ctx, account.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{}, fmt.Errorf("%w: %s", ErrAccountNotFound, account)
	}
	if err != nil {
		return Balance{}, fmt.Errorf("reading balance of %s: %w", account, err)
	}
	if row.DriftedAt.Valid {
		return Balance{}, fmt.Errorf("%w: %s since %v", ErrBalanceDrift, account, row.DriftedAt.Time)
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Balance{}, err
	}
	amount := func(minor int64) money.Amount {
		a, _ := money.New(minor, currency) // currency was validated above
		return a
	}
	return Balance{
		Account:        account,
		Normal:         Normal(row.Normal),
		PostedDebits:   amount(row.PostedDebits),
		PostedCredits:  amount(row.PostedCredits),
		PendingDebits:  amount(row.PendingDebits),
		PendingCredits: amount(row.PendingCredits),
	}, nil
}

func (l *Ledger) Transaction(ctx context.Context, q db.DBTX, transactionID id.ID) (Transaction, error) {
	t, _, err := loadTransaction(ctx, db.New(q), transactionID)
	return t, err
}

func loadTransaction(ctx context.Context, q *db.Queries, transactionID id.ID) (Transaction, []draftEntry, error) {
	row, err := q.GetTransaction(ctx, transactionID.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return Transaction{}, nil, fmt.Errorf("%w: %s", ErrTransactionNotFound, transactionID)
	}
	if err != nil {
		return Transaction{}, nil, fmt.Errorf("loading %s: %w", transactionID, err)
	}
	t := Transaction{
		ID:          transactionID,
		Kind:        Kind(row.Kind),
		ExpiresAt:   row.ExpiresAt.Time,
		Description: row.Description,
		CreatedAt:   row.CreatedAt.Time,
	}
	if t.Resolves, err = optionalTransactionID(row.ResolvesID.String, row.ResolvesID.Valid); err != nil {
		return Transaction{}, nil, err
	}
	if t.Reverses, err = optionalTransactionID(row.ReversesID.String, row.ReversesID.Valid); err != nil {
		return Transaction{}, nil, err
	}

	rows, err := q.GetEntries(ctx, transactionID.String())
	if err != nil {
		return Transaction{}, nil, fmt.Errorf("loading entries of %s: %w", transactionID, err)
	}
	ids := make([]id.ID, 0, len(rows))
	for _, r := range rows {
		accountID, parseErr := AccountPrefix.Parse(r.AccountID)
		if parseErr != nil {
			return Transaction{}, nil, fmt.Errorf("stored entry account: %w", parseErr)
		}
		ids = append(ids, accountID)
	}
	accounts, err := loadAccounts(ctx, q, ids)
	if err != nil {
		return Transaction{}, nil, err
	}
	entries := make([]draftEntry, len(rows))
	for i, r := range rows {
		account := accounts[ids[i]]
		entries[i] = draftEntry{account: account, layer: Layer(r.Layer), amount: r.Amount}
		amount, err := money.New(r.Amount, account.Currency)
		if err != nil {
			return Transaction{}, nil, err
		}
		t.Entries = append(t.Entries, Entry{Account: account.ID, Layer: entries[i].layer, Amount: amount})
	}
	return t, entries, nil
}

func optionalTransactionID(s string, valid bool) (id.ID, error) {
	if !valid {
		return id.ID{}, nil
	}
	return TransactionPrefix.Parse(s)
}

// ApplyQueued applies up to limit queued deltas to batched accounts' cached balances
// and returns how many it applied. Only one applier runs at a time, because deltas must
// be applied in entry order: a release applied before its hold would take a cached
// pending balance below zero.
func (l *Ledger) ApplyQueued(ctx context.Context, pool *pgxpool.Pool, limit int32) (int64, error) {
	var applied int64
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		locked, err := q.TryApplierLock(ctx)
		if err != nil || !locked {
			return err
		}
		applied, err = q.ApplyQueuedDeltas(ctx, limit)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("applying queued balance deltas: %w", err)
	}
	return applied, nil
}

// ExpireDue voids up to limit pending transfers whose expiry has passed, each in its own
// transaction, and returns how many it expired along with every failure. A transfer
// resolved concurrently is skipped.
func (l *Ledger) ExpireDue(ctx context.Context, pool *pgxpool.Pool, limit int32) (int, error) {
	due, err := db.New(pool).DuePendingTransfers(ctx, db.DuePendingTransfersParams{
		Now:      timestamptz(l.now()),
		MaxCount: limit,
	})
	if err != nil {
		return 0, fmt.Errorf("finding due pending transfers: %w", err)
	}
	expired := 0
	var failures []error
	for _, raw := range due {
		pendingID, err := TransactionPrefix.Parse(raw)
		if err != nil {
			failures = append(failures, fmt.Errorf("stored transaction id: %w", err))
			continue
		}
		err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, expireErr := l.expire(ctx, tx, pendingID)
			return expireErr
		})
		switch {
		case errors.Is(err, ErrAlreadyResolved):
		case err != nil:
			// One hold that cannot expire, such as one on a drifted account, must not
			// block every hold queued behind it.
			failures = append(failures, fmt.Errorf("expiring %s: %w", pendingID, err))
		default:
			expired++
		}
	}
	return expired, errors.Join(failures...)
}
