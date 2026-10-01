//go:build integration

// Package receivables_test runs card payments through the API in test mode into
// receivable units, registers them with the registry simulator over HTTP, has financiers
// place contracts on them, settles them through the SLC simulator, and pays recipients
// out through the bank simulator.
package receivables_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/receivables"
	"github.com/iricardofernandes/jupiter/internal/recipients"
	"github.com/iricardofernandes/jupiter/internal/reconciliation"
	"github.com/iricardofernandes/jupiter/internal/registry"
	banksim "github.com/iricardofernandes/jupiter/internal/sim/bank"
	registrysim "github.com/iricardofernandes/jupiter/internal/sim/registry"
	slcsim "github.com/iricardofernandes/jupiter/internal/sim/slc"
	"github.com/iricardofernandes/jupiter/internal/slc"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
	"github.com/iricardofernandes/jupiter/pkg/registryapi"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, recipients.Migrate, receivables.Migrate, bank.Migrate, disputes.Migrate, reconciliation.Migrate},
	}))
}

const (
	jupiterCNPJ    = "11222333000181"
	merchantCNPJ   = "11444777000161"
	bankCNPJ       = "33000167000101"
	jupiterToken   = "jupiter registry token"
	financierToken = "jupiter financier token"
	bankToken      = "bank registry token"
	slcToken       = "jupiter slc token" //nolint:gosec // a test credential for the simulator
	jupiterBank    = "jupiter bank token"
	jupiterISPB    = "30000001"
)

// clock starts on Thursday 1 October 2026, noon in Brasília.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t           *testing.T
	pool        *pgxpool.Pool
	clock       *clock
	ledger      *ledger.Ledger
	payments    *payments.Service
	receivables *receivables.Service
	recipients  *recipients.Service
	disputes    *disputes.Service
	recon       *reconciliation.Service
	transfers   *bank.Connector
	registry    *registrysim.Sim
	slc         *slcsim.Sim
	bank        *banksim.Sim
	api         *httptest.Server
	merchants   *merchant.Service
	key         string
	owner       payments.Owner

	mu         sync.Mutex
	bankFaults func(banksim.Event) banksim.Fault
}

// bankFault is what the bank simulator asks before writing a record.
func (h *harness) bankFault(e banksim.Event) banksim.Fault {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.bankFaults == nil {
		return banksim.Fault{}
	}
	return h.bankFaults(e)
}

func (h *harness) setBankFaults(f func(banksim.Event) banksim.Fault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.bankFaults = f
}

