//go:build integration

package ledger_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

func TestPostMovesPostedBalances(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()

	txn := f.mustPost(ledger.Debit(cash.ID, brl(t, 60000)), ledger.Credit(wallet.ID, brl(t, 60000)))

	if txn.Kind != ledger.KindPosted || len(txn.Entries) != 2 {
		t.Fatalf("transaction = %+v", txn)
	}
	if got := f.fields(cash); got != [4]int64{60000, 0, 0, 0} {
		t.Errorf("cash = %v", got)
	}
	if got := f.fields(wallet); got != [4]int64{0, 60000, 0, 0} {
		t.Errorf("wallet = %v", got)
	}
	posted, err := f.balance(wallet).Posted()
	if err != nil || posted.Minor() != 60000 {
		t.Errorf("wallet Posted() = %v, %v", posted, err)
	}
	f.requireClean()
}

func TestPostRejectsInvalidPostings(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()
	own := f.account(ledger.AccountSpec{Book: ledger.OwnFunds, Code: "revenue", Normal: ledger.CreditNormal})
	usd, err := money.New(100, money.USD)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		legs []ledger.Leg
		want error
	}{
		{"one leg", []ledger.Leg{ledger.Debit(cash.ID, brl(t, 1))}, ledger.ErrInvalid},
		{"unbalanced", []ledger.Leg{ledger.Debit(cash.ID, brl(t, 2)), ledger.Credit(wallet.ID, brl(t, 1))}, ledger.ErrUnbalanced},
		{"zero amount", []ledger.Leg{ledger.Debit(cash.ID, brl(t, 0)), ledger.Credit(wallet.ID, brl(t, 0))}, ledger.ErrInvalid},
		{"wrong currency", []ledger.Leg{ledger.Debit(cash.ID, usd), ledger.Credit(wallet.ID, usd)}, ledger.ErrInvalid},
		{"unknown account", []ledger.Leg{ledger.Debit(cash.ID, brl(t, 1)), ledger.Credit(ledger.AccountPrefix.New(), brl(t, 1))}, ledger.ErrAccountNotFound},
		// Balanced overall, but each book on its own is not: client funds cannot pay
		// Jupiter's revenue without passing through both books.
		{"crosses books", []ledger.Leg{ledger.Debit(cash.ID, brl(t, 5)), ledger.Credit(own.ID, brl(t, 5))}, ledger.ErrUnbalanced},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := f.post(tt.legs...); !errors.Is(err, tt.want) {
				t.Fatalf("Post error = %v, want %v", err, tt.want)
			}
		})
	}
	f.requireClean()
}

func TestNonNegativeAccountRefusesAnOverdraft(t *testing.T) {
	f := newFixture(t)
	cash, wallet, other := f.cash(), f.wallet(), f.wallet()
	f.mustPost(ledger.Debit(cash.ID, brl(t, 1000)), ledger.Credit(wallet.ID, brl(t, 1000)))

	if _, err := f.post(ledger.Debit(wallet.ID, brl(t, 1001)), ledger.Credit(other.ID, brl(t, 1001))); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("overdraft error = %v, want ErrInsufficientBalance", err)
	}
	f.mustPost(ledger.Debit(wallet.ID, brl(t, 1000)), ledger.Credit(other.ID, brl(t, 1000)))
	if got := f.fields(wallet); got != [4]int64{1000, 1000, 0, 0} {
		t.Errorf("wallet = %v", got)
	}
	f.requireClean()
}

func (f *fixture) hold(debit, credit ledger.Account, minor int64, expiresAt time.Time) (ledger.Transaction, error) {
	return f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Hold(f.t.Context(), tx, ledger.Hold{
			Debit: debit.ID, Credit: credit.ID, Amount: brl(f.t, minor), ExpiresAt: expiresAt,
		})
	})
}

