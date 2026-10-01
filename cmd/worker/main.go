package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/risk"
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
	tokensEvery       = 30 * time.Second
	tokensBatch       = 100
	pixExpiryEvery    = 30 * time.Second
	pixReturnsEvery   = time.Minute
	payoutsEvery      = 15 * time.Second
	pixReconcileEvery = 5 * time.Minute
	// subscriptionsEvery is how often the worker looks for subscriptions due a look;
	// each is looked at no more than hourly, and sooner when the bank notifies.
	subscriptionsEvery = time.Minute
	// receivablesEvery is how often units go to the registry, well within the business
	// day after a sale it allows, and reconciliations that are due run.
	receivablesEvery = 5 * time.Minute
	// collectionEvery is how often boletos go to the bank and its return files are read.
	collectionEvery = time.Minute
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
	r, err := connectRails(ctx, pool, cards, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	l := ledger.New()
	riskEngine := risk.New(risk.Config{})
	s := r.services(pool, l, eventService, riskEngine, cards, logger)
	a := api.New(api.Deps{
		Pool: pool, Merchants: s.merchants, Events: eventService, Payments: s.payments, Vault: cards, Risk: riskEngine,
		Subscriptions: s.subscriptions, Receivables: s.receivables, Recipients: s.recipients, Box: box, Logger: logger,
	})

	return service.App{
		Ready: pool.Ping,
		Background: tasks(jobClient, l, pool, a, s.payments, r.network, []*pix.Connector{r.livePix, r.testPix}, logger,
			service.Every(logger, "subscriptions.advance", subscriptionsEvery, counted(logger, "looked at subscriptions", func(ctx context.Context) (int, error) {
				return s.subscriptions.Advance(ctx, pool)
			})),
			service.Every(logger, "receivables.advance", receivablesEvery, advanceReceivables(s.receivables, pool, logger)),
			service.Every(logger, "bank.collection", collectionEvery, collect(s.payments, pool, logger, r.liveBank, r.testBank))),
		Close: func() {
			r.close()
			pool.Close()
		},
	}, nil
}
