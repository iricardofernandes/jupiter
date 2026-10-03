package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/authentication"
	"github.com/iricardofernandes/jupiter/internal/bank"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/registry"
	"github.com/iricardofernandes/jupiter/internal/risk"
	"github.com/iricardofernandes/jupiter/internal/slc"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

// rails are the counterparties payments reach, each nil when it is not configured: the
// card network, 3-D Secure, and in each mode the Pix bank, the registry, the bank for
// boletos and transfers, and the settlement system.
type rails struct {
	network       *acquirer.Connector
	authenticator *authentication.Server
	livePix       *pix.Connector
	testPix       *pix.Connector
	liveRegistry  *registry.Connector
	testRegistry  *registry.Connector
	liveBank      *bank.Connector
	testBank      *bank.Connector
	liveSLC       *slc.Connector
	testSLC       *slc.Connector
	payoutLimit   int64
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
	if r.liveBank, r.testBank, err = bank.Banks(os.Getenv, pool, logger); err != nil {
		r.close()
		return rails{}, err
	}
	if r.liveSLC, r.testSLC, err = slc.Connectors(os.Getenv); err != nil {
		r.close()
		return rails{}, err
	}
	if r.payoutLimit, err = payments.DailyPayoutLimit(os.Getenv); err != nil {
		r.close()
		return rails{}, err
	}
	return r, nil
}

// services are the domain services both the api and the worker build alike.
type services struct {
	merchants      *merchant.Service
	payments       *payments.Service
	subscriptions  *subscriptions.Service
	receivables    *receivables.Service
	recipients     *recipients.Service
	disputes       *disputes.Service
	reconciliation *reconciliation.Service
}

func (r rails) services(pool *pgxpool.Pool, l *ledger.Ledger, e *events.Service, riskEngine *risk.Service, cards *vault.Client, logger *slog.Logger) services {
	cfg := payments.Config{Ledger: l, Events: e, Risk: riskEngine, TestRail: payments.NewTestRail(pool, nil, logger).WithCards(cards), Logger: logger}
	r.configure(&cfg)
	s := services{merchants: merchant.New(nil)}
	s.recipients = recipients.New(recipients.Config{Merchants: s.merchants, Events: e})
	s.receivables = r.receivables(pool, l, s.merchants, s.recipients, e, logger)
	cfg.Receivables, cfg.Balances, cfg.Recipients = s.receivables, s.receivables, s.recipients
	s.payments = payments.New(cfg)
	s.receivables.UsePayments(s.payments)
	s.subscriptions = r.subscriptions(pool, s.payments, e, logger)
	s.disputes = r.disputes(pool, s.payments, e, logger)
	s.reconciliation = reconciliation.New(reconciliation.Config{
		Pool: pool, Live: r.reconciled(true, s), Test: r.reconciled(false, s), Logger: logger,
	})
	return s
}

// reconciled is what is reconciled in a mode: each counterparty configured in it.
func (r rails) reconciled(livemode bool, s services) reconciliation.Mode {
	var m reconciliation.Mode
	slcConn, bankConn, pixConn, registryConn := r.testSLC, r.testBank, r.testPix, r.testRegistry
	if livemode {
		slcConn, bankConn, pixConn, registryConn = r.liveSLC, r.liveBank, r.livePix, r.liveRegistry
		if r.network != nil {
			m.Streams = append(m.Streams, r.network.Clearing(s.payments))
		}
	}
	if slcConn != nil {
		m.Streams = append(m.Streams, reconciliation.Stream{
			Counterparty: "slc", Name: "grade", Ours: []reconciliation.OursFunc{s.receivables.Grades}, Theirs: slcConn.Grades,
		})
	}
	if bankConn != nil {
		ours := []reconciliation.OursFunc{bankConn.BoletoCredits(s.payments), s.payments.BankTransferRecords}
		if slcConn != nil {
			ours = append(ours, s.receivables.SettlementCredits)
		}
		m.Streams = append(m.Streams, bankConn.Returns(s.payments),
			reconciliation.Stream{Counterparty: "bank", Name: "statement", Ours: ours, Theirs: bankConn.Statement})
	}
	if pixConn != nil {
		m.Streams = append(m.Streams, reconciliation.Stream{
			Counterparty: "pix_bank", Name: "statement", Ours: []reconciliation.OursFunc{s.payments.PixRecords}, Theirs: pixConn.Statement,
		})
	}
	if registryConn != nil {
		m.Divergences = map[string]reconciliation.DivergenceFunc{"registry": s.receivables.Divergences}
	}
	return m
}

// disputes keeps chargebacks from the card network in live mode, the test network's in
// test mode, and MED claims from each mode's Pix bank.
func (r rails) disputes(pool *pgxpool.Pool, p *payments.Service, e *events.Service, logger *slog.Logger) *disputes.Service {
	cfg := disputes.Config{Pool: pool, Payments: p, Events: e, Logger: logger}
	if r.network != nil {
		cfg.LiveNetwork = r.network
	}
	if r.livePix != nil {
		cfg.LiveBank = r.livePix
	}
	if r.testPix != nil {
		cfg.TestBank = r.testPix
	}
	return disputes.New(cfg)
}

// receivables keeps card receivables, registered in the modes that have a registry.
func (r rails) receivables(pool *pgxpool.Pool, l *ledger.Ledger, merchants *merchant.Service, recs *recipients.Service, e *events.Service, logger *slog.Logger) *receivables.Service {
	cfg := receivables.Config{
		Pool: pool, Ledger: l, Merchants: merchants, Recipients: recs, Events: e, TaxID: os.Getenv("JUPITER_TAX_ID"),
		Domicile: registry.Domicile(os.Getenv), Logger: logger,
	}
	registry.Configure(&cfg, r.liveRegistry, r.testRegistry)
	slc.Configure(&cfg, r.liveSLC, r.testSLC)
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
	cfg.DailyPayoutLimit = r.payoutLimit
	if r.network != nil {
		cfg.LiveRail = r.network
	}
	if r.authenticator != nil {
		cfg.Authenticator = r.authenticator
	}
	pix.Configure(cfg, r.livePix, r.testPix)
	bank.Configure(cfg, r.liveBank, r.testBank)
}

func (r rails) close() {
	if r.network != nil {
		_ = r.network.Close()
	}
}
