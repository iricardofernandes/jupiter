//go:build integration

package ledger_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

// The reference model: the four balance fields of each account, computed in Go
// independently of the SQL that maintains and checks them.
type modelAccount struct {
	ledger.Account
	postedDebits, postedCredits, pendingDebits, pendingCredits int64
}

// headroom is how much more the account can be moved against its normal direction, or
// 0 if it is unconstrained.
func (a modelAccount) headroom() int64 {
	if !a.NonNegative {
		return 0
	}
	if a.Normal == ledger.DebitNormal {
		return a.postedDebits - a.postedCredits - a.pendingCredits
	}
	return a.postedCredits - a.postedDebits - a.pendingDebits
}

func (a modelAccount) allowed() bool {
	if !a.NonNegative {
		return true
	}
	if a.Normal == ledger.DebitNormal {
		return a.pendingCredits+a.postedCredits <= a.postedDebits
	}
	return a.pendingDebits+a.postedDebits <= a.postedCredits
}

type modelEntry struct {
	account id.ID
	amount  int64
}

type modelHold struct {
	id            id.ID
	debit, credit id.ID
	amount        int64
	expiresAt     time.Time
	resolved      bool
}

type modelPosting struct {
	id       id.ID
	entries  []modelEntry
	reversed bool
}

type ledgerMachine struct {
	ctx      context.Context
	pool     *pgxpool.Pool
	ledger   *ledger.Ledger
	clock    *clock
	accounts map[id.ID]*modelAccount
	books    map[ledger.Book][]id.ID
	holds    []*modelHold
	postings []*modelPosting
}

// Each iteration gets its own database, so the full invariant check after every step
// costs the same in the thousandth iteration as in the first.
func TestPropertyInvariantsHoldAfterEveryOperation(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		url, drop, err := server.Database(t.Context())
		if err != nil {
			rt.Fatal(err)
		}
		defer func() { _ = drop(context.WithoutCancel(t.Context())) }()
		pool, err := postgres.Connect(t.Context(), url)
		if err != nil {
			rt.Fatal(err)
		}
		defer pool.Close()
		c := newClock()
		m := &ledgerMachine{
			ctx: t.Context(), pool: pool, ledger: ledger.New(ledger.WithClock(c.Now)), clock: c,
			accounts: map[id.ID]*modelAccount{}, books: map[ledger.Book][]id.ID{},
		}
		m.open(rt)
		rt.Repeat(map[string]func(*rapid.T){
			"":             m.check,
			"post":         m.post,
			"hold":         m.hold,
			"capture":      m.capture,
			"void":         m.void,
			"resolveTwice": m.resolveTwice,
			"advance":      m.advance,
			"reverse":      m.reverse,
			"apply":        m.apply,
		})
	})
}

func (m *ledgerMachine) open(rt *rapid.T) {
	specs := []ledger.AccountSpec{
		{Book: ledger.ClientFunds, Code: "wallet", Normal: ledger.CreditNormal, NonNegative: true},
		{Book: ledger.ClientFunds, Code: "wallet", Normal: ledger.CreditNormal, NonNegative: true},
		{Book: ledger.ClientFunds, Code: "cash", Normal: ledger.DebitNormal},
		{Book: ledger.ClientFunds, Code: "platform", Normal: ledger.CreditNormal, Batched: true},
		{Book: ledger.ClientFunds, Code: "receivable", Normal: ledger.DebitNormal, NonNegative: true},
		{Book: ledger.OwnFunds, Code: "revenue", Normal: ledger.CreditNormal, Batched: true},
		{Book: ledger.OwnFunds, Code: "cash", Normal: ledger.DebitNormal},
	}
	for _, spec := range specs {
		spec.Currency = money.BRL
		a, err := m.ledger.CreateAccount(m.ctx, m.pool, spec)
		if err != nil {
			rt.Fatalf("CreateAccount: %v", err)
		}
		m.accounts[a.ID] = &modelAccount{Account: a}
		m.books[spec.Book] = append(m.books[spec.Book], a.ID)
	}
}

func (m *ledgerMachine) amount(rt *rapid.T, minor int64) money.Amount {
	a, err := money.New(minor, money.BRL)
	if err != nil {
		rt.Fatal(err)
	}
	return a
}

func (m *ledgerMachine) inTx(fn func(pgx.Tx) (ledger.Transaction, error)) (ledger.Transaction, error) {
	var out ledger.Transaction
	err := postgres.InTx(m.ctx, m.pool, func(tx pgx.Tx) error {
		var err error
		out, err = fn(tx)
		return err
	})
	return out, err
}

