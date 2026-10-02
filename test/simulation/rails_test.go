//go:build simulation

package simulation_test

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	mrand "math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/bank"
	"github.com/iricardofernandes/jupiter/internal/disputes"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/registry"
	banksim "github.com/iricardofernandes/jupiter/internal/sim/bank"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	slcsim "github.com/iricardofernandes/jupiter/internal/sim/slc"
	"github.com/iricardofernandes/jupiter/internal/slc"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

// The rails simulation covers every rail, in test mode, where each is a
// simulator: card payments in installments whose receivables are registered and settled
// through the SLC into Jupiter's bank account; Pix charges, refunds, payouts, payments
// that match nothing and MED claims; boletos; chargebacks. The simulators run on clocks
// skewed from Jupiter's, lose and repeat notifications, statement lines and return
// records, and leave transfers pending; background work crashes part-way; and every
// rail ends reconciled.
//
// SIM_RAIL_SCENARIOS (300 by default) sets how many scenarios a run starts.

const (
	railJupiterCNPJ  = "11222333000181"
	railMerchantCNPJ = "11444777000161"
	railISPB         = "30000001"
	railPixClient    = "jupiter-test"
	railPixSecret    = "simulation pix secret"
	railPixKey       = "5a4b3c2d-1e0f-4a9b-8c7d-6e5f4a3b2c1d"
	railBankIdent    = "spiffe://sim-pix/webhook"
	railBankToken    = "simulation bank token"
	railRegistry     = "simulation registry token"
	railFinancier    = "simulation financier token"
	railSLCToken     = "simulation slc token" //nolint:gosec // a test credential for the simulator
	// railPixFunds is what Jupiter's account at the Pix bank starts with.
	railPixFunds = 1_000_000_00
	// railPayoutKey is where the merchant's payouts go.
	railPayoutKey = "+5511912345678"
)

func TestRailsSimulation(t *testing.T) {
	scenarios := int(min(envUint("SIM_RAIL_SCENARIOS", 300), 100_000))
	for _, seed := range seeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			t.Logf("seed %d, %d scenarios; replay with SIM_SEED=%d SIM_RAIL_SCENARIOS=%d make test-simulation", seed, scenarios, seed, scenarios)
			r := newRails(t, seed)
			if path := os.Getenv("SIM_TRACE"); path != "" {
				t.Cleanup(func() {
					if err := os.WriteFile(path, r.trace, 0o600); err != nil { //nolint:gosec // a path the developer chose
						t.Error(err)
					}
				})
			}
			digest := r.run(scenarios)
			t.Logf("trace digest %x; %s", digest, r.stats)
		})
	}
}

// A run's trace must be the same for the same seed, across every rail.
func TestRailsSimulationIsDeterministic(t *testing.T) {
	const seed, scenarios = 7, 25
	first := newRails(t, seed).run(scenarios)
	second := newRails(t, seed).run(scenarios)
	if first != second {
		t.Fatalf("seed %d produced two different traces: %x and %x", seed, first, second)
	}
}

type rails struct {
	t        *testing.T
	seed     uint64
	rng      *mrand.Rand
	pool     *pgxpool.Pool
	clock    *clock
	crashes  *crashes
	ledger   *ledger.Ledger
	payments *payments.Service
	receiv   *receivables.Service
	disputes *disputes.Service
	recon    *reconciliation.Service
	api      *api.API
	handler  http.Handler
	pixConn  *pix.Connector
	bankConn *bank.Connector
	pix      *pixsim.Sim
	bank     *banksim.Sim
	slc      *slcsim.Sim
	registry *registrysim.Sim
	key      string

	scenarios []*scenario
	// faulty is off while the run drains: the rails then behave, and everything must
	// settle.
	faulty bool
	// injected are the breaks the faults must leave in reconciliation, as "counterparty
	// stream kind key".
	injected map[string]int
	ordinals map[string]int

	mu    sync.Mutex
	trace []byte
	crash bool
	stats railStats
}

type railStats struct {
	requests, crashes, duplicates, backgroundCrashes, backgroundErrors, faults, days int
	kinds, outcomes                                                                  map[string]int
}

func (s railStats) String() string {
	return fmt.Sprintf("%d requests, %d crashed between phases, %d duplicates, %d background runs crashed and %d failed, %d faults on the rails, %d days, scenarios %v, final %v",
		s.requests, s.crashes, s.duplicates, s.backgroundCrashes, s.backgroundErrors, s.faults, s.days, s.kinds, s.outcomes)
}

func (r *rails) record(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trace = fmt.Appendf(r.trace, format+"\n", args...)
}

