package payments

import (
	"context"
	"time"

	"github.com/iricardofernandes/jupiter/internal/payments/db"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// How long an outcome may stay open before monitoring counts it: the resolver runs every
// minute, and a payout's bank answers within the hour.
const (
	attemptsUnresolvedAfter = 15 * time.Minute
	payoutsUnresolvedAfter  = time.Hour
)

// Gauges are what the payments module reports to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.payments.attempts_unresolved", Description: "Card attempts whose outcome has not been final for 15 minutes."},
	{Name: "jupiter.payments.refunds_unresolved", Description: "Refunds whose outcome has not been final for 15 minutes."},
	{Name: "jupiter.payouts.unresolved", Description: "Payouts sent without a final outcome for an hour."},
	{Name: "jupiter.pix.unreturned", Description: "Pix received that paid nothing and have not been returned within an hour."},
}

func (s *Service) Health(ctx context.Context, q db.DBTX) ([]telemetry.Reading, error) {
	now := s.cfg.Now().UTC()
	row, err := db.New(q).Health(ctx, db.HealthParams{
		AttemptsBefore: ts(now.Add(-attemptsUnresolvedAfter)), PayoutsBefore: ts(now.Add(-payoutsUnresolvedAfter)),
	})
	if err != nil {
		return nil, err
	}
	return []telemetry.Reading{
		{Gauge: "jupiter.payments.attempts_unresolved", Value: row.AttemptsUnresolved},
		{Gauge: "jupiter.payments.refunds_unresolved", Value: row.RefundsUnresolved},
		{Gauge: "jupiter.payouts.unresolved", Value: row.PayoutsUnresolved},
		{Gauge: "jupiter.pix.unreturned", Value: row.PixUnreturned},
	}, nil
}
