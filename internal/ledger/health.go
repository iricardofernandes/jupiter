package ledger

import (
	"context"

	"github.com/iricardofernandes/jupiter/internal/ledger/db"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// ViolationsGauge is how many invariants the ledger's last check found violated, which
// the worker that runs the check reports.
var ViolationsGauge = telemetry.Gauge{Name: "jupiter.ledger.violations", Description: "Invariant violations the ledger's last check found."}

// Gauges are what the ledger reports to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	ViolationsGauge,
	{Name: "jupiter.ledger.queue.depth", Description: "Batched balance deltas not yet applied."},
	{Name: "jupiter.ledger.queue.oldest", Description: "How long ago the oldest unapplied balance delta was posted, in seconds."},
	{Name: "jupiter.ledger.drifted_accounts", Description: "Accounts whose cached balance the checker found drifted from their entries."},
}

func (l *Ledger) Health(ctx context.Context, q db.DBTX) ([]telemetry.Reading, error) {
	row, err := db.New(q).Health(ctx)
	if err != nil {
		return nil, err
	}
	var oldest int64
	if row.OldestQueued.Valid {
		oldest = max(0, int64(l.now().Sub(row.OldestQueued.Time).Seconds()))
	}
	return []telemetry.Reading{
		{Gauge: "jupiter.ledger.queue.depth", Value: row.Queued},
		{Gauge: "jupiter.ledger.queue.oldest", Value: oldest},
		{Gauge: "jupiter.ledger.drifted_accounts", Value: row.Drifted},
	}, nil
}
