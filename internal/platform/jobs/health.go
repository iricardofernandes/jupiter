package jobs

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// Gauges are what the job queue reports to monitoring (docs/runbooks): webhook deliveries
// are its jobs.
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.jobs.waiting_oldest", Description: "How long the oldest job due to run has waited, in seconds."},
	{Name: "jupiter.jobs.retrying", Description: "Jobs that failed and wait to be tried again, by kind."},
	{Name: "jupiter.jobs.discarded", Description: "Jobs given up after their last attempt in the past hour, by kind."},
}

func Health(ctx context.Context, pool *pgxpool.Pool, now time.Time) ([]telemetry.Reading, error) {
	var waiting float64
	if err := pool.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM $1::timestamptz - min(scheduled_at)), 0)::float8
		FROM river.river_job WHERE state = 'available' AND scheduled_at <= $1`, now).Scan(&waiting); err != nil {
		return nil, err
	}
	out := []telemetry.Reading{{Gauge: "jupiter.jobs.waiting_oldest", Value: max(0, int64(waiting))}}
	rows, err := pool.Query(ctx, `SELECT kind, count(*) FILTER (WHERE state = 'retryable'),
			count(*) FILTER (WHERE state = 'discarded' AND finalized_at > $1::timestamptz - interval '1 hour')
		FROM river.river_job WHERE state IN ('retryable', 'discarded') GROUP BY kind ORDER BY kind`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var retrying, discarded int64
		if err := rows.Scan(&kind, &retrying, &discarded); err != nil {
			return nil, err
		}
		attrs := []attribute.KeyValue{attribute.String("kind", kind)}
		out = append(out,
			telemetry.Reading{Gauge: "jupiter.jobs.retrying", Value: retrying, Attrs: attrs},
			telemetry.Reading{Gauge: "jupiter.jobs.discarded", Value: discarded, Attrs: attrs},
		)
	}
	return out, rows.Err()
}
