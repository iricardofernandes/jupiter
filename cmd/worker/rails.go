package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/registry"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

// rails are the counterparties payments reach, each nil when it is not configured: the
// card network, 3-D Secure, and the Pix bank in each mode.
type rails struct {
	network       *acquirer.Connector
	authenticator *authentication.Server
	livePix       *pix.Connector
	testPix       *pix.Connector
	liveRegistry  *registry.Connector
	testRegistry  *registry.Connector
}

func connectRails(ctx context.Context, pool *pgxpool.Pool, cards *vault.Client, logger *slog.Logger) (rails, error) {
	var r rails
	var err error
	if r.network, err = acquirer.FromEnv(ctx, os.Getenv, pool, cards, logger); err != nil {
		return rails{}, err
	}
	if r.authenticator, err = authentication.FromEnv(os.Getenv, pool, cards, logger); err != nil {
		r.close()
		return rails{}, err
	}
	if r.livePix, r.testPix, err = pix.Rails(os.Getenv, pool, logger); err != nil {
		r.close()
		return rails{}, err
	}
	if r.liveRegistry, r.testRegistry, err = registry.Registries(os.Getenv); err != nil {
		r.close()
		return rails{}, err
	}
	return r, nil
}

// services are the domain services both the api and the worker build alike.
type services struct {
	merchants     *merchant.Service
	payments      *payments.Service
	subscriptions *subscriptions.Service
	receivables   *receivables.Service
}

func (r rails) services(pool *pgxpool.Pool, l *ledger.Ledger, e *events.Service, riskEngine *risk.Service, cards *vault.Client, logger *slog.Logger) services {
	cfg := payments.Config{Ledger: l, Events: e, Risk: riskEngine, TestRail: payments.NewTestRail(pool, nil, logger).WithCards(cards)}
	r.configure(&cfg)
	s := services{merchants: merchant.New(nil)}
	s.receivables = r.receivables(pool, s.merchants, logger)
	cfg.Receivables = s.receivables
	s.payments = payments.New(cfg)
	s.subscriptions = r.subscriptions(pool, s.payments, e, logger)
	return s
}

// receivables keeps card receivables, registered in the modes that have a registry.
func (r rails) receivables(pool *pgxpool.Pool, merchants *merchant.Service, logger *slog.Logger) *receivables.Service {
	cfg := receivables.Config{Pool: pool, Merchants: merchants, Domicile: registry.Domicile(os.Getenv), Logger: logger}
	registry.Configure(&cfg, r.liveRegistry, r.testRegistry)
	return receivables.New(cfg)
}

// subscriptions charges by Pix Automático in the modes that have a Pix bank.
func (r rails) subscriptions(pool *pgxpool.Pool, p *payments.Service, e *events.Service, logger *slog.Logger) *subscriptions.Service {
	cfg := subscriptions.Config{Pool: pool, Payments: p, Events: e, Logger: logger}
	if r.livePix != nil {
		cfg.LiveBank = r.livePix
	}
	if r.testPix != nil {
		cfg.TestBank = r.testPix
	}
	return subscriptions.New(cfg)
}

// configure adds the rails that exist to cfg.
func (r rails) configure(cfg *payments.Config) {
	if r.network != nil {
		cfg.LiveRail = r.network
	}
	if r.authenticator != nil {
		cfg.Authenticator = r.authenticator
	}
	pix.Configure(cfg, r.livePix, r.testPix)
}

func (r rails) close() {
	if r.network != nil {
		_ = r.network.Close()
	}
}