func TestHoldCountsAgainstTheBalanceUntilPosted(t *testing.T) {
	f := newFixture(t)
	cash, wallet, merchant := f.cash(), f.wallet(), f.wallet()
	f.mustPost(ledger.Debit(cash.ID, brl(t, 1000)), ledger.Credit(wallet.ID, brl(t, 1000)))

	hold, err := f.hold(wallet, merchant, 700, time.Time{})
	if err != nil {
		t.Fatalf("Hold: %v", err)
	}
	if available, _ := f.balance(wallet).Available(); available.Minor() != 300 {
		t.Errorf("available after hold = %d, want 300", available.Minor())
	}
	if _, err := f.hold(wallet, merchant, 301, time.Time{}); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Errorf("second hold error = %v, want ErrInsufficientBalance", err)
	}
	// A pending credit does not count for the merchant yet.
	if available, _ := f.balance(merchant).Available(); available.Minor() != 0 {
		t.Errorf("merchant available = %d, want 0", available.Minor())
	}

	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.PostPending(t.Context(), tx, hold.ID, brl(t, 500))
	}); err != nil {
		t.Fatalf("PostPending: %v", err)
	}
	// The partial capture releases the whole hold: 200 goes back to the wallet.
	if got := f.fields(wallet); got != [4]int64{500, 1000, 0, 0} {
		t.Errorf("wallet = %v", got)
	}
	if got := f.fields(merchant); got != [4]int64{0, 500, 0, 0} {
		t.Errorf("merchant = %v", got)
	}
	f.requireClean()
}

func TestPostPendingRejectsMoreThanTheHold(t *testing.T) {
	f := newFixture(t)
	cash, merchant := f.cash(), f.wallet()
	hold, err := f.hold(cash, merchant, 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	for _, minor := range []int64{0, 101} {
		if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
			return f.ledger.PostPending(t.Context(), tx, hold.ID, brl(t, minor))
		}); !errors.Is(err, ledger.ErrInvalid) {
			t.Errorf("PostPending(%d) error = %v, want ErrInvalid", minor, err)
		}
	}
}

func TestAPendingTransferIsResolvedOnce(t *testing.T) {
	f := newFixture(t)
	cash, merchant := f.cash(), f.wallet()
	hold, err := f.hold(cash, merchant, 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) { return f.ledger.Void(t.Context(), tx, hold.ID) }); err != nil {
		t.Fatalf("Void: %v", err)
	}
	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.PostPending(t.Context(), tx, hold.ID, brl(t, 100))
	}); !errors.Is(err, ledger.ErrAlreadyResolved) {
		t.Fatalf("PostPending after Void error = %v, want ErrAlreadyResolved", err)
	}
	if got := f.fields(merchant); got != [4]int64{0, 0, 0, 0} {
		t.Errorf("merchant = %v", got)
	}
	f.requireClean()
}

// A capture and a void racing for one hold: the database's unique index lets exactly one
// through, whichever order they commit in.
func TestConcurrentResolutionsLetExactlyOneThrough(t *testing.T) {
	f := newFixture(t)
	cash, merchant := f.cash(), f.wallet()
	for range 20 {
		hold, err := f.hold(cash, merchant, 100, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		resolve := []func(pgx.Tx) (ledger.Transaction, error){
			func(tx pgx.Tx) (ledger.Transaction, error) { return f.ledger.Void(t.Context(), tx, hold.ID) },
			func(tx pgx.Tx) (ledger.Transaction, error) {
				return f.ledger.PostPending(t.Context(), tx, hold.ID, brl(t, 100))
			},
		}
		for i, fn := range resolve {
			wg.Go(func() {
				<-start
				_, errs[i] = f.tx(fn)
			})
		}
		close(start)
		wg.Wait()
		succeeded := 0
		for _, err := range errs {
			switch {
			case err == nil:
				succeeded++
			case !errors.Is(err, ledger.ErrAlreadyResolved):
				t.Fatalf("unexpected error: %v", err)
			}
		}
		if succeeded != 1 {
			t.Fatalf("%d resolutions succeeded, want exactly 1: %v", succeeded, errs)
		}
	}
	f.requireClean()
}

func TestExpiry(t *testing.T) {
	f := newFixture(t)
	cash, merchant := f.cash(), f.wallet()
	hold, err := f.hold(cash, merchant, 100, f.clock.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.ledger.ExpireDue(t.Context(), f.pool, 10); err != nil || n != 0 {
		t.Fatalf("ExpireDue before expiry = %d, %v; want 0", n, err)
	}

	f.clock.Advance(time.Hour)
	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.PostPending(t.Context(), tx, hold.ID, brl(t, 100))
	}); !errors.Is(err, ledger.ErrPendingExpired) {
		t.Fatalf("PostPending after expiry error = %v, want ErrPendingExpired", err)
	}
	if n, err := f.ledger.ExpireDue(t.Context(), f.pool, 10); err != nil || n != 1 {
		t.Fatalf("ExpireDue = %d, %v; want 1", n, err)
	}
	if got := f.fields(cash); got != [4]int64{0, 0, 0, 0} {
		t.Errorf("cash after expiry = %v", got)
	}
	f.requireClean()
}

