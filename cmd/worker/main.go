package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

const (
	applyInterval     = 200 * time.Millisecond
	applyBatch        = 5000
	expiryInterval    = 10 * time.Second
	expiryBatch       = 1000
	checkInterval     = 5 * time.Minute
	completerInterval = time.Minute
	reaperInterval    = time.Hour
	resolveInterval   = 15 * time.Second
	expireAuthsEvery  = time.Minute
	forwardEvery      = 5 * time.Second
	forwardBatch      = 500
	clearingEvery     = 15 * time.Minute
	clearingDays      = 7
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
	box, err := secretbox.FromBase64(os.Getenv("JUPITER_SECRET_KEY"))
	if err != nil {
		return service.App{}, errors.New("JUPITER_SECRET_KEY must be 32 random bytes in base64: " + err.Error())
	}
	cards, err := vault.ClientFromEnv(os.Getenv)
	if err != nil {
		return service.App{}, err
	}
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return service.App{}, err
	}
	inserter, err := jobs.NewInserter(pool, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	eventService := events.New(events.Config{
		Box: box, Jobs: inserter, Render: api.RenderEvent,
		// Only for local development, where webhook receivers run on this machine.
		AllowPrivateNetworks: os.Getenv("JUPITER_WEBHOOK_ALLOW_PRIVATE") == "true",
	})
	workers := river.NewWorkers()
	eventService.RegisterWorkers(workers, pool)
	jobClient, err := jobs.NewWorker(pool, jobs.WorkerConfig{Workers: workers, Logger: logger})
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	network, err := acquirer.FromEnv(ctx, os.Getenv, pool, cards, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	l := ledger.New()
	paymentsConfig := payments.Config{
		Ledger: l, Events: eventService, TestRail: payments.NewTestRail(pool, nil, logger).WithCards(cards),
	}
	if network != nil {
		paymentsConfig.LiveRail = network
	}
	paymentService := payments.New(paymentsConfig)
	a := api.New(api.Deps{
		Pool: pool, Merchants: merchant.New(nil), Events: eventService, Payments: paymentService, Vault: cards, Box: box, Logger: logger,
	})

	background := tasks(jobClient, l, pool, a, paymentService, network, logger)
	return service.App{
		Ready:      pool.Ping,
		Background: background,
		Close: func() {
			if network != nil {
				_ = network.Close()
			}
			pool.Close()
		},
	}, nil
}
