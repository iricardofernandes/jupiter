package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

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

func main() {
	service.Main("api", ":8080", build)
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
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent})
	network, err := acquirer.FromEnv(ctx, os.Getenv, pool, cards, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	a := api.New(api.Deps{
		Pool:      pool,
		Merchants: merchant.New(nil),
		Events:    eventService,
		Payments:  newPayments(pool, eventService, cards, network, logger),
		Vault:     cards,
		Box:       box,
		Logger:    logger,
	})
	return service.App{Handler: a.Handler(), Ready: pool.Ping, Close: func() {
		if network != nil {
			_ = network.Close()
		}
		pool.Close()
	}}, nil
}

// newPayments serves test mode with the test rail, and live mode with the card network
// when one is configured.
func newPayments(pool *pgxpool.Pool, eventService *events.Service, cards *vault.Client, network *acquirer.Connector, logger *slog.Logger) *payments.Service {
	cfg := payments.Config{
		Ledger:   ledger.New(),
		Events:   eventService,
		TestRail: payments.NewTestRail(pool, nil, logger).WithCards(cards),
	}
	if network != nil {
		cfg.LiveRail = network
	}
	return payments.New(cfg)
}
