package ledger

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger/db"
	"github.com/iricardofernandes/jupiter/internal/money"
)

var accountCode = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

func (l *Ledger) CreateAccount(ctx context.Context, q db.DBTX, spec AccountSpec) (Account, error) {
	if err := spec.validate(); err != nil {
		return Account{}, err
	}
	account := Account{AccountSpec: spec, ID: AccountPrefix.New(), CreatedAt: l.now().UTC()}
	err := db.New(q).InsertAccount(ctx, db.InsertAccountParams{
		ID:          account.ID.String(),
		Book:        string(spec.Book),
		Code:        spec.Code,
		Currency:    spec.Currency.Code(),
		Normal:      string(spec.Normal),
		NonNegative: spec.NonNegative,
		Batched:     spec.Batched,
		Clearing:    spec.Clearing,
		CreatedAt:   timestamptz(account.CreatedAt),
	})
	if err != nil {
		return Account{}, fmt.Errorf("creating account: %w", err)
	}
	return account, nil
}

func (s AccountSpec) validate() error {
	switch {
	case s.Book != ClientFunds && s.Book != OwnFunds:
		return fmt.Errorf("%w: book %q", ErrInvalid, s.Book)
	case s.Normal != DebitNormal && s.Normal != CreditNormal:
		return fmt.Errorf("%w: normal balance %q", ErrInvalid, s.Normal)
	case !accountCode.MatchString(s.Code):
		return fmt.Errorf("%w: account code %q", ErrInvalid, s.Code)
	case s.Currency.Code() == "":
		return fmt.Errorf("%w: account has no currency", ErrInvalid)
	case s.NonNegative && s.Batched:
		return fmt.Errorf("%w: a batched account cannot be non-negative", ErrInvalid)
	default:
		return nil
	}
}

func loadAccounts(ctx context.Context, q *db.Queries, ids []id.ID) (map[id.ID]Account, error) {
	keys := make([]string, 0, len(ids))
	for _, i := range ids {
		keys = append(keys, i.String())
	}
	rows, err := q.GetAccounts(ctx, keys)
	if err != nil {
		return nil, fmt.Errorf("loading accounts: %w", err)
	}
	accounts := make(map[id.ID]Account, len(rows))
	for _, row := range rows {
		a, err := accountFromRow(row)
		if err != nil {
			return nil, err
		}
		accounts[a.ID] = a
	}
	for _, i := range ids {
		if _, ok := accounts[i]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrAccountNotFound, i)
		}
	}
	return accounts, nil
}

func accountFromRow(row db.LedgerAccount) (Account, error) {
	accountID, err := AccountPrefix.Parse(row.ID)
	if err != nil {
		return Account{}, fmt.Errorf("stored account id: %w", err)
	}
	currency, err := money.CurrencyByCode(row.Currency)
	if err != nil {
		return Account{}, fmt.Errorf("stored account %s: %w", row.ID, err)
	}
	return Account{
		ID: accountID,
		AccountSpec: AccountSpec{
			Book:        Book(row.Book),
			Code:        row.Code,
			Currency:    currency,
			Normal:      Normal(row.Normal),
			NonNegative: row.NonNegative,
			Batched:     row.Batched,
			Clearing:    row.Clearing,
		},
		CreatedAt: row.CreatedAt.Time,
	}, nil
}

func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
