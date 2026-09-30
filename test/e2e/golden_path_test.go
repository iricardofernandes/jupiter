//go:build e2e

package e2e_test

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
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/id"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/money"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/webhook"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// The golden path, as far as phase 4 reaches: a customer's card goes from their browser
// to the vault, the customer pays R$ 600.00 with it, the merchant captures it, and the
// ledger and a signed webhook both say so. Later phases extend this test with
// installments, receivables, split, settlement and payout.
func TestGoldenPath(t *testing.T) {
	ctx := t.Context()
	pool := server.Pool(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent, AllowPrivateNetworks: true})
	cardVault := vaulttest.Start(t, server.PoolFrom(t, vaulttest.Template), vaulttest.Options{})
	l := ledger.New()
	paymentService := payments.New(payments.Config{
		Ledger: l, Events: eventService, TestRail: payments.NewTestRail(pool, nil, nil).WithCards(cardVault.Client),
	})
	merchants := merchant.New(nil)
	jupiter := httptest.NewServer(api.New(api.Deps{
		Pool: pool, Merchants: merchants, Events: eventService, Payments: paymentService, Vault: cardVault.Client, Box: box,
	}).Handler())
	defer jupiter.Close()

	workers := river.NewWorkers()
	eventService.RegisterWorkers(workers, pool)
	worker, err := jobs.NewWorker(pool, jobs.WorkerConfig{Workers: workers, FetchPollInterval: 50 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- jobs.Run(worker)(workerCtx) }()
	defer func() { stopWorker(); <-workerDone }()

	t.Log("1. a merchant signs up and receives its test keys")
	var secretKey, publishableKey string
	err = postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
		_, keys, err := merchants.Create(ctx, tx, "Loja do Caminho Dourado", api.CurrentVersion)
		for _, k := range keys {
			switch {
			case k.Livemode:
			case k.Kind == merchant.Secret:
				secretKey = k.Value
			case k.Kind == merchant.Publishable:
				publishableKey = k.Value
			}
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{t: t, base: jupiter.URL, key: secretKey}

	t.Log("2. it registers a webhook endpoint")
	delivered := make(chan []byte, 20)
	receiverSecret := make(chan string, 1)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		secret := <-receiverSecret
		receiverSecret <- secret
		if err := webhook.Verify(body, r.Header.Get(webhook.SignatureHeader), secret, webhook.DefaultTolerance, time.Now()); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		delivered <- body
	}))
	defer receiver.Close()
	var endpoint struct{ Secret string }
	client.post("/v1/webhook_endpoints", map[string]any{"url": receiver.URL, "enabled_events": []string{"payment_intent.succeeded"}}, &endpoint)
	receiverSecret <- endpoint.Secret

	t.Log("3. the customer types their card into the checkout page, which sends it to the vault")
	browser := &apiClient{t: t, base: cardVault.PublicURL, key: publishableKey, noIdempotencyKey: true}
	var token struct {
		ID    string `json:"id"`
		Last4 string `json:"last4"`
	}
	browser.post(vault.PublicTokensPath, map[string]any{"number": "4242 4242 4242 4242", "exp_month": 12, "exp_year": 2030, "cvc": "123"}, &token)
	t.Logf("   the page gets back %s, a token for the card ending %s; the number never reaches the merchant or Jupiter's API", token.ID[:8]+"…", token.Last4)

	t.Log("4. the merchant's server saves the card with the token")
	var method struct {
		ID   string `json:"id"`
		Card struct {
			Brand string `json:"brand"`
			BIN   string `json:"bin"`
		} `json:"card"`
	}
	client.post("/v1/payment_methods", map[string]any{"type": "card", "card": map[string]any{"token": token.ID}}, &method)
	if method.Card.Brand != "visa" || method.Card.BIN != "42424242" {
		t.Fatalf("payment method = %+v", method)
	}

	t.Log("5. the customer pays R$ 600.00 with it; the issuer authorizes it")
	var intent struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		AmountCapturable int64  `json:"amount_capturable"`
		AmountReceived   int64  `json:"amount_received"`
	}
	client.post("/v1/payment_intents", map[string]any{
		"amount": 60000, "currency": "brl", "capture_method": "manual",
		"payment_method": method.ID, "confirm": true,
	}, &intent)
	if intent.Status != "requires_capture" || intent.AmountCapturable != 60000 {
		t.Fatalf("after authorization: %+v", intent)
	}
	owner := payments.Owner{Merchant: merchantOf(t, merchants, pool, secretKey)}
	if posted, held := balance(t, paymentService, pool, owner); posted != 0 || held != 60000 {
		t.Fatalf("ledger after authorization: posted %d, held %d; want 0 and 60000", posted, held)
	}
	t.Log("   the ledger holds R$ 600.00 as pending for the merchant")

	t.Log("6. the merchant captures it")
	client.post("/v1/payment_intents/"+intent.ID+"/capture", nil, &intent)
	if intent.Status != "succeeded" || intent.AmountReceived != 60000 {
		t.Fatalf("after capture: %+v", intent)
	}
	if posted, held := balance(t, paymentService, pool, owner); posted != 60000 || held != 0 {
		t.Fatalf("ledger after capture: posted %d, held %d; want 60000 and 0", posted, held)
	}
	t.Log("   the ledger posts R$ 600.00 to the merchant's balance")

	t.Log("7. a signed webhook reports the payment")
	select {
	case body := <-delivered:
		var event struct {
			Type          string              `json:"type"`
			RelatedObject struct{ ID string } `json:"related_object"`
		}
		if err := json.Unmarshal(body, &event); err != nil || event.Type != "payment_intent.succeeded" || event.RelatedObject.ID != intent.ID {
			t.Fatalf("webhook = %s", body)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no webhook arrived")
	}

	t.Log("8. every invariant holds")
	if _, err := l.ApplyQueued(ctx, pool, 10_000); err != nil {
		t.Fatal(err)
	}
	report, err := l.Check(ctx, pool, ledger.CheckOptions{ClearingGrace: time.Hour, ExpiryGrace: time.Hour})
	if err != nil || len(report.Violations) > 0 {
		t.Fatalf("ledger check: %v %+v", err, report.Violations)
	}
	violations, err := paymentService.Check(ctx, pool)
	if err != nil || len(violations) > 0 {
		t.Fatalf("payments check: %v %+v", err, violations)
	}
}

type apiClient struct {
	t    *testing.T
	base string
	key  string
	// noIdempotencyKey is for the browser, whose body holds the card number: the key
	// derived from the body would carry it too.
	noIdempotencyKey bool
}

func (c *apiClient) post(path string, body, out any) {
	c.t.Helper()
	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(c.t.Context(), http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.key)
	if !c.noIdempotencyKey {
		req.Header.Set("Idempotency-Key", path+string(raw))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.t.Fatalf("POST %s = %d %s", path, resp.StatusCode, respBody)
	}
	if err := json.Unmarshal(respBody, out); err != nil {
		c.t.Fatal(err)
	}
}

func merchantOf(t *testing.T, merchants *merchant.Service, q *pgxpool.Pool, key string) id.ID {
	t.Helper()
	p, err := merchants.Authenticate(t.Context(), q, key)
	if err != nil {
		t.Fatal(err)
	}
	return p.Merchant
}

func balance(t *testing.T, s *payments.Service, q *pgxpool.Pool, owner payments.Owner) (posted, held int64) {
	t.Helper()
	b, err := s.MerchantBalance(t.Context(), q, owner, money.BRL)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.Posted()
	if err != nil {
		t.Fatal(err)
	}
	return p.Minor(), b.PendingCredits.Minor()
}
