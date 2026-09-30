package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
)

const (
	applyInterval  = 200 * time.Millisecond
	applyBatch     = 5000
	expiryInterval = 10 * time.Second
	expiryBatch    = 1000
	checkInterval  = 5 * time.Minute
)

var checkOptions = ledger.CheckOptions{
	ClearingGrace: 24 * time.Hour,
	ExpiryGrace:   5 * time.Minute,
	MarkDrift:     true,
}

func main() {
	service.Main("worker", ":8081", build)
}

func build(ctx context.Context, cfg service.Config, logger *slog.Logger) (service.App, error) {
	if cfg.DatabaseURL == "" {
		return service.App{}, errors.New("JUPITER_DATABASE_URL is required")
	}
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return service.App{}, err
	}
	l := ledger.New()

	applyQueued := func(ctx context.Context) error {
		// Keep draining while full batches come back, then wait for the next tick.
		for {
			applied, err := l.ApplyQueued(ctx, pool, applyBatch)
			if err != nil || applied < applyBatch {
				return err
			}
		}
	}
	expireDue := func(ctx context.Context) error {
		n, err := l.ExpireDue(ctx, pool, expiryBatch)
		if n > 0 {
			logger.InfoContext(ctx, "expired pending transfers", "count", n)
		}
		return err
	}
	check := func(ctx context.Context) error {
		report, err := l.Check(ctx, pool, checkOptions)
		for _, v := range report.Violations {
			logger.ErrorContext(ctx, "ledger invariant violated",
				"kind", string(v.Kind), "subject", v.Subject, "detail", v.Detail)
		}
		return err
	}

	return service.App{
		Ready: pool.Ping,
		Background: []func(context.Context) error{
			service.Every(logger, "ledger.apply_queued", applyInterval, applyQueued),
			service.Every(logger, "ledger.expire_due", expiryInterval, expireDue),
			service.Every(logger, "ledger.check", checkInterval, check),
		},
		Close: pool.Close,
	}, nil
}
