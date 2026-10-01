package acquirer

import (
	"context"
	"time"

	"github.com/iricardofernandes/jupiter/internal/acquirer/db"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
)

// forwardsPendingAfter is how long a reversal or advice may go unacknowledged before
// monitoring counts it.
const forwardsPendingAfter = 10 * time.Minute

// Gauges are what the acquirer connector reports to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.acquirer.forwards_pending", Description: "Reversals and advices the card network has not acknowledged for 10 minutes."},
	{Name: "jupiter.acquirer.clearing_exceptions", Description: "Clearing records that match no capture or refund."},
}

func (c *Connector) Health(ctx context.Context) ([]telemetry.Reading, error) {
	row, err := db.New(c.cfg.Pool).Health(ctx, timestamptz(c.cfg.Now().Add(-forwardsPendingAfter)))
	if err != nil {
		return nil, err
	}
	return []telemetry.Reading{
		{Gauge: "jupiter.acquirer.forwards_pending", Value: row.ForwardsPending},
		{Gauge: "jupiter.acquirer.clearing_exceptions", Value: row.ClearingExceptions},
	}, nil
}
