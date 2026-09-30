package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
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

func resolve(p *payments.Service, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := p.Resolve(ctx, pool)
		if n > 0 {
			logger.InfoContext(ctx, "resolved payment operations", "count", n)
		}
		return err
	}
}

func expireAuthorizations(p *payments.Service, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := p.ExpireAuthorizations(ctx, pool)
		if n > 0 {
			logger.InfoContext(ctx, "voided expired authorizations", "count", n)
		}
		return err
	}
}

func retryForwards(c *acquirer.Connector, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := c.RetryForwards(ctx, forwardBatch)
		if n > 0 {
			logger.InfoContext(ctx, "repeated reversals and advices to the card network", "count", n)
		}
		return err
	}
}

func importClearing(c *acquirer.Connector, p *payments.Service, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := c.ImportRecentClearing(ctx, p, clearingDays)
		if n > 0 {
			logger.InfoContext(ctx, "imported clearing files", "days", n)
		}
		return err
	}
}

// tasks are the worker's background loops; the card network's run only when one is
// configured.
func tasks(jobClient *jobs.Client, l *ledger.Ledger, pool *pgxpool.Pool, a *api.API, p *payments.Service, network *acquirer.Connector,
	pixRails []*pix.Connector, logger *slog.Logger, more ...func(context.Context) error,
) []func(context.Context) error {
	background := []func(context.Context) error{
		jobs.Run(jobClient),
		service.Every(logger, "ledger.apply_queued", applyInterval, applyQueued(l, pool)),
		service.Every(logger, "ledger.expire_due", expiryInterval, expireDue(l, pool, logger)),
		service.Every(logger, "ledger.check", checkInterval, check(l, pool, logger)),
		service.Every(logger, "api.complete_abandoned", completerInterval, completeAbandoned(a, logger)),
		service.Every(logger, "api.reap_idempotency_keys", reaperInterval, reap(a, logger)),
		service.Every(logger, "payments.resolve", resolveInterval, resolve(p, pool, logger)),
		service.Every(logger, "payments.expire_authorizations", expireAuthsEvery, expireAuthorizations(p, pool, logger)),
	}
	pixConfigured := false
	for _, c := range pixRails {
		if c == nil {
			continue
		}
		pixConfigured = true
		background = append(background, service.Every(logger, "pix.reconcile", pixReconcileEvery, reconcilePix(c, p, logger)))
	}
	if pixConfigured {
		background = append(background,
			service.Every(logger, "payments.expire_pix_charges", pixExpiryEvery, counted(logger, "settled or expired Pix charges", func(ctx context.Context) (int, error) { return p.ExpirePixCharges(ctx, pool) })),
			service.Every(logger, "payments.return_unmatched_pix", pixReturnsEvery, counted(logger, "returned Pix that paid nothing", func(ctx context.Context) (int, error) { return p.ReturnUnmatchedPix(ctx, pool) })),
			service.Every(logger, "payments.resolve_payouts", payoutsEvery, counted(logger, "resolved payouts", func(ctx context.Context) (int, error) { return p.ResolvePayouts(ctx, pool) })),
		)
	}
	if network != nil {
		background = append(background,
			service.Every(logger, "acquirer.retry_forwards", forwardEvery, retryForwards(network, logger)),
			service.Every(logger, "acquirer.import_clearing", clearingEvery, importClearing(network, p, logger)),
			service.Every(logger, "acquirer.provision_network_tokens", tokensEvery, provisionTokens(network, p, logger)),
		)
	}
	return append(background, more...)
}

// counted runs f and logs how many things it moved.
func counted(logger *slog.Logger, what string, f func(context.Context) (int, error)) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := f(ctx)
		if n > 0 {
			logger.InfoContext(ctx, what, "count", n)
		}
		return err
	}
}

func reconcilePix(c *pix.Connector, p *payments.Service, logger *slog.Logger) func(context.Context) error {
	return counted(logger, "read Pix received from the bank", func(ctx context.Context) (int, error) { return c.Reconcile(ctx, p) })
}

func provisionTokens(c *acquirer.Connector, p *payments.Service, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		n, err := c.ProvisionNetworkTokens(ctx, p, tokensBatch)
		if n > 0 {
			logger.InfoContext(ctx, "provisioned network tokens", "count", n)
		}
		return err
	}
}
