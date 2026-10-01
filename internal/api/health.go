package api

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/iricardofernandes/jupiter/internal/api/db"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// abandonedAfter is how long a request stopped between phases may wait for the
// completer before monitoring counts it.
const abandonedAfter = 10 * time.Minute

// Gauges are what the API reports to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.api.requests_unfinished", Description: "Requests stopped between phases that the completer has not finished in 10 minutes."},
}

func (a *API) Health(ctx context.Context) ([]telemetry.Reading, error) {
	n, err := db.New(a.deps.Pool).Health(ctx, pgtype.Timestamptz{Time: a.deps.Now().Add(-abandonedAfter), Valid: true})
	if err != nil {
		return nil, err
	}
	return []telemetry.Reading{{Gauge: "jupiter.api.requests_unfinished", Value: n}}, nil
}
