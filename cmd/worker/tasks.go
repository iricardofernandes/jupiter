package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/bank"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/service"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
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

func check(l *ledger.Ledger, pool *pgxpool.Pool, m *monitor, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		report, err := l.Check(ctx, pool, checkOptions)
		for _, v := range report.Violations {
			logger.ErrorContext(ctx, "ledger invariant violated",
				"kind", string(v.Kind), "subject", v.Subject, "detail", v.Detail)
		}
		if err == nil {
			m.checked(len(report.Violations))
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
	pixRails []*pix.Connector, m *monitor, logger *slog.Logger, more ...func(context.Context) error,
) []func(context.Context) error {
	background := []func(context.Context) error{
		jobs.Run(jobClient),
		service.Every(logger, "ledger.apply_queued", applyInterval, applyQueued(l, pool)),
		service.Every(logger, "ledger.expire_due", expiryInterval, expireDue(l, pool, logger)),
		service.Every(logger, "ledger.check", checkInterval, check(l, pool, m, logger)),
		service.Every(logger, "monitor.sample", sampleEvery, m.sample),
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

// advanceReceivables registers units, passes on opt-ins, runs the reconciliations due,
// recovers what recipients owe from their future units, and reports what the
// receivables check finds.
func advanceReceivables(r *receivables.Service, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		err := r.Advance(ctx, pool)
		recovered, recoverErr := r.Recover(ctx, pool)
		if recovered > 0 {
			logger.InfoContext(ctx, "recovered what recipients owed from their future units", "amount", recovered)
		}
		err = errors.Join(err, recoverErr)
		violations, checkErr := r.Check(ctx, pool)
		for _, v := range violations {
			logger.ErrorContext(ctx, "receivables invariant violated", "subject", v.Subject, "detail", v.Detail)
		}
		return errors.Join(err, checkErr)
	}
}

// collect sends each bank the boletos it has not had, and reads its return files.
func collect(p *payments.Service, pool *pgxpool.Pool, logger *slog.Logger, banks ...*bank.Connector) func(context.Context) error {
	return func(ctx context.Context) error {
		var errs []error
		for _, b := range banks {
			if b == nil {
				continue
			}
			sent, err := b.Remit(ctx, p)
			errs = append(errs, err)
			read, err := b.ImportReturns(ctx, pool, p)
			errs = append(errs, err)
			if sent > 0 || read > 0 {
				logger.InfoContext(ctx, "exchanged files with the bank", "remittances", sent, "returns", read)
			}
		}
		return errors.Join(errs...)
	}
}

// advanceDisputes moves disputes on: deadlines that passed, answers to send, and cases to
// ask the network or the bank about.
func advanceDisputes(d *disputes.Service, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		r, err := d.Advance(ctx, pool)
		if r.Expired+r.Sent+r.Refreshed > 0 {
			logger.InfoContext(ctx, "moved disputes on", "expired", r.Expired, "sent", r.Sent, "refreshed", r.Refreshed)
		}
		return err
	}
}

// monitorDisputes reports merchants whose dispute ratio this month is excessive, and what
// the disputes check finds.
func monitorDisputes(d *disputes.Service, pool *pgxpool.Pool, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		found, owners, err := d.Excessive(ctx, pool, time.Now())
		for i, m := range found {
			logger.WarnContext(ctx, "a merchant's dispute ratio is excessive", "merchant", owners[i].Merchant, "livemode", owners[i].Livemode,
				"month", m.Month, "ratio_bps", m.RatioBps, "threshold_bps", m.Threshold.Bps, "events", m.Disputes+m.FraudReports)
		}
		violations, checkErr := d.Check(ctx, pool)
		for _, v := range violations {
			logger.ErrorContext(ctx, "disputes invariant violated", "subject", v.Subject, "detail", v.Detail)
		}
		return errors.Join(err, checkErr)
	}
}

// domainTasks are the loops of subscriptions, receivables, boletos and disputes.
func domainTasks(s services, r rails, pool *pgxpool.Pool, logger *slog.Logger) []func(context.Context) error {
	return []func(context.Context) error{
		service.Every(logger, "subscriptions.advance", subscriptionsEvery, counted(logger, "looked at subscriptions", func(ctx context.Context) (int, error) {
			return s.subscriptions.Advance(ctx, pool)
		})),
		service.Every(logger, "receivables.advance", receivablesEvery, advanceReceivables(s.receivables, pool, logger)),
		service.Every(logger, "bank.collection", collectionEvery, collect(s.payments, pool, logger, r.liveBank, r.testBank)),
		service.Every(logger, "disputes.advance", disputesEvery, advanceDisputes(s.disputes, pool, logger)),
		service.Every(logger, "disputes.reconcile_med", medEvery, counted(logger, "read MED claims from the banks", func(ctx context.Context) (int, error) {
			return s.disputes.ReconcileMED(ctx, pool)
		})),
		service.Every(logger, "disputes.reconcile_cases", medEvery, counted(logger, "read the card network's dispute cases", func(ctx context.Context) (int, error) {
			return s.disputes.ReconcileCases(ctx, pool)
		})),
		service.Every(logger, "disputes.monitor", monitorEvery, monitorDisputes(s.disputes, pool, logger)),
		service.Every(logger, "reconciliation.run", reconcileEvery, reconcile(s.reconciliation, logger)),
	}
}

// reconcile reconciles each mode through yesterday and reports what it found.
func reconcile(r *reconciliation.Service, logger *slog.Logger) func(context.Context) error {
	return func(ctx context.Context) error {
		runs, err := r.RunDue(ctx)
		for _, run := range runs {
			if run.Matched+run.Opened+run.Resolved > 0 {
				logger.InfoContext(ctx, "reconciled", "through", run.Through.Format(time.DateOnly), "matched", run.Matched, "opened", run.Opened, "resolved", run.Resolved)
			}
		}
		return err
	}
}
