package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
)

// sampleEvery is how often the worker reads the gauges the alerts in
// deploy/prometheus/alerts.yml watch.
const sampleEvery = time.Minute

// monitor samples the worker's gauges: what each module reads of its own state, and
// what the ledger's check found.
type monitor struct {
	sampler    *telemetry.Sampler
	violations atomic.Int64
	hasChecked atomic.Bool
}

func newMonitor(pool *pgxpool.Pool, l *ledger.Ledger, a *api.API, s services, network *acquirer.Connector) (*monitor, error) {
	m := &monitor{}
	meter := telemetry.Meter()
	if err := telemetry.ObservePool(meter, "jupiter", pool); err != nil {
		return nil, err
	}
	var gauges []telemetry.Gauge
	for _, g := range [][]telemetry.Gauge{ledger.Gauges, payments.Gauges, api.Gauges, disputes.Gauges, reconciliation.Gauges, jobs.Gauges, acquirer.Gauges} {
		gauges = append(gauges, g...)
	}
	probes := []telemetry.Probe{
		m.ledgerCheck,
		func(ctx context.Context) ([]telemetry.Reading, error) { return l.Health(ctx, pool) },
		func(ctx context.Context) ([]telemetry.Reading, error) { return s.payments.Health(ctx, pool) },
		a.Health, s.disputes.Health, s.reconciliation.Health,
		func(ctx context.Context) ([]telemetry.Reading, error) { return jobs.Health(ctx, pool, time.Now()) },
	}
	if network != nil {
		probes = append(probes, network.Health)
	}
	sampler, err := telemetry.NewSampler(meter, gauges, probes...)
	if err != nil {
		return nil, err
	}
	m.sampler = sampler
	return m, nil
}

func (m *monitor) sample(ctx context.Context) error { return m.sampler.Sample(ctx) }

func (m *monitor) checked(violations int) {
	m.violations.Store(int64(violations))
	m.hasChecked.Store(true)
}

func (m *monitor) ledgerCheck(context.Context) ([]telemetry.Reading, error) {
	if !m.hasChecked.Load() {
		return nil, nil
	}
	return []telemetry.Reading{{Gauge: ledger.ViolationsGauge.Name, Value: m.violations.Load()}}, nil
}