// apply returns the model after applying posted and pending deltas, and whether every
// touched account still satisfies its constraint.
func (m *ledgerMachine) tryApply(posted, holds, releases []modelEntry) (map[id.ID]modelAccount, bool) {
	next := map[id.ID]modelAccount{}
	get := func(i id.ID) modelAccount {
		if a, ok := next[i]; ok {
			return a
		}
		return *m.accounts[i]
	}
	for _, e := range releases {
		a := get(e.account)
		if e.amount > 0 {
			a.pendingDebits -= e.amount
		} else {
			a.pendingCredits += e.amount
		}
		next[e.account] = a
	}
	for _, e := range holds {
		a := get(e.account)
		if e.amount > 0 {
			a.pendingDebits += e.amount
		} else {
			a.pendingCredits -= e.amount
		}
		next[e.account] = a
	}
	for _, e := range posted {
		a := get(e.account)
		if e.amount > 0 {
			a.postedDebits += e.amount
		} else {
			a.postedCredits -= e.amount
		}
		next[e.account] = a
	}
	for _, a := range next {
		if !a.allowed() {
			return nil, false
		}
	}
	return next, true
}

func (m *ledgerMachine) commit(next map[id.ID]modelAccount) {
	for i, a := range next {
		*m.accounts[i] = a
	}
}

func (m *ledgerMachine) expectOutcome(rt *rapid.T, op string, err error, allowed bool) bool {
	switch {
	case allowed && err != nil:
		rt.Fatalf("%s: unexpected error %v", op, err)
	case !allowed && !errors.Is(err, ledger.ErrInsufficientBalance):
		rt.Fatalf("%s: error = %v, want ErrInsufficientBalance", op, err)
	}
	return allowed
}

// drawAmount usually draws freely, but often draws up to the debited account's headroom,
// so that sequences reach the edge of the non-negative constraint where ordering bugs live.
func (m *ledgerMachine) drawAmount(rt *rapid.T, debited id.ID, lowest int64) int64 {
	if room := m.accounts[debited].headroom(); room >= lowest && rapid.Bool().Draw(rt, "near limit") {
		return rapid.Int64Range(max(lowest, room-2), room).Draw(rt, "amount")
	}
	return rapid.Int64Range(lowest, 5000).Draw(rt, "amount")
}

func (m *ledgerMachine) distinctAccounts(rt *rapid.T, n int) []id.ID {
	book := rapid.SampledFrom([]ledger.Book{ledger.ClientFunds, ledger.OwnFunds}).Draw(rt, "book")
	candidates := m.books[book]
	if n > len(candidates) {
		n = len(candidates)
	}
	perm := rapid.Permutation(candidates).Draw(rt, "accounts")
	return perm[:n]
}

func (m *ledgerMachine) post(rt *rapid.T) {
	accounts := m.distinctAccounts(rt, rapid.IntRange(2, 3).Draw(rt, "legs"))
	total := m.drawAmount(rt, accounts[0], 2)
	entries := []modelEntry{{accounts[0], total}}
	if len(accounts) == 2 {
		entries = append(entries, modelEntry{accounts[1], -total})
	} else {
		first := rapid.Int64Range(1, total-1).Draw(rt, "split")
		entries = append(entries, modelEntry{accounts[1], -first}, modelEntry{accounts[2], -(total - first)})
	}
	legs := make([]ledger.Leg, 0, len(entries))
	for _, e := range entries {
		if e.amount > 0 {
			legs = append(legs, ledger.Debit(e.account, m.amount(rt, e.amount)))
		} else {
			legs = append(legs, ledger.Credit(e.account, m.amount(rt, -e.amount)))
		}
	}

	next, allowed := m.tryApply(entries, nil, nil)
	txn, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return m.ledger.Post(m.ctx, tx, ledger.Posting{Description: "property", Legs: legs})
	})
	if m.expectOutcome(rt, "post", err, allowed) {
		m.commit(next)
		m.postings = append(m.postings, &modelPosting{id: txn.ID, entries: entries})
	}
}

func (m *ledgerMachine) hold(rt *rapid.T) {
	accounts := m.distinctAccounts(rt, 2)
	amount := m.drawAmount(rt, accounts[0], 1)
	var expiresAt time.Time
	if rapid.Bool().Draw(rt, "expires") {
		expiresAt = m.clock.Now().Add(time.Duration(rapid.IntRange(1, 180).Draw(rt, "minutes")) * time.Minute)
	}
	entries := []modelEntry{{accounts[0], amount}, {accounts[1], -amount}}

	next, allowed := m.tryApply(nil, entries, nil)
	txn, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return m.ledger.Hold(m.ctx, tx, ledger.Hold{
			Debit: accounts[0], Credit: accounts[1], Amount: m.amount(rt, amount), ExpiresAt: expiresAt,
		})
	})
	if m.expectOutcome(rt, "hold", err, allowed) {
		m.commit(next)
		m.holds = append(m.holds, &modelHold{id: txn.ID, debit: accounts[0], credit: accounts[1], amount: amount, expiresAt: expiresAt})
	}
}

func (m *ledgerMachine) openHold(rt *rapid.T) *modelHold {
	var open []*modelHold
	for _, h := range m.holds {
		if !h.resolved {
			open = append(open, h)
		}
	}
	if len(open) == 0 {
		rt.Skip("no open hold")
	}
	return rapid.SampledFrom(open).Draw(rt, "hold")
}

func (h *modelHold) release() []modelEntry {
	return []modelEntry{{h.debit, h.amount}, {h.credit, -h.amount}}
}