func TestAnOverdueHoldIsReported(t *testing.T) {
	f := newFixture(t)
	hold, err := f.hold(f.cash(), f.wallet(), 100, f.clock.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * time.Hour)
	report, err := f.ledger.Check(t.Context(), f.pool, ledger.CheckOptions{ExpiryGrace: time.Hour, ClearingGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Kind != ledger.ViolationOverdueHold ||
		report.Violations[0].Subject != hold.ID.String() {
		t.Fatalf("violations = %+v, want one overdue hold", report.Violations)
	}
}

func TestReverse(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()
	original := f.mustPost(ledger.Debit(cash.ID, brl(t, 250)), ledger.Credit(wallet.ID, brl(t, 250)))

	reversal, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Reverse(t.Context(), tx, original.ID, "mistake")
	})
	if err != nil {
		t.Fatalf("Reverse: %v", err)
	}
	if reversal.Reverses != original.ID {
		t.Errorf("Reverses = %v, want %v", reversal.Reverses, original.ID)
	}
	if posted, _ := f.balance(wallet).Posted(); !posted.IsZero() {
		t.Errorf("wallet after reversal = %v", posted)
	}
	// History is kept: both sides grew, nothing was deleted.
	if got := f.fields(wallet); got != [4]int64{250, 250, 0, 0} {
		t.Errorf("wallet = %v", got)
	}
	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Reverse(t.Context(), tx, original.ID, "again")
	}); !errors.Is(err, ledger.ErrAlreadyReversed) {
		t.Errorf("second Reverse error = %v, want ErrAlreadyReversed", err)
	}
	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Reverse(t.Context(), tx, reversal.ID, "reverse the reversal")
	}); !errors.Is(err, ledger.ErrNotReversible) {
		t.Errorf("reversing a reversal error = %v, want ErrNotReversible", err)
	}
	f.requireClean()
}

func TestAReversalThatWouldOverdrawIsRefused(t *testing.T) {
	f := newFixture(t)
	cash, wallet, other := f.cash(), f.wallet(), f.wallet()
	funding := f.mustPost(ledger.Debit(cash.ID, brl(t, 100)), ledger.Credit(wallet.ID, brl(t, 100)))
	f.mustPost(ledger.Debit(wallet.ID, brl(t, 100)), ledger.Credit(other.ID, brl(t, 100)))

	if _, err := f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Reverse(t.Context(), tx, funding.ID, "too late")
	}); !errors.Is(err, ledger.ErrInsufficientBalance) {
		t.Fatalf("Reverse error = %v, want ErrInsufficientBalance", err)
	}
}

func TestBatchedAccountBalanceIncludesQueuedDeltas(t *testing.T) {
	f := newFixture(t)
	cash := f.cash()
	platform := f.account(ledger.AccountSpec{Code: "platform", Normal: ledger.CreditNormal, Batched: true})

	for range 5 {
		f.mustPost(ledger.Debit(cash.ID, brl(t, 10)), ledger.Credit(platform.ID, brl(t, 10)))
	}
	if got := f.fields(platform); got != [4]int64{0, 50, 0, 0} {
		t.Errorf("platform before applying = %v", got)
	}
	f.requireClean()

	applied, err := f.ledger.ApplyQueued(t.Context(), f.pool, 1000)
	if err != nil || applied != 5 {
		t.Fatalf("ApplyQueued = %d, %v; want 5", applied, err)
	}
	if got := f.fields(platform); got != [4]int64{0, 50, 0, 0} {
		t.Errorf("platform after applying = %v", got)
	}
	f.requireClean()
}

