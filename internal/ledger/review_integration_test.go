//go:build integration

package ledger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

func (f *fixture) corrupt(a ledger.Account) {
	f.t.Helper()
	if _, err := f.pool.Exec(f.t.Context(), "UPDATE ledger.balances SET posted_credits = posted_credits + 999 WHERE account_id = $1", a.ID.String()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) markDrift() ledger.Report {
	f.t.Helper()
	report, err := f.ledger.Check(f.t.Context(), f.pool, ledger.CheckOptions{MarkDrift: true, ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil {
		f.t.Fatal(err)
	}
	return report
}

func TestExpiryContinuesPastAHoldThatCannotExpire(t *testing.T) {
	f := newFixture(t)
	cash, stuck, healthy := f.cash(), f.wallet(), f.wallet()
	soon := f.clock.Now().Add(time.Minute)
	if _, err := f.hold(cash, stuck, 100, soon); err != nil {
		t.Fatal(err)
	}
	if _, err := f.hold(cash, healthy, 100, soon.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	f.corrupt(stuck)
	f.markDrift()
	f.clock.Advance(time.Hour)

	n, err := f.ledger.ExpireDue(t.Context(), f.pool, 10)
	if n != 1 {
		t.Fatalf("expired %d, want 1: the healthy hold must not wait behind the stuck one", n)
	}
	if !errors.Is(err, ledger.ErrBalanceDrift) {
		t.Fatalf("ExpireDue error = %v, want the stuck hold's ErrBalanceDrift", err)
	}
	if got := f.fields(healthy); got != [4]int64{0, 0, 0, 0} {
		t.Errorf("healthy account after expiry = %v", got)
	}
}

func TestADriftedBatchedAccountRefusesEntries(t *testing.T) {
	f := newFixture(t)
	cash := f.cash()
	platform := f.account(ledger.AccountSpec{Code: "platform", Normal: ledger.CreditNormal, Batched: true})
	f.mustPost(ledger.Debit(cash.ID, brl(t, 10)), ledger.Credit(platform.ID, brl(t, 10)))
	f.corrupt(platform)
	f.markDrift()

	if _, err := f.post(ledger.Debit(cash.ID, brl(t, 10)), ledger.Credit(platform.ID, brl(t, 10))); !errors.Is(err, ledger.ErrBalanceDrift) {
		t.Fatalf("Post to a drifted batched account error = %v, want ErrBalanceDrift", err)
	}
}

func TestDriftIsReportedAsDriftNotAsInsufficientBalance(t *testing.T) {
	f := newFixture(t)
	cash, wallet, other := f.cash(), f.wallet(), f.wallet()
	f.mustPost(ledger.Debit(cash.ID, brl(t, 100)), ledger.Credit(wallet.ID, brl(t, 100)))
	if _, err := f.pool.Exec(t.Context(), "UPDATE ledger.balances SET posted_credits = 0 WHERE account_id = $1", wallet.ID.String()); err != nil {
		t.Fatal(err)
	}
	f.markDrift()

	if _, err := f.post(ledger.Debit(wallet.ID, brl(t, 50)), ledger.Credit(other.ID, brl(t, 50))); !errors.Is(err, ledger.ErrBalanceDrift) {
		t.Fatalf("error = %v, want ErrBalanceDrift", err)
	}
}

// Check takes its snapshot, then marks. An account repaired in between must stay usable.
func TestAnAccountRepairedAfterTheSnapshotIsNotMarkedAgain(t *testing.T) {
	f := newFixture(t)
	cash, wallet := f.cash(), f.wallet()
	f.mustPost(ledger.Debit(cash.ID, brl(t, 100)), ledger.Credit(wallet.ID, brl(t, 100)))
	f.corrupt(wallet)
	if err := f.ledger.Repair(t.Context(), f.pool, wallet.ID); err != nil {
		t.Fatal(err)
	}

	if err := ledger.MarkDrifted(f.ledger, t.Context(), f.pool, []string{wallet.ID.String()}, f.clock.Now()); err != nil {
		t.Fatalf("markDrifted: %v", err)
	}
	if _, err := f.ledger.Balance(t.Context(), f.pool, wallet.ID); err != nil {
		t.Fatalf("Balance after a stale mark = %v, want the repaired account readable", err)
	}
}

func TestRepairOfAnUnknownAccount(t *testing.T) {
	f := newFixture(t)
	if err := f.ledger.Repair(t.Context(), f.pool, ledger.AccountPrefix.New()); !errors.Is(err, ledger.ErrAccountNotFound) {
		t.Fatalf("Repair error = %v, want ErrAccountNotFound", err)
	}
}

func TestAResolutionMustNameAPendingTransaction(t *testing.T) {
	f := newFixture(t)
	posted := f.mustPost(ledger.Debit(f.cash().ID, brl(t, 1)), ledger.Credit(f.wallet().ID, brl(t, 1)))
	_, err := f.pool.Exec(t.Context(), `INSERT INTO ledger.transactions (id, kind, resolves_id, entry_count, description, created_at)
		VALUES ($1, 'void_pending', $2, 2, 'raw', now())`, ledger.TransactionPrefix.New().String(), posted.ID.String())
	if code := postgres.ErrorCode(err); code != "23503" {
		t.Fatalf("resolving a posted transaction: error %v (code %q), want a foreign key violation", err, code)
	}
}

// A void written around the ledger API that releases more than its hold balances and
// matches the cache, so only the link check can see it.
func TestAResolutionThatDoesNotCancelItsHoldIsReported(t *testing.T) {
	f := newFixture(t)
	cash, merchant := f.cash(), f.wallet()
	first, err := f.hold(cash, merchant, 100, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, holdErr := f.hold(cash, merchant, 100, time.Time{}); holdErr != nil {
		t.Fatal(holdErr)
	}
	bogus := ledger.TransactionPrefix.New().String()
	err = pgx.BeginTxFunc(t.Context(), f.pool, pgx.TxOptions{}, func(tx pgx.Tx) error {
		if _, txnErr := tx.Exec(t.Context(), `INSERT INTO ledger.transactions (id, kind, resolves_id, entry_count, description, created_at)
			VALUES ($1, 'void_pending', $2, 2, 'raw', now())`, bogus, first.ID.String()); txnErr != nil {
			return txnErr
		}
		_, entriesErr := tx.Exec(t.Context(), `INSERT INTO ledger.entries (transaction_id, transaction_kind, account_id, book, currency, layer, amount)
			VALUES ($1, 'void_pending', $2, 'client_funds', 'BRL', 'pending', -200),
			       ($1, 'void_pending', $3, 'client_funds', 'BRL', 'pending', 200)`, bogus, cash.ID.String(), merchant.ID.String())
		return entriesErr
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := f.ledger.Check(t.Context(), f.pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Violations) != 1 || report.Violations[0].Kind != ledger.ViolationBrokenLink || report.Violations[0].Subject != bogus {
		t.Fatalf("violations = %+v, want one broken link on %s", report.Violations, bogus)
	}
}
