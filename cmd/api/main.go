package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
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
	pool, err := postgres.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return service.App{}, err
	}
	inserter, err := jobs.NewInserter(pool, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	a := api.New(api.Deps{
		Pool:      pool,
		Merchants: merchant.New(nil),
		Events:    events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent}),
		Box:       box,
		Logger:    logger,
	})
	return service.App{Handler: a.Handler(), Ready: pool.Ping, Close: pool.Close}, nil
}
