package payments

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

type Violation struct {
	Subject string
	Detail  string
}

// Check verifies, in one snapshot, that every intent agrees with its latest attempt,
// that refunded amounts match succeeded refunds, and that each merchant's ledger balance
// holds exactly what its payments say: posted, what was received less what was
// refunded; pending, what open authorizations hold.
func (s *Service) Check(ctx context.Context, pool *pgxpool.Pool) ([]Violation, error) {
	var violations []Violation
	snapshot := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := postgres.InTxWith(ctx, pool, snapshot, func(tx pgx.Tx) error {
		violations = nil
		q := db.New(tx)
		rows, err := q.IntentInconsistencies(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			violations = append(violations, Violation{
				Subject: row.ID,
				Detail:  fmt.Sprintf("intent %s disagrees with its attempt (%s) or its refunds", row.Status, row.AttemptStatus),
			})
		}
		found, err := s.checkBalances(ctx, tx, q)
		violations = append(violations, found...)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("checking payments: %w", err)
	}
	return violations, nil
}

func (s *Service) checkBalances(ctx context.Context, tx pgx.Tx, q *db.Queries) ([]Violation, error) {
	totals, err := q.MerchantTotals(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := q.MerchantLedgerAccounts(ctx)
	if err != nil {
		return nil, err
	}
	type scope struct {
		merchant, currency string
		livemode           bool
	}
	want := map[scope]db.MerchantTotalsRow{}
	for _, t := range totals {
		want[scope{t.MerchantID, t.Currency, t.Livemode}] = t
	}
	var violations []Violation
	for _, a := range accounts {
		accountID, err := ledger.AccountPrefix.Parse(a.AccountID)
		if err != nil {
			return nil, err
		}
		balance, err := s.cfg.Ledger.Balance(ctx, tx, accountID)
		if err != nil {
			return nil, err
		}
		posted, err := balance.Posted()
		if err != nil {
			return nil, err
		}
		t := want[scope{a.MerchantID, a.Currency, a.Livemode}]
		if posted.Minor() != t.Posted || balance.PendingCredits.Minor() != t.Held || balance.PendingDebits.Minor() != t.PayingOut {
			violations = append(violations, Violation{
				Subject: a.AccountID,
				Detail: fmt.Sprintf("ledger has %d posted, %d held for and %d against the merchant; payments say %d received less refunded and paid out, %d authorized and %d paying out",
					posted.Minor(), balance.PendingCredits.Minor(), balance.PendingDebits.Minor(), t.Posted, t.Held, t.PayingOut),
			})
		}
	}
	unmatched, err := s.checkUnmatchedPix(ctx, tx, q)
	if err != nil {
		return nil, err
	}
	fees, err := s.checkFees(ctx, tx, q)
	return append(append(violations, unmatched...), fees...), err
}

// checkFees: each mode's fee account holds the fees charged less what refunds returned.
func (s *Service) checkFees(ctx context.Context, tx pgx.Tx, q *db.Queries) ([]Violation, error) {
	totals, err := q.FeeTotals(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := q.FeeLedgerAccounts(ctx)
	if err != nil {
		return nil, err
	}
	type scope struct {
		livemode bool
		currency string
	}
	want := map[scope]int64{}
	for _, t := range totals {
		want[scope{t.Livemode, t.Currency}] = t.Held
	}
	var violations []Violation
	for _, a := range accounts {
		accountID, err := ledger.AccountPrefix.Parse(a.AccountID)
		if err != nil {
			return nil, err
		}
		balance, err := s.cfg.Ledger.Balance(ctx, tx, accountID)
		if err != nil {
			return nil, err
		}
		posted, err := balance.Posted()
		if err != nil {
			return nil, err
		}
		if held := want[scope{a.Livemode, a.Currency}]; posted.Minor() != held {
			violations = append(violations, Violation{
				Subject: a.AccountID,
				Detail:  fmt.Sprintf("the fee account holds %d; payments charged %d net of what refunds returned", posted.Minor(), held),
			})
		}
	}
	return violations, nil
}

// checkUnmatchedPix: what the held-apart account holds is the Pix received that paid
// nothing and are not yet returned.
func (s *Service) checkUnmatchedPix(ctx context.Context, tx pgx.Tx, q *db.Queries) ([]Violation, error) {
	totals, err := q.UnmatchedPixTotals(ctx)
	if err != nil {
		return nil, err
	}
	accounts, err := q.PixLedgerAccounts(ctx)
	if err != nil {
		return nil, err
	}
	type scope struct {
		livemode bool
		currency string
	}
	want := map[scope]int64{}
	for _, t := range totals {
		want[scope{t.Livemode, t.Currency}] = t.Held
	}
	var violations []Violation
	for _, a := range accounts {
		accountID, err := ledger.AccountPrefix.Parse(a.AccountID)
		if err != nil {
			return nil, err
		}
		balance, err := s.cfg.Ledger.Balance(ctx, tx, accountID)
		if err != nil {
			return nil, err
		}
		posted, err := balance.Posted()
		if err != nil {
			return nil, err
		}
		if held := want[scope{a.Livemode, a.Currency}]; posted.Minor() != held {
			violations = append(violations, Violation{
				Subject: a.AccountID,
				Detail:  fmt.Sprintf("the unmatched Pix account holds %d; %d is waiting to be returned", posted.Minor(), held),
			})
		}
	}
	return violations, nil
}
