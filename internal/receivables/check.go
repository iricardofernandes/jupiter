package receivables

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

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
func (s *Service) Check(ctx context.Context, pool *pgxpool.Pool) ([]Violation, error) {
	q := db.New(pool)
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
