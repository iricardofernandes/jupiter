//go:build integration

// Package cardrail_test runs live-mode payments through the whole stack and the card
// network simulator: the API, the vault, the acquirer connector over ISO 8583 on TCP,
// and the network's clearing files over HTTP.
package cardrail_test

import (
	"bytes"
	"context"
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

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/sim/cardnetwork"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, acquirer.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// timeout is the acquirer's: short, so timeouts and late answers take little time.
const timeout = 400 * time.Millisecond

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
	network   *cardnetwork.Network
	files     *httptest.Server
	connector *acquirer.Connector
	payments  *payments.Service
	ledger    *ledger.Ledger
	api       *httptest.Server
	liveKey   string
	owner     payments.Owner

	mu     sync.Mutex
	faults func(cardnet.Message) cardnetwork.Fault
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, pool: server.Pool(t), clock: &clock{}}
	h.network = cardnetwork.New(cardnetwork.Config{Now: h.clock.Now, LateAfter: 2 * timeout, Faults: h.fault})
	if err := h.network.Start("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.network.Close)
	h.files = httptest.NewServer(h.network.Handler())
	t.Cleanup(h.files.Close)

	cardVault := vaulttest.Start(t, server.PoolFrom(t, vaulttest.Template), vaulttest.Options{})
	connector, err := acquirer.New(acquirer.Config{
		Pool: h.pool, Addr: h.network.Addr(), ClearingURL: h.files.URL, Timeout: timeout,
		Cards: cardVault.Client, Now: h.clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := connector.Connect(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connector.Close() })
	h.connector = connector

	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	inserter, err := jobs.NewInserter(h.pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, Now: h.clock.Now})
	h.ledger = ledger.New(ledger.WithClock(h.clock.Now))
	h.payments = payments.New(payments.Config{
		Ledger: h.ledger, Events: eventService, Now: h.clock.Now, LiveRail: connector,
		TestRail: payments.NewTestRail(h.pool, h.clock.Now, nil).WithCards(cardVault.Client),
	})
	merchants := merchant.New(h.clock.Now)
	h.api = httptest.NewServer(api.New(api.Deps{
		Pool: h.pool, Merchants: merchants, Events: eventService, Payments: h.payments, Vault: cardVault.Client, Box: box, Now: h.clock.Now,
	}).Handler())
	t.Cleanup(h.api.Close)
	err = postgres.InTx(t.Context(), h.pool, func(tx pgx.Tx) error {
		m, keys, err := merchants.Create(t.Context(), tx, "Loja ao Vivo", api.CurrentVersion)
		for _, k := range keys {
			if k.Livemode && k.Kind == merchant.Secret {
				h.liveKey = k.Value
			}
		}
		h.owner = payments.Owner{Merchant: m.ID, Livemode: true}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) fault(m cardnet.Message) cardnetwork.Fault {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.faults == nil {
		return cardnetwork.NoFault
	}
	return h.faults(m)
}

func (h *harness) setFaults(f func(cardnet.Message) cardnetwork.Fault) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.faults = f
}

type reply struct {
	status int
	body   map[string]any
	raw    []byte
}

func (h *harness) post(path string, body any) reply {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.api.URL+path, bytes.NewReader(raw))
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

func (h *harness) saveCard(number string) string {
	h.t.Helper()
	r := h.post("/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{
		"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123",
	}})
	if r.status != http.StatusOK {
		h.t.Fatalf("saving %s: %d %s", number[12:], r.status, r.raw)
	}
	return str(r.body, "id")
}

// pay creates and confirms a live payment intent, returning it (from the error when
// declined).
func (h *harness) pay(body map[string]any) (int, map[string]any) {
	h.t.Helper()
	body["currency"], body["confirm"] = "brl", true
	r := h.post("/v1/payment_intents", body)
	if r.status == http.StatusPaymentRequired {
		return r.status, obj(obj(r.body, "error"), "payment_intent")
	}
	if r.status != http.StatusOK {
		h.t.Fatalf("paying: %d %s", r.status, r.raw)
	}
	return r.status, r.body
}

func declineCodeOf(it map[string]any) string {
	return str(obj(it, "last_payment_error"), "decline_code")
}

func (h *harness) attempt(intentID string) payments.Attempt {
	h.t.Helper()
	parsed, err := payments.IntentPrefix.Parse(intentID)
	if err != nil {
		h.t.Fatal(err)
	}
	a, err := h.payments.LatestAttempt(h.t.Context(), h.pool, h.owner, parsed)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

func (h *harness) exchange(key string) (state, lateCode string) {
	h.t.Helper()
	if err := h.pool.QueryRow(h.t.Context(), "SELECT state, late_response_code FROM acquirer.exchanges WHERE key = $1", key).
		Scan(&state, &lateCode); err != nil {
		h.t.Fatal(err)
	}
	return state, lateCode
}

func (h *harness) resolve() {
	h.t.Helper()
	h.clock.Advance(2 * time.Minute)
	if _, err := h.connector.RetryForwards(h.t.Context(), 100); err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.payments.Resolve(h.t.Context(), h.pool); err != nil {
		h.t.Fatal(err)
	}
}

// consistent checks the ledger, that payments agree with it, and that the issuer holds
// exactly what Jupiter says is authorized and uncaptured.
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
	var authorized int
	if err := h.pool.QueryRow(h.t.Context(), "SELECT count(*) FROM payments.attempts WHERE status = 'authorized'").Scan(&authorized); err != nil {
		h.t.Fatal(err)
	}
	if holds := h.network.Holds(); len(holds) != authorized {
		h.t.Errorf("the issuer holds %d authorizations, Jupiter has %d uncaptured: %+v", len(holds), authorized, holds)
	}
}

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func obj(m map[string]any, key string) map[string]any {
	o, _ := m[key].(map[string]any)
	return o
}

type nopCards struct{}

func (nopCards) Detokenize(context.Context, string, string) (vault.CardData, error) {
	return vault.CardData{}, nil
}
