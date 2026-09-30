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
		if posted.Minor() != t.Posted || balance.PendingCredits.Minor() != t.Held {
			violations = append(violations, Violation{
				Subject: a.AccountID,
				Detail: fmt.Sprintf("ledger has %d posted and %d held; payments say %d received less refunded and %d authorized",
					posted.Minor(), balance.PendingCredits.Minor(), t.Posted, t.Held),
			})
		}
	}
	return violations, nil
}
