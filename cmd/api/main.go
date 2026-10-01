package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/platform/telemetry"
	"github.com/iricardofernandes/jupiter/internal/risk"
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
	if err := telemetry.ObservePool(telemetry.Meter(), "jupiter", pool); err != nil {
		pool.Close()
		return service.App{}, err
	}
	inserter, err := jobs.NewInserter(pool, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent})
	riskEngine := risk.New(risk.Config{})
	r, err := connectRails(ctx, pool, cards, logger)
	if err != nil {
		pool.Close()
		return service.App{}, err
	}
	s := r.services(pool, ledger.New(), eventService, riskEngine, cards, logger)
	paymentService, subscriptionService := s.payments, s.subscriptions
	a := api.New(api.Deps{
		Pool: pool, Merchants: s.merchants, Events: eventService, Payments: paymentService, Risk: riskEngine,
		Subscriptions: subscriptionService, Receivables: s.receivables, Recipients: s.recipients, Disputes: s.disputes, Reconciliation: s.reconciliation,
		Vault: cards, Box: box, Logger: logger,
	})
	// The public address also serves the 3DS server's pages and results, and the card
	// network's token and dispute events.
	mux := http.NewServeMux()
	mux.Handle("/", a.Handler())
	if r.authenticator != nil {
		r.authenticator.Attach(paymentService, a.ResumePayment)
		mux.Handle("/3ds/", r.authenticator.Handler())
	}
	if r.network != nil {
		mux.Handle("/network/", r.network.Handler(paymentService, s.disputes))
	}
	webhooks, err := pixWebhooks(ctx, paymentService, subscriptionService, s.disputes, r.livePix, r.testPix, logger)
	if err != nil {
		r.close()
		pool.Close()
		return service.App{}, err
	}
	return service.App{Handler: mux, Ready: pool.Ping, Background: webhooks, Close: func() {
		r.close()
		pool.Close()
	}}, nil
}

// pixWebhooks serves the Pix bank's notifications on JUPITER_PIX_WEBHOOK_ADDR, over
// mutual TLS (JUPITER_PIX_WEBHOOK_CERT, _KEY, and _CA, the CA of the bank's client
// certificate, whose identity is JUPITER_PIX_WEBHOOK_IDENTITY): live mode's under
// /pix/live, test mode's under /pix/test, MED claims among them. It registers each
// connector's webhook with the bank; a registration that fails is logged, and
// reconciliation covers for it.
func pixWebhooks(ctx context.Context, p *payments.Service, subs pix.RecurrenceSync, claims *disputes.Service, live, test *pix.Connector, logger *slog.Logger) ([]func(context.Context) error, error) {
	addr := os.Getenv("JUPITER_PIX_WEBHOOK_ADDR")
	if addr == "" || (live == nil && test == nil) {
		return nil, nil
	}
	for _, name := range []string{"JUPITER_PIX_WEBHOOK_CERT", "JUPITER_PIX_WEBHOOK_KEY", "JUPITER_PIX_WEBHOOK_CA", "JUPITER_PIX_WEBHOOK_IDENTITY"} {
		if os.Getenv(name) == "" {
			return nil, fmt.Errorf("%s is required to receive the Pix bank's notifications", name)
		}
	}
	tlsConfig, err := mtls.LoadServerConfig(os.Getenv("JUPITER_PIX_WEBHOOK_CERT"), os.Getenv("JUPITER_PIX_WEBHOOK_KEY"),
		os.Getenv("JUPITER_PIX_WEBHOOK_CA"), os.Getenv("JUPITER_PIX_WEBHOOK_IDENTITY"))
	if err != nil {
		return nil, fmt.Errorf("JUPITER_PIX_WEBHOOK_CERT: %w", err)
	}
	mux := http.NewServeMux()
	for prefix, c := range map[string]*pix.Connector{"/pix/live": live, "/pix/test": test} {
		if c == nil {
			continue
		}
		mux.Handle(prefix+"/", http.StripPrefix(prefix, c.Handler(p, subs, claims)))
		if err := c.RegisterWebhook(ctx); err != nil {
			logger.WarnContext(ctx, "registering the Pix webhook", "path", prefix, "error", err)
		}
	}
	return []func(context.Context) error{func(ctx context.Context) error {
		return service.Run(ctx, service.Config{Name: "pix-webhooks", HTTPAddr: addr}, service.App{Handler: mux, TLS: tlsConfig}, logger)
	}}, nil
}
