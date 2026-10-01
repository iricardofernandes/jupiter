package disputes

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/disputes/db"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// dueSoon is how close a dispute's deadline is when monitoring counts it unanswered.
const dueSoon = 48 * time.Hour

// Gauges are what disputes report to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.disputes.due_soon", Description: "Disputes waiting for the merchant's answer with a deadline in the next 48 hours."},
}

func (s *Service) Health(ctx context.Context) ([]telemetry.Reading, error) {
	n, err := db.New(s.cfg.Pool).Health(ctx, pgtype.Timestamptz{Time: s.cfg.Now().Add(dueSoon), Valid: true})
	if err != nil {
		return nil, err
	}
	return []telemetry.Reading{{Gauge: "jupiter.disputes.due_soon", Value: n}}, nil
}
