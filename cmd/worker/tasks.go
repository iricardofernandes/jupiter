package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/ledger"
)

func applyQueued(l *ledger.Ledger, pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		// Keep draining while full batches come back, then wait for the next tick.
		for {
			applied, err := l.ApplyQueued(ctx, pool, applyBatch)
			if err != nil || applied < applyBatch {
				return err
			}
		}
	}
}

func expireDue(l *ledger.Ledger, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := l.ExpireDue(ctx, pool, expiryBatch)
		if n > 0 {
			logger.InfoContext(ctx, "expired pending transfers", "count", n)
		}
		return err
	}
}

func check(l *ledger.Ledger, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		report, err := l.Check(ctx, pool, checkOptions)
		for _, v := range report.Violations {
			logger.ErrorContext(ctx, "ledger invariant violated",
				"kind", string(v.Kind), "subject", v.Subject, "detail", v.Detail)
		}
		return err
	}
}

func completeAbandoned(a *api.API, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := a.CompleteAbandoned(ctx)
		if n > 0 {
			logger.InfoContext(ctx, "completed abandoned requests", "count", n)
		}
		return err
	}
}

func reap(a *api.API, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := a.ReapIdempotencyKeys(ctx)
		if n > 0 {
			logger.InfoContext(ctx, "reaped idempotency keys", "count", n)
		}
		return err
	}
}