func (h *modelHold) expired(now time.Time) bool {
	return !h.expiresAt.IsZero() && !now.Before(h.expiresAt)
}

func (m *ledgerMachine) capture(rt *rapid.T) {
	h := m.openHold(rt)
	amount := rapid.Int64Range(1, h.amount).Draw(rt, "capture")
	txn, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return m.ledger.PostPending(m.ctx, tx, h.id, m.amount(rt, amount))
	})
	if h.expired(m.clock.Now()) {
		if !errors.Is(err, ledger.ErrPendingExpired) {
			rt.Fatalf("capturing an expired hold: error = %v, want ErrPendingExpired", err)
		}
		return
	}
	posted := []modelEntry{{h.debit, amount}, {h.credit, -amount}}
	next, allowed := m.tryApply(posted, nil, h.release())
	if m.expectOutcome(rt, "capture", err, allowed) {
		m.commit(next)
		h.resolved = true
		m.postings = append(m.postings, &modelPosting{id: txn.ID, entries: posted})
	}
}

func (m *ledgerMachine) void(rt *rapid.T) {
	h := m.openHold(rt)
	next, allowed := m.tryApply(nil, nil, h.release())
	_, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) { return m.ledger.Void(m.ctx, tx, h.id) })
	if m.expectOutcome(rt, "void", err, allowed) {
		m.commit(next)
		h.resolved = true
	}
}

func (m *ledgerMachine) resolveTwice(rt *rapid.T) {
	var resolved []*modelHold
	for _, h := range m.holds {
		if h.resolved {
			resolved = append(resolved, h)
		}
	}
	if len(resolved) == 0 {
		rt.Skip("no resolved hold")
	}
	h := rapid.SampledFrom(resolved).Draw(rt, "hold")
	_, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) { return m.ledger.Void(m.ctx, tx, h.id) })
	if !errors.Is(err, ledger.ErrAlreadyResolved) {
		rt.Fatalf("resolving %s twice: error = %v, want ErrAlreadyResolved", h.id, err)
	}
}

// advance moves the clock and runs the expiry job.
func (m *ledgerMachine) advance(rt *rapid.T) {
	m.clock.Advance(time.Duration(rapid.IntRange(1, 120).Draw(rt, "minutes")) * time.Minute)
	if _, err := m.ledger.ExpireDue(m.ctx, m.pool, 10_000); err != nil {
		rt.Fatalf("ExpireDue: %v", err)
	}
	now := m.clock.Now()
	for _, h := range m.holds {
		if h.resolved || !h.expired(now) {
			continue
		}
		next, allowed := m.tryApply(nil, nil, h.release())
		if !allowed {
			rt.Fatalf("expiring %s would break a constraint in the model", h.id)
		}
		m.commit(next)
		h.resolved = true
	}
}

func (m *ledgerMachine) reverse(rt *rapid.T) {
	var reversible []*modelPosting
	for _, p := range m.postings {
		if !p.reversed {
			reversible = append(reversible, p)
		}
	}
	if len(reversible) == 0 {
		rt.Skip("nothing to reverse")
	}
	p := rapid.SampledFrom(reversible).Draw(rt, "posting")
	negated := make([]modelEntry, len(p.entries))
	for i, e := range p.entries {
		negated[i] = modelEntry{e.account, -e.amount}
	}
	next, allowed := m.tryApply(negated, nil, nil)
	_, err := m.inTx(func(tx pgx.Tx) (ledger.Transaction, error) {
		return m.ledger.Reverse(m.ctx, tx, p.id, "property")
	})
	if m.expectOutcome(rt, "reverse", err, allowed) {
		m.commit(next)
		p.reversed = true
	}
}

func (m *ledgerMachine) apply(rt *rapid.T) {
	if _, err := m.ledger.ApplyQueued(m.ctx, m.pool, rapid.Int32Range(1, 50).Draw(rt, "batch")); err != nil {
		rt.Fatalf("ApplyQueued: %v", err)
	}
}

func (m *ledgerMachine) check(rt *rapid.T) {
	for _, a := range m.accounts {
		b, err := m.ledger.Balance(m.ctx, m.pool, a.ID)
		if err != nil {
			rt.Fatalf("Balance(%s): %v", a.ID, err)
		}
		got := [4]int64{b.PostedDebits.Minor(), b.PostedCredits.Minor(), b.PendingDebits.Minor(), b.PendingCredits.Minor()}
		want := [4]int64{a.postedDebits, a.postedCredits, a.pendingDebits, a.pendingCredits}
		if got != want {
			rt.Fatalf("%s %s: ledger %v, model %v", a.Code, a.ID, got, want)
		}
	}
	report, err := m.ledger.Check(m.ctx, m.pool, ledger.CheckOptions{
		ClearingGrace: 1000 * time.Hour,
		ExpiryGrace:   time.Nanosecond,
	})
	if err != nil {
		rt.Fatalf("Check: %v", err)
	}
	if len(report.Violations) > 0 {
		rt.Fatalf("invariant violations: %s", fmt.Sprint(report.Violations))
	}
}
