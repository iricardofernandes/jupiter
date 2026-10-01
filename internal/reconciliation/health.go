package reconciliation

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
	"github.com/iricardofernandes/jupiter/internal/reconciliation/db"
)

// Gauges are what reconciliation reports to monitoring (docs/runbooks).
var Gauges = []telemetry.Gauge{
	{Name: "jupiter.reconciliation.breaks_open", Description: "Open reconciliation breaks, by counterparty."},
	{Name: "jupiter.reconciliation.breaks_ageing", Description: "Reconciliation breaks open for more than a day, by counterparty."},
	{Name: "jupiter.reconciliation.days_behind", Description: "Days between the last day live mode was reconciled through and yesterday."},
}

func (s *Service) Health(ctx context.Context) ([]telemetry.Reading, error) {
	q := db.New(s.cfg.Pool)
	today := Day(s.cfg.Now())
	rows, err := q.Health(ctx, date(today.AddDate(0, 0, -1)))
	if err != nil {
		return nil, err
	}
	out := make([]telemetry.Reading, 0, 2*len(rows)+1)
	for _, r := range rows {
		counterparty := attribute.String("counterparty", r.Counterparty)
		out = append(out,
			telemetry.Reading{Gauge: "jupiter.reconciliation.breaks_open", Value: r.Open, Attrs: []attribute.KeyValue{counterparty}},
			telemetry.Reading{Gauge: "jupiter.reconciliation.breaks_ageing", Value: r.Ageing, Attrs: []attribute.KeyValue{counterparty}},
		)
	}
	last, err := q.LastRun(ctx, true)
	if err != nil {
		return nil, err
	}
	if last.Valid && last.Time.Year() > 1 {
		behind := int64(today.AddDate(0, 0, -1).Sub(last.Time).Hours() / 24)
		out = append(out, telemetry.Reading{Gauge: "jupiter.reconciliation.days_behind", Value: max(0, behind)})
	}
	return out, nil
}
