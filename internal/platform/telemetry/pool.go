package telemetry

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// ObservePool reports a database pool's connections and how often a request had to wait
// for one: the first sign the pool, or the database behind it, is saturated.
//   - jupiter.db.pool.connections, by state (acquired, idle, max);
//   - jupiter.db.pool.acquires, every connection taken;
//   - jupiter.db.pool.waited_acquires, those that found none free and waited;
//   - jupiter.db.pool.acquire_wait, the time spent waiting, in seconds.
func ObservePool(meter metric.Meter, name string, pool *pgxpool.Pool) error {
	connections, err := meter.Int64ObservableGauge("jupiter.db.pool.connections", metric.WithDescription("Database connections, by state."))
	if err != nil {
		return err
	}
	acquires, err := meter.Int64ObservableCounter("jupiter.db.pool.acquires", metric.WithDescription("Connections taken from the pool."))
	if err != nil {
		return err
	}
	waited, err := meter.Int64ObservableCounter("jupiter.db.pool.waited_acquires", metric.WithDescription("Connections taken after waiting for one to be free."))
	if err != nil {
		return err
	}
	wait, err := meter.Float64ObservableCounter("jupiter.db.pool.acquire_wait", metric.WithUnit("s"), metric.WithDescription("Time spent waiting for a connection."))
	if err != nil {
		return err
	}
	poolAttr := attribute.String("pool", name)
	_, err = meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		st := pool.Stat()
		for state, n := range map[string]int32{"acquired": st.AcquiredConns(), "idle": st.IdleConns(), "max": st.MaxConns()} {
			o.ObserveInt64(connections, int64(n), metric.WithAttributes(poolAttr, attribute.String("state", state)))
		}
		o.ObserveInt64(acquires, st.AcquireCount(), metric.WithAttributes(poolAttr))
		o.ObserveInt64(waited, st.EmptyAcquireCount(), metric.WithAttributes(poolAttr))
		o.ObserveFloat64(wait, st.EmptyAcquireWaitTime().Seconds(), metric.WithAttributes(poolAttr))
		return nil
	}, connections, acquires, waited, wait)
	return err
}
