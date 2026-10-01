package disputes

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/disputes/rules"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
)

type Violation struct {
	Subject string
	Detail  string
}

// stuckAfter is how long past the network's deadline a dispute may stay open, or an
// action unsent, before the check reports it.
const stuckAfter = 24 * time.Hour

// Check verifies, in one snapshot, that what each dispute says it did to the merchant's
// money is what payments recorded; that every MED claim was held within the window after
// the bank's notice; and that no dispute is stuck open past its deadline or with an
// action no one takes.
func (s *Service) Check(ctx context.Context, pool *pgxpool.Pool) ([]Violation, error) {
	var out []Violation
	snapshot := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	err := postgres.InTxWith(ctx, pool, snapshot, func(tx pgx.Tx) error {
		out = nil
		q := db.New(tx)
		funds, err := s.checkFunds(ctx, tx)
		if err != nil {
			return err
		}
		out = append(out, funds...)
		window := rules.For(rules.Pix, rules.MEDBlockApplied, s.cfg.Now())
		late, err := q.LateBlocks(ctx, int32(window.Length)) //nolint:gosec // minutes, from the table
		if err != nil {
			return err
		}
		for _, l := range late {
			out = append(out, Violation{l.ID, fmt.Sprintf("held %s after the bank's notice, more than %d minutes", l.BlockedAt.Time.Sub(l.NotifiedAt.Time).Round(time.Second), window.Length)})
		}
		stuck, err := q.StuckDisputes(ctx, ts(s.cfg.Now().Add(-stuckAfter)))
		if err != nil {
			return err
		}
		for _, d := range stuck {
			out = append(out, Violation{d.ID, fmt.Sprintf("%s past its deadline, action %q unsent: %s", d.Status, d.PendingAction, d.ActionError)})
		}
		return nil
	})
	return out, err
}

func (s *Service) checkFunds(ctx context.Context, tx pgx.Tx) ([]Violation, error) {
	recorded, err := s.cfg.Payments.AllDisputeFunds(ctx, tx)
	if err != nil {
		return nil, err
	}
	rows, err := db.New(tx).FundsOf(ctx)
	if err != nil {
		return nil, err
	}
	var out []Violation
	for _, d := range rows {
		f, ok := recorded[d.ID]
		switch {
		case !ok:
			out = append(out, Violation{d.ID, fmt.Sprintf("its funds are %s; payments recorded none", d.Funds)})
		case f.Status != d.Funds:
			out = append(out, Violation{d.ID, fmt.Sprintf("its funds are %s; payments recorded them %s", d.Funds, f.Status)})
		case d.Kind == KindMED && f.Amount != d.Blocked:
			out = append(out, Violation{d.ID, fmt.Sprintf("it held %d; payments recorded %d", d.Blocked, f.Amount)})
		}
		delete(recorded, d.ID)
	}
	for reference, f := range recorded {
		out = append(out, Violation{reference, fmt.Sprintf("payments recorded %d %s for a dispute that took nothing", f.Amount, f.Status)})
	}
	return out, nil
}
