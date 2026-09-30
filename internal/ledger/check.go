package ledger

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

type ViolationKind string

const (
	ViolationUnbalanced      ViolationKind = "unbalanced_transaction"
	ViolationBrokenLink      ViolationKind = "broken_link"
	ViolationBalanceDrift    ViolationKind = "balance_drift"
	ViolationClearingNotZero ViolationKind = "clearing_not_zero"
	ViolationOverdueHold     ViolationKind = "overdue_hold"
)

type Violation struct {
	Kind    ViolationKind
	Subject string
	Detail  string
}

type Report struct {
	CheckedAt  time.Time
	Violations []Violation
}

type CheckOptions struct {
	// ClearingGrace is how long a clearing account may hold a balance after its last
	// entry before it is reported.
	ClearingGrace time.Duration
	// ExpiryGrace is how long past its expiry a hold may stay unresolved before it is
	// reported; within it, the expiry job is merely behind.
	ExpiryGrace time.Duration
	// MarkDrift makes drifted accounts refuse reads and entries until repaired.
	MarkDrift bool
}

// Check verifies the ledger's invariants against one consistent snapshot: every
// transaction balances, every cached balance equals what its entries prove, clearing
// accounts return to zero and no hold outlives its expiry.
func (l *Ledger) Check(ctx context.Context, pool *pgxpool.Pool, opts CheckOptions) (Report, error) {
	report := Report{CheckedAt: l.now().UTC()}
	var drifted []string
	snapshot := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := postgres.InTxWith(ctx, pool, snapshot, func(tx pgx.Tx) error {
		report.Violations, drifted = nil, nil
		q := db.New(tx)
		checks := []func(context.Context, *db.Queries, CheckOptions, *Report) ([]string, error){
			checkUnbalanced, checkLinks, checkBalances, checkClearing, checkOverdueHolds,
		}
		for _, check := range checks {
			found, err := check(ctx, q, opts, &report)
			if err != nil {
				return err
			}
			drifted = append(drifted, found...)
		}
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("checking ledger invariants: %w", err)
	}
	if opts.MarkDrift && len(drifted) > 0 {
		if err := l.markDrifted(ctx, pool, drifted, report.CheckedAt); err != nil {
			return report, err
		}
	}
	return report, nil
}

// markDrifted marks only the accounts that still disagree once their rows are locked:
// an account repaired since the snapshot was taken is left alone.
func (l *Ledger) markDrifted(ctx context.Context, pool *pgxpool.Pool, accounts []string, at time.Time) error {
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockApplier(ctx); err != nil {
			return err
		}
		if err := q.LockBalances(ctx, accounts); err != nil {
			return err
		}
		rows, err := q.BalanceMismatches(ctx, accounts)
		if err != nil {
			return err
		}
		still := make([]string, 0, len(rows))
		for _, row := range rows {
			still = append(still, row.AccountID)
		}
		return q.MarkDrifted(ctx, db.MarkDriftedParams{Now: timestamptz(at), AccountIds: still})
	})
	if err != nil {
		return fmt.Errorf("marking drifted accounts: %w", err)
	}
	return nil
}

func checkUnbalanced(ctx context.Context, q *db.Queries, _ CheckOptions, r *Report) ([]string, error) {
	rows, err := q.UnbalancedTransactions(ctx)
	if err != nil {
		return nil, fmt.Errorf("unbalanced transactions: %w", err)
	}
	for _, row := range rows {
		r.Violations = append(r.Violations, Violation{
			Kind:    ViolationUnbalanced,
			Subject: row.ID,
			Detail:  fmt.Sprintf("declares %d entries, has %d, or does not sum to zero", row.EntryCount, row.ActualCount),
		})
	}
	return nil, nil
}

func checkLinks(ctx context.Context, q *db.Queries, _ CheckOptions, r *Report) ([]string, error) {
	rows, err := q.InconsistentLinks(ctx)
	if err != nil {
		return nil, fmt.Errorf("inconsistent links: %w", err)
	}
	for _, row := range rows {
		r.Violations = append(r.Violations, Violation{
			Kind:    ViolationBrokenLink,
			Subject: row.ID,
			Detail:  row.Kind + " does not exactly cancel the transaction it names",
		})
	}
	return nil, nil
}

func checkBalances(ctx context.Context, q *db.Queries, _ CheckOptions, r *Report) ([]string, error) {
	rows, err := q.BalanceMismatches(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("balance mismatches: %w", err)
	}
	drifted := make([]string, 0, len(rows))
	for _, row := range rows {
		drifted = append(drifted, row.AccountID)
		r.Violations = append(r.Violations, Violation{
			Kind:    ViolationBalanceDrift,
			Subject: row.AccountID,
			Detail: fmt.Sprintf("cached posted %d/%d pending %d/%d, entries prove posted %d/%d pending %d/%d",
				row.CachedPostedDebits, row.CachedPostedCredits, row.CachedPendingDebits, row.CachedPendingCredits,
				row.PostedDebits, row.PostedCredits, row.PendingDebits, row.PendingCredits),
		})
	}
	return drifted, nil
}

func checkClearing(ctx context.Context, q *db.Queries, opts CheckOptions, r *Report) ([]string, error) {
	rows, err := q.ClearingAccountsNotAtZero(ctx, timestamptz(r.CheckedAt.Add(-opts.ClearingGrace)))
	if err != nil {
		return nil, fmt.Errorf("clearing accounts: %w", err)
	}
	for _, row := range rows {
		r.Violations = append(r.Violations, Violation{
			Kind:    ViolationClearingNotZero,
			Subject: row.ID,
			Detail:  fmt.Sprintf("posted balance %d since %v", row.PostedBalance, row.LastActivity.Time),
		})
	}
	return nil, nil
}

func checkOverdueHolds(ctx context.Context, q *db.Queries, opts CheckOptions, r *Report) ([]string, error) {
	rows, err := q.OverduePendingTransfers(ctx, timestamptz(r.CheckedAt.Add(-opts.ExpiryGrace)))
	if err != nil {
		return nil, fmt.Errorf("overdue holds: %w", err)
	}
	for _, row := range rows {
		r.Violations = append(r.Violations, Violation{
			Kind:    ViolationOverdueHold,
			Subject: row.ID,
			Detail:  fmt.Sprintf("expired at %v and is still unresolved", row.ExpiresAt.Time),
		})
	}
	return nil, nil
}

// Repair resets an account's cached balance to what its entries prove and lets it be
// read and written again. The applier lock and the balance row lock keep concurrent
// writers and the applier from interleaving with the recomputation.
func (l *Ledger) Repair(ctx context.Context, pool *pgxpool.Pool, account id.ID) error {
	err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		if err := q.LockApplier(ctx); err != nil {
			return err
		}
		if _, err := q.LockBalance(ctx, account.String()); errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: %s", ErrAccountNotFound, account)
		} else if err != nil {
			return err
		}
		return q.RepairBalance(ctx, account.String())
	})
	if err != nil {
		return fmt.Errorf("repairing balance of %s: %w", account, err)
	}
	return nil
}
