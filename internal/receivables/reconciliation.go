package receivables

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/receivables/db"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
)

// SettlementReference is how a grade's credit reads on Jupiter's bank statement.
func SettlementReference(date string) string { return "SLC/" + date }

// Grades are Jupiter's side of the SLC's grades: each settled grade, by its day, for its
// total.
func (s *Service) Grades(ctx context.Context, pool *pgxpool.Pool, livemode bool, since time.Time) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).SettledGradesSince(ctx, db.SettledGradesSinceParams{Livemode: livemode, Since: dateOf(since)})
	if err != nil {
		return nil, err
	}
	out := make([]reconciliation.Record, 0, len(rows))
	for _, g := range rows {
		day := g.Date.Time.Format(time.DateOnly)
		out = append(out, reconciliation.Record{
			Identity: day, Key: day, Direction: reconciliation.In, Amount: g.Total, Date: g.Date.Time, Reference: g.LedgerTxn,
		})
	}
	return out, nil
}

// SettlementCredits are what settled grades paid into Jupiter's account at its bank: the
// statement's side of them.
func (s *Service) SettlementCredits(ctx context.Context, pool *pgxpool.Pool, livemode bool, since time.Time) ([]reconciliation.Record, error) {
	rows, err := db.New(pool).SettledGradesSince(ctx, db.SettledGradesSinceParams{Livemode: livemode, Since: dateOf(since)})
	if err != nil {
		return nil, err
	}
	out := make([]reconciliation.Record, 0, len(rows))
	for _, g := range rows {
		if g.Credited == 0 {
			continue
		}
		ref := SettlementReference(g.Date.Time.Format(time.DateOnly))
		out = append(out, reconciliation.Record{
			Identity: ref, Key: ref, Direction: reconciliation.In, Amount: g.Credited, Date: g.Date.Time, Reference: g.LedgerTxn,
		})
	}
	return out, nil
}

// Divergences are the registry reconciliation's divergences still open in a mode.
func (s *Service) Divergences(ctx context.Context, pool *pgxpool.Pool, livemode bool) ([]reconciliation.Divergence, error) {
	rows, err := db.New(pool).OpenDivergences(ctx)
	if err != nil {
		return nil, err
	}
	var out []reconciliation.Divergence
	for _, d := range rows {
		if d.Livemode == livemode {
			out = append(out, reconciliation.Divergence{Subject: d.Kind + ":" + d.Subject, Detail: d.Detail, FoundOn: d.FoundAt.Time})
		}
	}
	return out, nil
}
