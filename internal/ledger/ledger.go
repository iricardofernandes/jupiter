package ledger

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger/migrations"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

var (
	AccountPrefix     = id.MustPrefix("acct")
	TransactionPrefix = id.MustPrefix("txn")
)

var (
	ErrInvalid             = errors.New("ledger: invalid request")
	ErrUnbalanced          = errors.New("ledger: entries do not balance")
	ErrAccountNotFound     = errors.New("ledger: account not found")
	ErrTransactionNotFound = errors.New("ledger: transaction not found")
	ErrInsufficientBalance = errors.New("ledger: insufficient balance")
	ErrBalanceDrift        = errors.New("ledger: cached balance disagrees with entries")
	ErrNotPending          = errors.New("ledger: transaction is not a pending transfer")
	ErrAlreadyResolved     = errors.New("ledger: pending transfer already resolved")
	ErrPendingExpired      = errors.New("ledger: pending transfer has expired")
	ErrNotYetExpired       = errors.New("ledger: pending transfer has not expired")
	ErrNotReversible       = errors.New("ledger: transaction cannot be reversed")
	ErrAlreadyReversed     = errors.New("ledger: transaction already reversed")
)

// Book separates money Jupiter holds for its clients from its own: Brazilian law makes
// payment-account balances a segregated estate, so every transaction must balance
// within each book on its own.
type Book string

const (
	ClientFunds Book = "client_funds"
	OwnFunds    Book = "own_funds"
)

type Normal string

const (
	DebitNormal  Normal = "debit"
	CreditNormal Normal = "credit"
)

type Kind string

const (
	KindPosted        Kind = "posted"
	KindPending       Kind = "pending"
	KindPostPending   Kind = "post_pending"
	KindVoidPending   Kind = "void_pending"
	KindExpirePending Kind = "expire_pending"
	KindReversal      Kind = "reversal"
)

type Layer string

const (
	LayerPending Layer = "pending"
	LayerPosted  Layer = "posted"
)

type AccountSpec struct {
	Book     Book
	Code     string
	Currency money.Currency
	Normal   Normal
	// NonNegative makes the database refuse any entry that would take the balance, in
	// its normal direction and net of pending holds against it, below zero.
	NonNegative bool
	// Batched accounts are hot: their entries are written synchronously, but their
	// cached balance is updated in batches, so writers never queue on its row (ADR 0008).
	Batched bool
	// Clearing accounts must return to zero; the invariant checker reports one that
	// has not settled within the grace period.
	Clearing bool
}

type Account struct {
	AccountSpec
	ID        id.ID
	CreatedAt time.Time
}

type side int

const (
	debit side = iota + 1
	credit
)

// Leg is one line of a posting. Its amount is always positive; the side is chosen by
// Debit or Credit.
type Leg struct {
	Account id.ID
	Amount  money.Amount
	side    side
}

func Debit(account id.ID, amount money.Amount) Leg {
	return Leg{Account: account, Amount: amount, side: debit}
}

func Credit(account id.ID, amount money.Amount) Leg {
	return Leg{Account: account, Amount: amount, side: credit}
}

type Posting struct {
	Description string
	Legs        []Leg
}

type Hold struct {
	Description string
	Debit       id.ID
	Credit      id.ID
	Amount      money.Amount
	// ExpiresAt is when an unresolved hold is voided; the zero value never expires.
	ExpiresAt time.Time
}

// Entry amounts are signed: a debit is positive, a credit negative.
type Entry struct {
	Account id.ID
	Layer   Layer
	Amount  money.Amount
}

type Transaction struct {
	ID          id.ID
	Kind        Kind
	Resolves    id.ID
	Reverses    id.ID
	ExpiresAt   time.Time
	Description string
	CreatedAt   time.Time
	Entries     []Entry
}

type Ledger struct {
	now func() time.Time
}

type Option func(*Ledger)

// WithClock replaces the wall clock, for deterministic tests and simulations.
func WithClock(now func() time.Time) Option {
	return func(l *Ledger) { l.now = now }
}

func New(opts ...Option) *Ledger {
	l := &Ledger{now: time.Now}
	for _, opt := range opts {
		opt(l)
	}
	return l
}

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	return postgres.Migrate(ctx, pool, "ledger", migrations.FS)
}
