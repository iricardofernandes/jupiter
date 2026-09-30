package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

// rails are the counterparties payments reach, each nil when it is not configured: the
// card network, 3-D Secure, and the Pix bank in each mode.
type rails struct {
	network       *acquirer.Connector
	authenticator *authentication.Server
	livePix       *pix.Connector
	testPix       *pix.Connector
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
	return r, nil
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
