//go:build integration

// Package pixrail_test runs live-mode Pix payments, refunds and payouts through the whole
// stack and the Pix simulator: the API, the Pix connector over mutual TLS with
// certificate-bound tokens, the bank's notifications over mutual TLS the other way, and
// payers paying the BR Codes Jupiter shows.
package pixrail_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
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
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/pix"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	pixsim "github.com/iricardofernandes/jupiter/internal/sim/pix"
	"github.com/iricardofernandes/jupiter/internal/subscriptions"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, subscriptions.Migrate},
	}))
}

const (
	clientID   = "jupiter-live"
	secret     = "pix client secret" //nolint:gosec // a test credential for the simulator
	jupiterKey = "7f6e5d4c-3b2a-4190-8f7e-6d5c4b3a2918"
	bankIdent  = "spiffe://sim-pix/webhook"
	sellerKey  = "vendedor@example.com"
	payerTaxID = "12345678909"
)

type clock struct {
	mu     sync.Mutex
	offset time.Duration
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

type harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	clock     *clock
	bank      *pixsim.Sim
	connector *pix.Connector
	payments  *payments.Service
	subs      *subscriptions.Service
	ledger    *ledger.Ledger
	api       *httptest.Server
	liveKey   string

	mu     sync.Mutex
	faults func(pixsim.Event) pixsim.Fault
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, pool: server.Pool(t), clock: &clock{}}
	pki, err := mtls.NewPKI("pixrail")
	if err != nil {
		t.Fatal(err)
	}
	issue := func(issued mtls.Issued, err error) mtls.Issued {
		if err != nil {
			t.Fatal(err)
		}
		return issued
	}

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	inserter, err := jobs.NewInserter(h.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: h.clock.Now})
	h.ledger = ledger.New(ledger.WithClock(h.clock.Now))

	// Jupiter's side of the bank's notifications: mutual TLS admitting the bank only.
	var notifications http.Handler
	webhooks := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { notifications.ServeHTTP(w, r) }))
	webhooks.TLS = mtls.ServerConfig(issue(pki.Server("127.0.0.1")).TLS, pki.Pool(), bankIdent)
	webhooks.StartTLS()
	t.Cleanup(webhooks.Close)

	var bankHandler http.Handler
	bank := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { bankHandler.ServeHTTP(w, r) }))
	bank.TLS = pixsim.ServerTLS(issue(pki.Server("127.0.0.1")).TLS, pki.Pool())
	h.bank, err = pixsim.New(pixsim.Config{
		Now: h.clock.Now, Host: bank.Listener.Addr().String(),
		Clients: []pixsim.Client{{
			ID: clientID, Secret: secret, Name: "Jupiter Pagamentos", TaxID: "11222333000181", Keys: []string{jupiterKey},
			Balance: 100_000_00,
		}},
		WebhookTLS: mtls.ClientConfig(issue(pki.Client(bankIdent)).TLS, pki.Pool()),
		PayerTLS:   &tls.Config{RootCAs: pki.Pool(), MinVersion: tls.VersionTLS12},
		Faults:     h.fault,
	})
	if err != nil {
		t.Fatal(err)
	}
	bankHandler = h.bank.Handler()
	bank.StartTLS()
	t.Cleanup(bank.Close)
	t.Cleanup(h.bank.Close)
	h.bank.AddKey(pixsim.Entry{Key: sellerKey, ISPB: "30000003", Name: "Vendedor da Silva", TaxID: "98765432100"})

	h.connector, err = pix.New(pix.Config{
		BaseURL: bank.URL, ClientID: clientID, ClientSecret: secret, Key: jupiterKey, Account: clientID,
		TLS:        mtls.ClientConfig(issue(pki.Client("spiffe://jupiter/pix")).TLS, pki.Pool()),
		WebhookURL: webhooks.URL + "/pix/live", Livemode: true, Pool: h.pool, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.payments = payments.New(payments.Config{
		Ledger: h.ledger, Events: eventService, Now: h.clock.Now, LivePix: h.connector,
		TestRail: payments.NewTestRail(h.pool, h.clock.Now, nil),
	})
	h.subs = subscriptions.New(subscriptions.Config{Pool: h.pool, Payments: h.payments, Events: eventService, LiveBank: h.connector, Now: h.clock.Now})
	mux := http.NewServeMux()
	mux.Handle("/pix/live/", http.StripPrefix("/pix/live", h.connector.Handler(h.payments, h.subs)))
	notifications = mux
	if err := h.connector.RegisterWebhook(t.Context()); err != nil {
		t.Fatal(err)
	}

	merchants := merchant.New(h.clock.Now)
	a := api.New(api.Deps{Pool: h.pool, Merchants: merchants, Events: eventService, Payments: h.payments, Subscriptions: h.subs, Box: box, Now: h.clock.Now})
	h.api = httptest.NewServer(a.Handler())
	t.Cleanup(h.api.Close)
	err = postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
		_, keys, err := merchants.Create(t.Context(), tx, "Loja Pix", api.CurrentVersion)
		for _, k := range keys {
			if k.Livemode && k.Kind == merchant.Secret {
				h.liveKey = k.Value
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) fault(e pixsim.Event) pixsim.Fault {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.faults == nil {
		return pixsim.Fault{}
	}
	return h.faults(e)
}

func (h *harness) setFaults(f func(pixsim.Event) pixsim.Fault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults = f
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
	req.Header.Set("Authorization", "Bearer "+h.liveKey)
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

// charge creates and confirms a live Pix payment intent, which waits to be paid.
func (h *harness) charge(amount int64, extra map[string]any) map[string]any {
	h.t.Helper()
	body := map[string]any{"amount": amount, "currency": "brl", "payment_method": "pix", "confirm": true}
	for k, v := range extra {
		body[k] = v
	}
	r := h.call(http.MethodPost, "/v1/payment_intents", body)
	if r.status != http.StatusOK || str(r.body, "status") != "requires_action" {
		h.t.Fatalf("charging: %d %s", r.status, r.raw)
	}
	return r.body
}

func qrCode(it map[string]any) string {
	return str(obj(obj(it, "next_action"), "pix_display_qr_code"), "data")
}

// payQR is a customer paying the BR Code shown to them, from their bank.
func (h *harness) payQR(it map[string]any) pixsim.PaymentResult {
	h.t.Helper()
	res, err := h.bank.Pay(context.Background(), pixsim.Payment{BRCode: qrCode(it), PayerName: "Maria", PayerTaxID: payerTaxID})
	if err != nil {
		h.t.Fatal(err)
	}
	return res
}

func (h *harness) intent(id string) map[string]any {
	h.t.Helper()
	r := h.call(http.MethodGet, "/v1/payment_intents/"+id, nil)
	if r.status != http.StatusOK {
		h.t.Fatalf("GET %s: %d %s", id, r.status, r.raw)
	}
	return r.body
}

// waitFor reads an object until its status is status: notifications arrive on their own.
func (h *harness) waitFor(path, status string) map[string]any {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		r := h.call(http.MethodGet, path, nil)
		if str(r.body, "status") == status || time.Now().After(deadline) {
			return r.body
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// consistent checks the ledger, and that payments, refunds, payouts and Pix held apart
// agree with it.
func (h *harness) consistent() {
	h.t.Helper()
	report, err := h.ledger.Check(h.t.Context(), h.pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: 1000 * time.Hour})
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
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func num(m map[string]any, key string) int64 {
	f, _ := m[key].(float64)
	return int64(f)
}

func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}
