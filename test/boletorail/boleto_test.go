//go:build integration

// Package boletorail_test runs boletos through the whole stack and the bank simulator:
// issued through the API, sent in a CNAB 240 remittance, registered with a Pix QR code,
// paid, and reported in the bank's return files.
package boletorail_test

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
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	banksim "github.com/iricardofernandes/jupiter/internal/sim/bank"
	"github.com/iricardofernandes/jupiter/pkg/cnab240"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, bank.Migrate},
	}))
}

const (
	bankToken = "jupiter bank token"
	jupiterID = "11222333000181"
)

var account = cnab240.Account{Branch: "00001", BranchDV: "0", Number: "000000123456", NumberDV: "7"}

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
	t         *testing.T
	pool      *pgxpool.Pool
	clock     *clock
	ledger    *ledger.Ledger
	payments  *payments.Service
	bank      *banksim.Sim
	connector *bank.Connector
	api       *httptest.Server
	key       string
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
	h.bank = banksim.New(banksim.Config{Now: h.clock.Now, Host: "banco.example", Clients: []banksim.Client{{
		Token: bankToken, TaxID: jupiterID, Name: "Jupiter Pagamentos", Agreement: "CONVENIO-0001", Account: account,
	}}})
	bankServer := httptest.NewServer(h.bank.Handler())
	t.Cleanup(bankServer.Close)
	h.connector, err = bank.New(bank.Config{
		BaseURL: bankServer.URL, Token: bankToken, Profile: cnab240.Febraban{BankCode: banksim.Code, BankName: "BANCO SIMULADO"},
		Account: account, Agreement: "CONVENIO-0001", TaxID: jupiterID, Name: "Jupiter Pagamentos", Pool: h.pool, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.payments = payments.New(payments.Config{
		Ledger: h.ledger, Events: eventService, Now: h.clock.Now, TestBoleto: h.connector,
		TestRail: payments.NewTestRail(h.pool, h.clock.Now, nil),
	})
	merchants := merchant.New(h.clock.Now)
	a := api.New(api.Deps{Pool: h.pool, Merchants: merchants, Events: eventService, Payments: h.payments, Box: box, Now: h.clock.Now})
	h.api = httptest.NewServer(a.Handler())
	t.Cleanup(h.api.Close)
	err = postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
		_, keys, err := merchants.Create(t.Context(), tx, "Loja do Boleto", api.CurrentVersion)
		for _, k := range keys {
			if !k.Livemode && k.Kind == merchant.Secret {
				h.key = k.Value
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) call(method, path string, body any) (int, map[string]any) {
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
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func obj(m map[string]any, k string) map[string]any {
	o, _ := m[k].(map[string]any)
	return o
}

// issue creates and confirms a hybrid boleto due in two weeks.
func (h *harness) issue(amount int64) map[string]any {
	h.t.Helper()
	status, it := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{
		"amount": amount, "currency": "brl", "payment_method": "boleto", "confirm": true,
		"boleto": map[string]any{"due_date": "2026-10-15", "days_after_due": 5, "payer": map[string]any{"name": "Maria da Silva", "tax_id": "12345678909"}},
	})
	if status != http.StatusOK || str(it, "status") != "requires_action" {
		h.t.Fatalf("issuing: %d %v", status, it)
	}
	return it
}

// day lets the bank close a day, and Jupiter send its remittance and read the returns.
func (h *harness) day() {
	h.t.Helper()
	if _, err := h.connector.Remit(h.t.Context(), h.payments); err != nil {
		h.t.Fatal(err)
	}
	h.bank.Tick()
	if _, err := h.connector.ImportReturns(h.t.Context(), h.pool, h.payments); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) intent(id string) map[string]any {
	h.t.Helper()
	_, it := h.call(http.MethodGet, "/v1/payment_intents/"+id, nil)
	return it
}

func (h *harness) consistent() {
	h.t.Helper()
	if _, err := h.ledger.ApplyQueued(h.t.Context(), h.pool, 10_000); err != nil {
		h.t.Fatal(err)
	}
	report, err := h.ledger.Check(h.t.Context(), h.pool, ledger.CheckOptions{ClearingGrace: 1000 * time.Hour, ExpiryGrace: 1000 * time.Hour})
	if err != nil || len(report.Violations) > 0 {
		h.t.Fatalf("ledger: %v %+v", err, report.Violations)
	}
	violations, err := h.payments.Check(h.t.Context(), h.pool)
	if err != nil || len(violations) > 0 {
		h.t.Fatalf("payments: %v %+v", err, violations)
	}
}

// The plan's boleto scenario: issued, sent in a remittance, registered with a Pix QR
// code, paid by Pix, reported paid in the return file, and posted to the ledger.
func TestAHybridBoletoPaidByPix(t *testing.T) {
	h := newHarness(t)
	it := h.issue(15000)
	details := obj(obj(it, "next_action"), "boleto_display_details")
	if len(str(details, "line")) != 47 || len(str(details, "barcode")) != 44 || details["pix_code"] != nil {
		t.Fatalf("before registration: %v", details)
	}
	h.day()
	details = obj(obj(h.intent(str(it, "id")), "next_action"), "boleto_display_details")
	pix := str(details, "pix_code")
	if pix == "" {
		t.Fatalf("after registration: %v", details)
	}
	if _, err := h.bank.PayPix(pix); err != nil {
		t.Fatal(err)
	}
	h.day()
	paid := h.intent(str(it, "id"))
	if str(paid, "status") != "succeeded" || paid["amount_received"] != float64(15000) {
		t.Fatalf("after the return: %v", paid)
	}
	if status, _ := h.call(http.MethodPost, "/v1/refunds", map[string]any{"payment_intent": str(it, "id")}); status == http.StatusOK {
		t.Fatal("a boleto refunded")
	}
	h.consistent()
}

// A boleto is not split: only card payments are.
func TestABoletoIsNotSplit(t *testing.T) {
	h := newHarness(t)
	status, out := h.call(http.MethodPost, "/v1/payment_intents", map[string]any{
		"amount": 1000, "currency": "brl", "payment_method": "boleto",
		"boleto": map[string]any{"due_date": "2026-10-15", "payer": map[string]any{"name": "Maria da Silva", "tax_id": "12345678909"}},
		"split":  []map[string]any{{"recipient": "me", "percentage": "100.00", "remainder": true, "liable": true, "charge_fee": true}},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("a split boleto: %d %v", status, out)
	}
}

// Paid by its typed line, a boleto is the same payment.
func TestABoletoPaidByItsLine(t *testing.T) {
	h := newHarness(t)
	it := h.issue(9900)
	h.day()
	if _, err := h.bank.Pay(str(obj(obj(it, "next_action"), "boleto_display_details"), "line")); err != nil {
		t.Fatal(err)
	}
	h.day()
	if paid := h.intent(str(it, "id")); str(paid, "status") != "succeeded" {
		t.Fatalf("after the return: %v", paid)
	}
	h.consistent()
}

// Canceled before it was sent, a boleto is never sent; canceled once registered, it is
// written off at the bank; unpaid past its term, the bank writes it off.
func TestBoletosThatAreNotPaid(t *testing.T) {
	h := newHarness(t)
	early := h.issue(1000)
	if status, it := h.call(http.MethodPost, "/v1/payment_intents/"+str(early, "id")+"/cancel", nil); status != http.StatusOK || str(it, "status") != "processing" {
		t.Fatalf("canceling an unsent boleto: %d %v", status, it)
	}
	registered := h.issue(2000)
	expiring := h.issue(3000)
	h.day()
	if str(h.intent(str(early, "id")), "status") != "canceled" {
		t.Fatalf("never sent: %v", h.intent(str(early, "id")))
	}
	h.call(http.MethodPost, "/v1/payment_intents/"+str(registered, "id")+"/cancel", nil)
	h.day()
	if it := h.intent(str(registered, "id")); str(it, "status") != "canceled" {
		t.Fatalf("written off on request: %v", it)
	}
	h.clock.Advance(20 * 24 * time.Hour)
	h.day()
	if it := h.intent(str(expiring, "id")); str(it, "status") != "requires_payment_method" || str(obj(it, "last_payment_error"), "decline_code") != "boleto_expired" {
		t.Fatalf("past its term: %v", it)
	}
	h.consistent()
}
