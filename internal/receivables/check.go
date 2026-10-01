package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/receivables/rules"
)

type Violation struct {
	Subject string
	Detail  string
}

// Check verifies that each unit is worth what its installments constituted less what
// refunds took, that no unit is still unregistered past the business day after its
// sale, and that no divergence with the registry is open past the deadline to fix it.
// It reads one snapshot, so that nothing posted while it runs can make it see an account
// and the units that back it at different moments.
func (s *Service) Check(ctx context.Context, pool *pgxpool.Pool) ([]Violation, error) {
	var out []Violation
	snapshot := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := postgres.InTxWith(ctx, pool, snapshot, func(tx pgx.Tx) error {
		var err error
		out, err = s.check(ctx, tx)
		return err
	})
	return out, err
}

func (s *Service) check(ctx context.Context, tx pgx.Tx) ([]Violation, error) {
	q := db.New(tx)
	totals, err := q.InstallmentTotals(ctx)
	if err != nil {
		return nil, err
	}
	var out []Violation
	for _, t := range totals {
		if t.Value != t.FromInstallments {
			out = append(out, Violation{t.ID, fmt.Sprintf("worth %d; its installments constituted %d net of reductions", t.Value, t.FromInstallments)})
		}
	}
	today := dayOf(s.cfg.Now())
	// A unit is late once the business day after its sale has passed, so only those from
	// before yesterday can be.
	var modes []bool
	for _, livemode := range []bool{false, true} {
		if _, err := s.registry(livemode); err == nil {
			modes = append(modes, livemode)
		}
	}
	late, err := q.UnregisteredSince(ctx, db.UnregisteredSinceParams{Day: dateOf(today.AddDate(0, 0, -1)), Modes: modes})
	if err != nil {
		return nil, err
	}
	for _, u := range late {
		due := rules.DeadlineFor(rules.UpdateAfterSale, u.ConstitutedOn.Time).Due(u.ConstitutedOn.Time)
		if today.After(due) {
			out = append(out, Violation{u.ID, fmt.Sprintf("not registered since %s, due by %s: %s",
				u.ConstitutedOn.Time.Format(time.DateOnly), due.Format(time.DateOnly), u.RegisterError)})
		}
	}
	balances, err := s.checkBalances(ctx, tx)
	if err != nil {
		return nil, err
	}
	out = append(out, balances...)
	bought, err := q.AnticipationTotals(ctx)
	if err != nil {
		return nil, err
	}
	for _, b := range bought {
		if b.Anticipated > b.Bought {
			out = append(out, Violation{b.ID, fmt.Sprintf("Jupiter holds %d of it, more than the %d it bought", b.Anticipated, b.Bought)})
		}
	}
	open, err := q.OpenDivergences(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range open {
		found := dayOf(d.FoundAt.Time)
		if due := rules.DeadlineFor(rules.FixDivergence, found).Due(found); today.After(due) {
			out = append(out, Violation{d.Subject, fmt.Sprintf("a %s divergence open since %s: %s", d.Kind, found.Format(time.DateOnly), d.Detail)})
		}
	}
	return out, nil
}

type bucketKey struct {
	recipient, currency, bucket string
	livemode                    bool
}

// checkBalances: each recipient account holds what was posted to it, by kind, and the
// pending one what the recipient's unsettled units hold that Jupiter did not buy.
func (s *Service) checkBalances(ctx context.Context, tx pgx.Tx) ([]Violation, error) {
	q := db.New(tx)
	totals, err := q.MovementTotals(ctx)
	if err != nil {
		return nil, err
	}
	want := map[bucketKey]int64{}
	for _, t := range totals {
		want[bucketKey{t.RecipientID, t.Currency, t.Bucket, t.Livemode}] = t.Total
	}
	pending, err := q.PendingFromUnits(ctx)
	if err != nil {
		return nil, err
	}
	fromUnits := map[bucketKey]int64{}
	for _, p := range pending {
		fromUnits[bucketKey{p.RecipientID, p.Currency, Pending, p.Livemode}] = p.Pending
	}
	accounts, err := q.AllLedgerAccounts(ctx)
	if err != nil {
		return nil, err
	}
	var out []Violation
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
		k := bucketKey{a.RecipientID, a.Currency, a.Role, a.Livemode}
		if posted.Minor() != want[k] {
			out = append(out, Violation{a.RecipientID, fmt.Sprintf("its %s account holds %d; %d was posted to it", a.Role, posted.Minor(), want[k])})
		}
		if a.Role == Pending && posted.Minor() != fromUnits[k] {
			out = append(out, Violation{a.RecipientID, fmt.Sprintf("its pending balance is %d; its unsettled units hold %d not bought by Jupiter", posted.Minor(), fromUnits[k])})
		}
	}
	return out, nil
}