func TestDriftIsDetectedAndRefusesReadsAndWritesUntilRepaired(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()
	f.mustPost(ledger.Debit(cash.ID, brl(t, 100)), ledger.Credit(wallet.ID, brl(t, 100)))

	// Something outside the ledger corrupts the cache.
	if _, err := f.pool.Exec(t.Context(), "UPDATE ledger.balances SET posted_credits = 999 WHERE account_id = $1", wallet.ID.String()); err != nil {
		t.Fatal(err)
	}
	report, err := f.ledger.Check(t.Context(), f.pool, ledger.CheckOptions{MarkDrift: true, ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Kind != ledger.ViolationBalanceDrift {
		t.Fatalf("violations = %+v, want one balance drift", report.Violations)
	}
	if _, err := f.ledger.Balance(t.Context(), f.pool, wallet.ID); !errors.Is(err, ledger.ErrBalanceDrift) {
		t.Fatalf("Balance error = %v, want ErrBalanceDrift", err)
	}
	if _, err := f.post(ledger.Debit(cash.ID, brl(t, 1)), ledger.Credit(wallet.ID, brl(t, 1))); !errors.Is(err, ledger.ErrBalanceDrift) {
		t.Fatalf("Post error = %v, want ErrBalanceDrift", err)
	}

	if err := f.ledger.Repair(t.Context(), f.pool, wallet.ID); err != nil {
		t.Fatalf("Repair: %v", err)
	}
	if got := f.fields(wallet); got != [4]int64{0, 100, 0, 0} {
		t.Errorf("wallet after repair = %v", got)
	}
	f.mustPost(ledger.Debit(cash.ID, brl(t, 1)), ledger.Credit(wallet.ID, brl(t, 1)))
	f.requireClean()
}

func TestAClearingAccountThatDoesNotReturnToZeroIsReported(t *testing.T) {
	f := newFixture(t)
	cash, settled, unsettled := f.cash(),
		f.account(ledger.AccountSpec{Code: "clearing", Normal: ledger.DebitNormal, Clearing: true}),
		f.account(ledger.AccountSpec{Code: "clearing", Normal: ledger.DebitNormal, Clearing: true})

	f.mustPost(ledger.Debit(settled.ID, brl(t, 100)), ledger.Credit(cash.ID, brl(t, 100)))
	f.mustPost(ledger.Debit(cash.ID, brl(t, 100)), ledger.Credit(settled.ID, brl(t, 100)))
	f.mustPost(ledger.Debit(unsettled.ID, brl(t, 100)), ledger.Credit(cash.ID, brl(t, 100)))
	f.clock.Advance(2 * time.Hour)

	report, err := f.ledger.Check(t.Context(), f.pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Subject != unsettled.ID.String() {
		t.Fatalf("violations = %+v, want only the unsettled clearing account", report.Violations)
	}
}

func TestLedgerTablesAreAppendOnly(t *testing.T) {
	f := newFixture(t)
	f.mustPost(ledger.Debit(f.cash().ID, brl(t, 1)), ledger.Credit(f.wallet().ID, brl(t, 1)))

	for _, sql := range []string{
		"UPDATE ledger.entries SET amount = amount * 2",
		"DELETE FROM ledger.entries",
		"TRUNCATE ledger.entries CASCADE",
		"UPDATE ledger.transactions SET description = 'rewritten'",
		"DELETE FROM ledger.transactions",
		"UPDATE ledger.accounts SET normal = 'debit'",
		"DELETE FROM ledger.accounts",
	} {
		_, err := f.pool.Exec(t.Context(), sql)
		if code := postgres.ErrorCode(err); code != "JL003" {
			t.Errorf("%s: error %v (code %q), want JL003", sql, err, code)
		}
	}
}

func TestTheDatabaseRefusesUnbalancedEntriesWrittenDirectly(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()
	original := f.mustPost(ledger.Debit(cash.ID, brl(t, 1)), ledger.Credit(wallet.ID, brl(t, 1)))

	insertTxn := `INSERT INTO ledger.transactions (id, kind, entry_count, description, created_at)
	              VALUES ($1, 'posted', 2, 'raw', now())`
	insertEntry := `INSERT INTO ledger.entries (transaction_id, transaction_kind, account_id, book, currency, layer, amount)
	                VALUES ($1, 'posted', $2, 'client_funds', 'BRL', 'posted', $3)`

	tests := []struct {
		name string
		run  func(pgx.Tx) error
	}{
		{"entries that do not sum to zero", func(tx pgx.Tx) error {
			txnID := ledger.TransactionPrefix.New().String()
			if _, err := tx.Exec(t.Context(), insertTxn, txnID); err != nil {
				return err
			}
			if _, err := tx.Exec(t.Context(), insertEntry, txnID, cash.ID.String(), 5); err != nil {
				return err
			}
			_, err := tx.Exec(t.Context(), insertEntry, txnID, wallet.ID.String(), -4)
			return err
		}},
		{"balanced entries appended to a committed transaction", func(tx pgx.Tx) error {
			if _, err := tx.Exec(t.Context(), insertEntry, original.ID.String(), cash.ID.String(), 5); err != nil {
				return err
			}
			_, err := tx.Exec(t.Context(), insertEntry, original.ID.String(), wallet.ID.String(), -5)
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := pgx.BeginTxFunc(t.Context(), f.pool, pgx.TxOptions{}, tt.run)
			if code := postgres.ErrorCode(err); code != "JL002" {
				t.Fatalf("commit error = %v (code %q), want JL002", err, code)
			}
		})
	}
	f.requireClean()
}