// another signs up a second merchant, with taxID if not empty, and returns its test key.
func (h *harness) another(taxID string) string {
	h.t.Helper()
	var key string
	err := postgres.InTx(h.t.Context(), h.pool, func(tx pgx.Tx) error {
		m, keys, err := h.merchants.Create(h.t.Context(), tx, "Outra Loja", api.CurrentVersion)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !k.Livemode && k.Kind == merchant.Secret {
				key = k.Value
			}
		}
		if taxID == "" {
			return nil
		}
		return h.merchants.SetTaxID(h.t.Context(), tx, m.ID, taxID)
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return key
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, pool: server.Pool(t), clock: &clock{now: time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)}}
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	inserter, err := jobs.NewInserter(h.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: h.clock.Now})
	h.ledger = ledger.New(ledger.WithClock(h.clock.Now))

	h.registry = registrysim.New(registrysim.Config{Now: h.clock.Now, Participants: []registrysim.Participant{
		{Token: jupiterToken, TaxID: jupiterCNPJ, Role: registrysim.Accreditor},
		{Token: financierToken, TaxID: jupiterCNPJ, Role: registrysim.Financier},
		{Token: bankToken, TaxID: bankCNPJ, Role: registrysim.Financier},
	}})
	registryServer := httptest.NewServer(h.registry.Handler())
	t.Cleanup(registryServer.Close)
	connector, err := registry.New(registry.Config{BaseURL: registryServer.URL, Token: jupiterToken, FinancierToken: financierToken})
	if err != nil {
		t.Fatal(err)
	}
	settlement, transfers := h.connectSLCAndBank()
	merchants := merchant.New(h.clock.Now)
	h.recipients = recipients.New(recipients.Config{Merchants: merchants, Events: eventService, Now: h.clock.Now})
	h.receivables = receivables.New(receivables.Config{
		Pool: h.pool, Ledger: h.ledger, Merchants: merchants, Recipients: h.recipients, Events: eventService, TaxID: jupiterCNPJ,
		TestRegistry: connector, TestSettlement: settlement, Now: h.clock.Now,
		Domicile: registryapi.Domicile{ISPB: jupiterISPB, Branch: "0001"},
	})
	h.transfers = transfers
	h.payments = payments.New(payments.Config{
		Ledger: h.ledger, Events: eventService, Now: h.clock.Now, Receivables: h.receivables, Balances: h.receivables,
		Recipients: h.recipients, TestRail: payments.NewTestRail(h.pool, h.clock.Now, nil), TestTransfers: transfers, TestBoleto: transfers,
	})
	h.receivables.UsePayments(h.payments)
	h.merchants = merchants
	h.disputes = disputes.New(disputes.Config{Pool: h.pool, Payments: h.payments, Events: eventService, Now: h.clock.Now})
	h.recon = reconciliation.New(reconciliation.Config{Pool: h.pool, Now: h.clock.Now, Test: reconciliation.Mode{
		Streams: []reconciliation.Stream{
			{Counterparty: "slc", Name: "grade", Ours: []reconciliation.OursFunc{h.receivables.Grades}, Theirs: settlement.Grades},
			transfers.Returns(h.payments),
			{Counterparty: "bank", Name: "statement", Theirs: transfers.Statement, Ours: []reconciliation.OursFunc{
				transfers.BoletoCredits(h.payments), h.payments.BankTransferRecords, h.receivables.SettlementCredits,
			}},
		},
		Divergences: map[string]reconciliation.DivergenceFunc{"registry": h.receivables.Divergences},
	}})
	a := api.New(api.Deps{
		Pool: h.pool, Merchants: merchants, Events: eventService, Payments: h.payments, Receivables: h.receivables,
		Recipients: h.recipients, Disputes: h.disputes, Box: box, Now: h.clock.Now,
	})
	h.api = httptest.NewServer(a.Handler())
	t.Cleanup(h.api.Close)
	err = postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
		m, keys, err := merchants.Create(t.Context(), tx, "Loja dos Recebíveis", api.CurrentVersion)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if !k.Livemode && k.Kind == merchant.Secret {
				h.key = k.Value
			}
		}
		h.owner = payments.Owner{Merchant: m.ID}
		return merchants.SetTaxID(t.Context(), tx, m.ID, merchantCNPJ)
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// connectSLCAndBank starts the settlement and bank simulators, with Jupiter taking part.
func (h *harness) connectSLCAndBank() (*slc.Connector, *bank.Connector) {
	h.t.Helper()
	h.slc = slcsim.New(slcsim.Config{
		Now: h.clock.Now, Participants: []slcsim.Participant{{Token: slcToken, TaxID: jupiterCNPJ, ISPB: jupiterISPB}},
		Credit: func(p slcsim.Participant, date string, amount int64) {
			if err := h.bank.Credit(p.TaxID, date, amount, slcsim.CreditReference(date), "LIQUIDACAO SLC"); err != nil {
				h.t.Error(err)
			}
		},
	})
	slcServer := httptest.NewServer(h.slc.Handler())
	h.t.Cleanup(slcServer.Close)
	settlement, err := slc.New(slc.Config{BaseURL: slcServer.URL, Token: slcToken})
	if err != nil {
		h.t.Fatal(err)
	}
	account := cnab240.Account{Branch: "00001", BranchDV: "0", Number: "000000123456", NumberDV: "7"}
	h.bank = banksim.New(banksim.Config{Now: h.clock.Now, Host: "banco.example", Faults: h.bankFault, Clients: []banksim.Client{{
		Token: jupiterBank, TaxID: jupiterCNPJ, Name: "Jupiter Pagamentos", Agreement: "CONVENIO-0001", Account: account,
	}}})
	bankServer := httptest.NewServer(h.bank.Handler())
	h.t.Cleanup(bankServer.Close)
	transfers, err := bank.New(bank.Config{
		BaseURL: bankServer.URL, Token: jupiterBank, Profile: cnab240.Febraban{BankCode: banksim.Code, BankName: "BANCO SIMULADO"},
		Account: account, Agreement: "CONVENIO-0001", TaxID: jupiterCNPJ, Name: "Jupiter Pagamentos", Pool: h.pool, Now: h.clock.Now,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return settlement, transfers
}

type reply struct {
	status int
	body   map[string]any
	raw    []byte
}

func (h *harness) call(method, path string, body any) reply {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequestWithContext(h.t.Context(), method, h.api.URL+path, reader)
	req.Header.Set("Authorization", "Bearer "+h.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	r := reply{status: resp.StatusCode, raw: out}
	_ = json.Unmarshal(out, &r.body)
	return r
}

// pay captures a card payment in installments financed by the merchant.
func (h *harness) pay(amount int64, installments int) string {
	h.t.Helper()
	body := map[string]any{"amount": amount, "currency": "brl", "payment_method": payments.TestCardVisa, "confirm": true}
	if installments > 1 {
		body["installments"] = map[string]any{"count": installments, "financed_by": "merchant"}
	}
	r := h.call(http.MethodPost, "/v1/payment_intents", body)
	if r.status != http.StatusOK || r.body["status"] != "succeeded" {
		h.t.Fatalf("paying: %d %s", r.status, r.raw)
	}
	id, _ := r.body["id"].(string)
	return id
}

func (h *harness) register() {
	h.t.Helper()
	if _, err := h.receivables.Register(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) agenda(asOf time.Time) []receivables.Entry {
	h.t.Helper()
	entries, err := h.receivables.Agenda(h.t.Context(), h.pool, h.owner, receivables.AgendaQuery{
		From: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2027, 10, 1, 0, 0, 0, 0, time.UTC), AsOf: asOf,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return entries
}

// reconcile runs a reconciliation and fails on any divergence.
func (h *harness) reconcile(kind string) receivables.Report {
	h.t.Helper()
	r, err := h.receivables.ReconcileNow(h.t.Context(), h.pool, false, kind)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// consistent checks the ledger, payments and receivables.
func (h *harness) consistent() {
	h.t.Helper()
	report, err := h.ledger.Check(h.t.Context(), h.pool, ledger.CheckOptions{ClearingGrace: 1000 * time.Hour, ExpiryGrace: 1000 * time.Hour})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range report.Violations {
		h.t.Errorf("ledger violation: %+v", v)
	}
	violations, err := h.payments.Check(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range violations {
		h.t.Errorf("payments violation: %+v", v)
	}
	found, err := h.receivables.Check(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range found {
		h.t.Errorf("receivables violation: %+v", v)
	}
	stuck, err := h.disputes.Check(h.t.Context(), h.pool)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, v := range stuck {
		h.t.Errorf("disputes violation: %+v", v)
	}
}
