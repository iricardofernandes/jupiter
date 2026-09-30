//go:build integration

package ledger_test

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
)

var server *postgrestest.Server

func TestMain(m *testing.M) { os.Exit(postgrestest.Main(m, &server, ledger.Migrate)) }

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type fixture struct {
	t      testing.TB
	pool   *pgxpool.Pool
	ledger *ledger.Ledger
	clock  *clock
}

func newFixture(t testing.TB) *fixture {
	t.Helper()
	c := newClock()
	return &fixture{t: t, pool: server.Pool(t), ledger: ledger.New(ledger.WithClock(c.Now)), clock: c}
}

func brl(t testing.TB, minor int64) money.Amount {
	t.Helper()
	a, err := money.New(minor, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func (f *fixture) account(spec ledger.AccountSpec) ledger.Account {
	f.t.Helper()
	if spec.Currency.Code() == "" {
		spec.Currency = money.BRL
	}
	if spec.Book == "" {
		spec.Book = ledger.ClientFunds
	}
	if spec.Code == "" {
		spec.Code = "test"
	}
	a, err := f.ledger.CreateAccount(f.t.Context(), f.pool, spec)
	if err != nil {
		f.t.Fatalf("CreateAccount: %v", err)
	}
	return a
}

// wallet is a client balance: credit-normal and never below zero.
func (f *fixture) wallet() ledger.Account {
	return f.account(ledger.AccountSpec{Code: "wallet", Normal: ledger.CreditNormal, NonNegative: true})
}

// cash is the bank account holding client funds: debit-normal and free to go negative
// in tests, so it can fund anything.
func (f *fixture) cash() ledger.Account {
	return f.account(ledger.AccountSpec{Code: "cash", Normal: ledger.DebitNormal})
}

func (f *fixture) tx(fn func(pgx.Tx) (ledger.Transaction, error)) (ledger.Transaction, error) {
	f.t.Helper()
	var out ledger.Transaction
	err := postgres.InTx(f.t.Context(), f.pool, func(tx pgx.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

func (f *fixture) post(legs ...ledger.Leg) (ledger.Transaction, error) {
	return f.tx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return f.ledger.Post(f.t.Context(), tx, ledger.Posting{Description: "test", Legs: legs})
	})
}

func (f *fixture) mustPost(legs ...ledger.Leg) ledger.Transaction {
	f.t.Helper()
	txn, err := f.post(legs...)
	if err != nil {
		f.t.Fatalf("Post: %v", err)
	}
	return txn
}

func (f *fixture) balance(a ledger.Account) ledger.Balance {
	f.t.Helper()
	b, err := f.ledger.Balance(f.t.Context(), f.pool, a.ID)
	if err != nil {
		f.t.Fatalf("Balance(%s): %v", a.ID, err)
	}
	return b
}

// fields returns posted debits, posted credits, pending debits and pending credits.
func (f *fixture) fields(a ledger.Account) [4]int64 {
	f.t.Helper()
	b := f.balance(a)
	return [4]int64{b.PostedDebits.Minor(), b.PostedCredits.Minor(), b.PendingDebits.Minor(), b.PendingCredits.Minor()}
}

func (f *fixture) requireClean() {
	f.t.Helper()
	report, err := f.ledger.Check(f.t.Context(), f.pool, ledger.CheckOptions{
		ClearingGrace: 24 * time.Hour,
		ExpiryGrace:   24 * time.Hour,
	})
	if err != nil {
		f.t.Fatalf("Check: %v", err)
	}
	for _, v := range report.Violations {
		f.t.Errorf("invariant violation: %+v", v)
	}
}
