package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"
	"github.com/riverqueue/river/rivermigrate"
)

const schema = "river"

type Client = river.Client[pgx.Tx]

func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		return fmt.Errorf("creating schema %s: %w", schema, err)
	}
	migrator, err := rivermigrate.New(riverpgxv5.New(pool), &rivermigrate.Config{Schema: schema})
	if err != nil {
		return fmt.Errorf("river migrator: %w", err)
	}
	if _, err := migrator.Migrate(ctx, rivermigrate.DirectionUp, nil); err != nil {
		return fmt.Errorf("migrating river: %w", err)
	}
	return nil
}

// NewInserter returns a client that only enqueues jobs, for processes that run none.
func NewInserter(pool *pgxpool.Pool, logger *slog.Logger) (*Client, error) {
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Schema: schema, Logger: logger})
	if err != nil {
		return nil, fmt.Errorf("river inserter: %w", err)
	}
	return client, nil
}

// QueueWebhooks holds webhook deliveries, apart from the rest of the background.
const QueueWebhooks = "webhooks"

type WorkerConfig struct {
	Workers           *river.Workers
	Concurrency       int
	FetchPollInterval time.Duration
	Logger            *slog.Logger
}

func NewWorker(pool *pgxpool.Pool, cfg WorkerConfig) (*Client, error) {
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 20
	}
	if cfg.FetchPollInterval == 0 {
		cfg.FetchPollInterval = river.FetchPollIntervalDefault
	}
	client, err := river.NewClient(riverpgxv5.New(pool), &river.Config{
		Schema:  schema,
		Logger:  cfg.Logger,
		Workers: cfg.Workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: cfg.Concurrency}, QueueWebhooks: {MaxWorkers: cfg.Concurrency},
		},
		FetchPollInterval: cfg.FetchPollInterval,
		FetchCooldown:     min(cfg.FetchPollInterval, river.FetchCooldownDefault),
	})
	if err != nil {
		return nil, fmt.Errorf("river worker: %w", err)
	}
	return client, nil
}

// Run starts the client and stops it, letting running jobs finish, when ctx is done.
func Run(client *Client) func(context.Context) error {
	return func(ctx context.Context) error {
		if err := client.Start(ctx); err != nil {
			return fmt.Errorf("starting river: %w", err)
		}
		<-ctx.Done()
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return client.Stop(stopCtx)
	}
}