// hash draws a fault from the seed and what it is about, not from the run's generator:
// simulators decide on their own goroutines.
func (r *rails) hash(parts ...string) uint64 {
	h := fnv.New64a()
	_ = binary.Write(h, binary.LittleEndian, r.seed)
	for _, p := range parts {
		_, _ = h.Write([]byte(p + "\x00"))
	}
	return h.Sum64() % 1000
}

// skew is how far a simulator's clock runs from Jupiter's: up to two minutes either way.
func (r *rails) skew(name string) func() time.Time {
	d := time.Duration(int64(r.hash("skew", name)%241)-120) * time.Second
	r.record("clock %s skewed %v", name, d)
	return func() time.Time { return r.clock.Now().Add(d) }
}

func newRails(t *testing.T, seed uint64) *rails {
	t.Helper()
	url, drop, err := server.Database(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = drop(context.WithoutCancel(t.Context())) })
	r := &rails{
		t: t, seed: seed, rng: mrand.New(mrand.NewPCG(seed, seed^0x51ed270b27f3a5c1)), //nolint:gosec // reproducibility is the point
		clock: &clock{now: time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC)}, crashes: &crashes{},
		faulty: true, injected: map[string]int{}, ordinals: map[string]int{}, stats: railStats{kinds: map[string]int{}, outcomes: map[string]int{}},
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = r.crashes
	if r.pool, err = pgxpool.NewWithConfig(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.pool.Close)

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(r.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: r.clock.Now})
	r.ledger = ledger.New(ledger.WithClock(r.clock.Now))
	merchants := merchant.New(r.clock.Now)
	recipientService := recipients.New(recipients.Config{Merchants: merchants, Events: eventService, Now: r.clock.Now})

	r.startBank()
	settlement := r.startSLC()
	registryConn := r.startRegistry()
	r.receiv = receivables.New(receivables.Config{
		Pool: r.pool, Ledger: r.ledger, Merchants: merchants, Recipients: recipientService, Events: eventService, TaxID: railJupiterCNPJ,
		TestRegistry: registryConn, TestSettlement: settlement, Now: r.clock.Now, Domicile: registryapi.Domicile{ISPB: railISPB, Branch: "0001"},
	})
	webhooks := r.startPix()
	r.payments = payments.New(payments.Config{
		Ledger: r.ledger, Events: eventService, Now: r.clock.Now, Receivables: r.receiv, Balances: r.receiv, Recipients: recipientService,
		TestRail: &railCards{rails: r, inner: payments.NewTestRail(r.pool, r.clock.Now, nil)},
		TestPix:  r.pixConn, TestBoleto: r.bankConn, TestTransfers: r.bankConn,
	})
	r.receiv.UsePayments(r.payments)
	subs := subscriptions.New(subscriptions.Config{Pool: r.pool, Payments: r.payments, Events: eventService, TestBank: r.pixConn, Now: r.clock.Now})
	r.disputes = disputes.New(disputes.Config{Pool: r.pool, Payments: r.payments, Events: eventService, TestBank: r.pixConn, Now: r.clock.Now})
	r.recon = reconciliation.New(reconciliation.Config{Pool: r.pool, Now: r.clock.Now, Test: reconciliation.Mode{
		Streams: []reconciliation.Stream{
			{Counterparty: "slc", Name: "grade", Ours: []reconciliation.OursFunc{r.receiv.Grades}, Theirs: settlement.Grades},
			r.bankConn.Returns(r.payments),
			{Counterparty: "bank", Name: "statement", Theirs: r.bankConn.Statement, Ours: []reconciliation.OursFunc{
				r.bankConn.BoletoCredits(r.payments), r.payments.BankTransferRecords, r.receiv.SettlementCredits,
			}},
			{Counterparty: "pix_bank", Name: "statement", Ours: []reconciliation.OursFunc{r.payments.PixRecords}, Theirs: r.pixConn.Statement},
		},
		Divergences: map[string]reconciliation.DivergenceFunc{"registry": r.receiv.Divergences},
	}})
	webhooks.Handle("/pix/test/", http.StripPrefix("/pix/test", r.pixConn.Handler(r.payments, subs, r.disputes)))
	if err := r.pixConn.RegisterWebhook(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.api = api.New(api.Deps{
		Pool: r.pool, Merchants: merchants, Events: eventService, Payments: r.payments, Receivables: r.receiv, Recipients: recipientService,
		Disputes: r.disputes, Subscriptions: subs, Reconciliation: r.recon, Box: box, Now: r.clock.Now, IdempotencyWait: time.Nanosecond,
		AfterPhase: func(string) { r.afterPhase() },
	})
	r.handler = r.api.Handler()
	err = postgres.InTx(t.Context(), r.pool, func(tx pgx.Tx) error {
		m, keys, err := merchants.Create(t.Context(), tx, "Loja de Todos os Trilhos", api.CurrentVersion)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !k.Livemode && k.Kind == merchant.Secret {
				r.key = k.Value
			}
		}
		if err := merchants.SetTaxID(t.Context(), tx, m.ID, railMerchantCNPJ); err != nil {
			return err
		}
		return ownDestination(t.Context(), tx, recipientService, recipients.Owner{Merchant: m.ID}, railPayoutKey)
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// draw numbers the events of each kind a counterparty sees, and decides from the seed
// and that number what happens to one: per1000 of them, for each of whats, in turn. The
// order of events is the run's, so a seed draws the same faults again, whatever
// identifiers the rails made up.
func (r *rails) draw(counterparty, kind string, per1000 map[string]uint64, whats ...string) (string, int) {
	r.mu.Lock()
	n := r.ordinals[counterparty+"/"+kind]
	r.ordinals[counterparty+"/"+kind]++
	faulty := r.faulty
	r.mu.Unlock()
	if !faulty {
		return "", n
	}
	for _, what := range whats {
		if r.hash(counterparty, kind, strconv.Itoa(n), what) < per1000[what] {
			return what, n
		}
	}
	return "", n
}

// expect notes a break the faults must leave in reconciliation.
func (r *rails) expect(counterparty, stream, kind, key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.injected[strings.Join([]string{counterparty, stream, kind, key}, " ")]++
}

func (r *rails) startBank() {
	account := cnab240.Account{Branch: "00001", BranchDV: "0", Number: "000000123456", NumberDV: "7"}
	r.bank = banksim.New(banksim.Config{Now: r.skew("bank"), Host: "banco.example", Faults: r.bankFault, Clients: []banksim.Client{{
		Token: railBankToken, TaxID: railJupiterCNPJ, Name: "Jupiter Pagamentos", Agreement: "CONVENIO-0001", Account: account,
	}}})
	srv := httptest.NewServer(r.bank.Handler())
	r.t.Cleanup(srv.Close)
	var err error
	r.bankConn, err = bank.New(bank.Config{
		BaseURL: srv.URL, Token: railBankToken, Profile: cnab240.Febraban{BankCode: banksim.Code, BankName: "BANCO SIMULADO"},
		Account: account, Agreement: "CONVENIO-0001", TaxID: railJupiterCNPJ, Name: "Jupiter Pagamentos", Pool: r.pool, Now: r.clock.Now,
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

// bankFault duplicates and delays statement lines and return records, and loses
// statement lines: each a break reconciliation must find, or a late one it must forgive.
func (r *rails) bankFault(e banksim.Event) banksim.Fault {
	what, n := r.draw("bank", e.Kind, map[string]uint64{"duplicate": 15, "delay": 30, "drop": 10}, "duplicate", "delay", "drop")
	var f banksim.Fault
	switch {
	case what == "duplicate" && e.Kind == "statement":
		f.Duplicate = true
		r.expect("bank", "statement", reconciliation.Duplicate, e.Reference)
	case what == "duplicate":
		f.Duplicate = true
		if e.Paid {
			r.expect("bank", "cnab_return", reconciliation.Duplicate, e.Reference)
		}
	case what == "delay":
		f.Delay = true
	case what == "drop" && e.Kind == "statement":
		f.Drop = true
		r.expect("bank", "statement", reconciliation.MissingAtCounterparty, e.Reference)
	default:
		return f
	}
	r.noteFault("bank", e.Kind, n, what)
	return f
}

func (r *rails) startSLC() *slc.Connector {
	r.slc = slcsim.New(slcsim.Config{
		Now: r.skew("slc"), Participants: []slcsim.Participant{{Token: railSLCToken, TaxID: railJupiterCNPJ, ISPB: railISPB}},
		Credit: func(p slcsim.Participant, date string, amount int64) {
			if err := r.bank.Credit(p.TaxID, date, amount, slcsim.CreditReference(date), "LIQUIDACAO SLC"); err != nil {
				r.t.Error(err)
			}
		},
	})
	srv := httptest.NewServer(r.slc.Handler())
	r.t.Cleanup(srv.Close)
	connector, err := slc.New(slc.Config{BaseURL: srv.URL, Token: railSLCToken})
	if err != nil {
		r.t.Fatal(err)
	}
	return connector
}

func (r *rails) startRegistry() *registry.Connector {
	r.registry = registrysim.New(registrysim.Config{Now: r.skew("registry"), Participants: []registrysim.Participant{
		{Token: railRegistry, TaxID: railJupiterCNPJ, Role: registrysim.Accreditor},
		{Token: railFinancier, TaxID: railJupiterCNPJ, Role: registrysim.Financier},
	}})
	srv := httptest.NewServer(r.registry.Handler())
	r.t.Cleanup(srv.Close)
	connector, err := registry.New(registry.Config{BaseURL: srv.URL, Token: railRegistry, FinancierToken: railFinancier})
	if err != nil {
		r.t.Fatal(err)
	}
	return connector
}

// startPix runs the Pix bank over mutual TLS both ways, and returns the mux Jupiter's
// side of its notifications is served on.
func (r *rails) startPix() *http.ServeMux {
	pki, err := mtls.NewPKI("simulation-pix")
	if err != nil {
		r.t.Fatal(err)
	}
	issue := func(issued mtls.Issued, err error) mtls.Issued {
		if err != nil {
			r.t.Fatal(err)
		}
		return issued
	}
	webhookMux := http.NewServeMux()
	webhooks := httptest.NewUnstartedServer(webhookMux)
	webhooks.TLS = mtls.ServerConfig(issue(pki.Server("127.0.0.1")).TLS, pki.Pool(), railBankIdent)
	webhooks.StartTLS()
	r.t.Cleanup(webhooks.Close)

	var bankHandler http.Handler
	bankServer := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { bankHandler.ServeHTTP(w, req) }))
	bankServer.TLS = pixsim.ServerTLS(issue(pki.Server("127.0.0.1")).TLS, pki.Pool())
	r.pix, err = pixsim.New(pixsim.Config{
		Now: r.skew("pix"), Host: bankServer.Listener.Addr().String(),
		Clients: []pixsim.Client{{
			ID: railPixClient, Secret: railPixSecret, Name: "Jupiter Pagamentos", TaxID: railJupiterCNPJ, Keys: []string{railPixKey}, Balance: railPixFunds,
		}},
		WebhookTLS: mtls.ClientConfig(issue(pki.Client(railBankIdent)).TLS, pki.Pool()),
		PayerTLS:   &tls.Config{RootCAs: pki.Pool(), MinVersion: tls.VersionTLS12},
		Faults:     r.pixFault,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	bankHandler = r.pix.Handler()
	bankServer.StartTLS()
	r.t.Cleanup(bankServer.Close)
	r.t.Cleanup(r.pix.Close)
	r.pix.AddKey(pixsim.Entry{Key: railPayoutKey, ISPB: "30000003", Name: "Loja de Todos os Trilhos", TaxID: railMerchantCNPJ})
	r.pixConn, err = pix.New(pix.Config{
		BaseURL: bankServer.URL, ClientID: railPixClient, ClientSecret: railPixSecret, Key: railPixKey, Account: railPixClient,
		TLS:        mtls.ClientConfig(issue(pki.Client("spiffe://jupiter/pix")).TLS, pki.Pool()),
		WebhookURL: webhooks.URL + "/pix/test", Pool: r.pool, Now: r.clock.Now,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return webhookMux
}

// pixFault loses notifications, duplicates and delays statement lines, and leaves
// transfers and returns pending, or their answers lost.
func (r *rails) pixFault(e pixsim.Event) pixsim.Fault {
	var f pixsim.Fault
	var what string
	var n int
	switch e.Kind {
	case "webhook", "infraction":
		what, n = r.draw("pix", e.Kind, map[string]uint64{"drop": 80}, "drop")
		f.Drop = what == "drop"
	case "statement":
		what, n = r.draw("pix", e.Kind, map[string]uint64{"duplicate": 15, "delay": 30}, "duplicate", "delay")
		f.Duplicate, f.Delay = what == "duplicate", what == "delay"
		if f.Duplicate {
			r.expect("pix_bank", "statement", reconciliation.Duplicate, e.ID)
		}
	case "transfer", "return":
		what, n = r.draw("pix", e.Kind, map[string]uint64{"pending": 60, "lose": 30}, "pending", "lose")
		f.Pending, f.LoseResponse = what == "pending", what == "lose"
		if f.Pending {
			f.PendingFor = 20 * time.Minute
		}
	default:
		_, n = r.draw("pix", e.Kind, nil)
	}
	if what != "" {
		r.noteFault("pix", e.Kind, n, what)
	}
	return f
}

func (r *rails) noteFault(counterparty, kind string, n int, what string) {
	r.mu.Lock()
	r.stats.faults++
	r.mu.Unlock()
	r.record("fault %s %s #%d: %s", counterparty, kind, n, what)
}

// ownDestination gives the merchant's own recipient, which its balance is paid out to, a
// Pix key.
func ownDestination(ctx context.Context, tx pgx.Tx, s *recipients.Service, owner recipients.Owner, key string) error {
	me, err := s.Default(ctx, tx, owner)
	if err != nil {
		return err
	}
	_, err = s.Update(ctx, tx, owner, me.ID, recipients.Params{Destination: &recipients.Destination{Method: "pix", PixKey: key}})
	return err
}
