//go:build integration

// Package pci_test holds the tests that keep card data where docs/pci-scope.md says it
// is: in the vault, and nowhere else.
package pci_test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// canary returns a Luhn-valid number that exists nowhere but in this test run, so
// finding it anywhere means this run put it there.
func canary() string {
	var b strings.Builder
	b.WriteString("4")
	for range 14 {
		n, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			panic(err)
		}
		b.WriteString(n.String())
	}
	digits := b.String()
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 0 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return digits + strconv.Itoa((10-sum%10)%10)
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	vault    *vaulttest.Vault
	payments *payments.Service
	api      *httptest.Server
	keys     map[string]string
	logs     *lockedBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	logs := &lockedBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e := &env{t: t, pool: server.Pool(t), logs: logs, keys: map[string]string{}}
	e.vault = vaulttest.Start(t, server.PoolFrom(t, vaulttest.Template), vaulttest.Options{Logger: logger})
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(e.pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent})
	e.payments = payments.New(payments.Config{
		Ledger: ledger.New(), Events: eventService,
		TestRail: payments.NewTestRail(e.pool, nil, logger).WithCards(e.vault.Client),
	})
	merchants := merchant.New(nil)
	e.api = httptest.NewServer(api.New(api.Deps{
		Pool: e.pool, Merchants: merchants, Events: eventService, Payments: e.payments, Vault: e.vault.Client, Box: box, Logger: logger,
	}).Handler())
	t.Cleanup(e.api.Close)
	err = postgres.InTx(t.Context(), e.pool, func(tx pgx.Tx) error {
		_, keys, err := merchants.Create(t.Context(), tx, "Loja Canário", api.CurrentVersion)
		for _, k := range keys {
			e.keys[k.Value[:8]] = k.Value
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) post(url, key, idempotencyKey string, body any) (int, map[string]any) {
	e.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(e.t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+key)
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(out, &decoded)
	return resp.StatusCode, decoded
}

func (e *env) jupiter(path, idempotencyKey string, body any) (int, map[string]any) {
	return e.post(e.api.URL+path, e.keys["sk_test_"], idempotencyKey, body)
}

func card(number string) map[string]any {
	return map[string]any{"type": "card", "card": map[string]any{"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123"}}
}

func (e *env) saveCard(number, idempotencyKey string) string {
	e.t.Helper()
	status, pm := e.jupiter("/v1/payment_methods", idempotencyKey, card(number))
	if status != http.StatusOK {
		e.t.Fatalf("saving a card: %d %v", status, pm)
	}
	id, ok := pm["id"].(string)
	if !ok {
		e.t.Fatalf("saving a card answered %v", pm)
	}
	return id
}

// Exit criterion: a canary card number goes through every path by which a card reaches
// Jupiter, and the API's database, every table of every schema, never holds it; nor do
// the logs of the API, the rail and the vault.
func TestTheAPIDatabaseNeverHoldsACardNumber(t *testing.T) {
	e := newEnv(t)
	serverSide, declined, webPage, paid := canary(), canary(), canary(), "4242424242424242"

	// A card saved by number, retried with the same Idempotency-Key, and one saved
	// without a key.
	pm := e.saveCard(serverSide, "canary-1")
	if again := e.saveCard(serverSide, "canary-1"); again != pm {
		t.Fatalf("the retry saved another card: %s, %s", pm, again)
	}
	e.saveCard(serverSide, "")
	// Requests that fail: an expired card, a malformed request, and a number sent where
	// a token belongs.
	if status, _ := e.jupiter("/v1/payment_methods", "canary-2", map[string]any{
		"type": "card",
		"card": map[string]any{"number": declined, "exp_month": 1, "exp_year": 2020},
	}); status != http.StatusPaymentRequired {
		t.Fatalf("an expired card: %d", status)
	}
	if status, _ := e.jupiter("/v1/payment_methods", "canary-3", map[string]any{
		"type": "card",
		"card": map[string]any{"number": declined, "token": "tok_x"},
	}); status != http.StatusBadRequest {
		t.Fatalf("a malformed request: %d", status)
	}
	if status, _ := e.jupiter("/v1/payment_methods", "canary-3b", map[string]any{
		"type": "card", "card": map[string]any{"token": declined},
	}); status != http.StatusBadRequest {
		t.Fatalf("a number as a token: %d", status)
	}
	// A payment the rail declines, since the canary is not a test card.
	if status, it := e.jupiter("/v1/payment_intents", "canary-4", map[string]any{
		"amount": 1000, "currency": "brl", "payment_method": e.saveCard(declined, ""), "confirm": true,
	}); status != http.StatusPaymentRequired {
		t.Fatalf("paying with the canary: %d %v", status, it)
	}
	// A card from a web page, through the vault's public route and a claim.
	status, token := e.post(e.vault.PublicURL+vault.PublicTokensPath, e.keys["pk_test_"], "",
		map[string]any{"number": webPage, "exp_month": 12, "exp_year": 2030, "cvc": "123"})
	if status != http.StatusOK {
		t.Fatalf("tokenizing from a web page: %d %v", status, token)
	}
	if status, pm := e.jupiter("/v1/payment_methods", "canary-5", map[string]any{"type": "card", "card": map[string]any{"token": token["id"]}}); status != http.StatusOK {
		t.Fatalf("claiming the token: %d %v", status, pm)
	}
	// A payment that succeeds end to end: authorization, capture and refund.
	status, it := e.jupiter("/v1/payment_intents", "canary-6", map[string]any{
		"amount": 60000, "currency": "brl", "payment_method": e.saveCard(paid, ""), "confirm": true, "capture_method": "manual",
	})
	if status != http.StatusOK || it["status"] != "requires_capture" {
		t.Fatalf("authorizing: %d %v", status, it)
	}
	intentID, _ := it["id"].(string)
	if status, it := e.jupiter("/v1/payment_intents/"+intentID+"/capture", "canary-7", nil); status != http.StatusOK || it["status"] != "succeeded" {
		t.Fatalf("capturing: %d %v", status, it)
	}
	if status, re := e.jupiter("/v1/refunds", "canary-8", map[string]any{"payment_intent": it["id"], "amount": 1000}); status != http.StatusOK {
		t.Fatalf("refunding: %d %v", status, re)
	}

	for _, number := range []string{serverSide, declined, webPage, paid} {
		if n := rowsContaining(t, e.pool, number); n > 0 {
			t.Errorf("card number ending %s is in %d rows of the API's database", number[12:], n)
		}
		if n := rowsContaining(t, e.vault.Pool, number); n > 0 {
			t.Errorf("card number ending %s is in the clear in %d rows of the vault's database", number[12:], n)
		}
		if bytes.Contains([]byte(e.logs.String()), []byte(number)) {
			t.Errorf("card number ending %s is in the logs", number[12:])
		}
	}
	// The scan itself works: the BIN, which the API may keep, is found.
	if rowsContaining(t, e.pool, paid[:8]) == 0 {
		t.Fatal("the scan does not find the BIN of the paid card, which the API stores")
	}
}

// rowsContaining counts the rows, in every table of every schema, whose text form holds
// needle as text or as hex bytes.
func rowsContaining(t *testing.T, pool *pgxpool.Pool, needle string) int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT quote_ident(table_schema) || '.' || quote_ident(table_name)
		FROM information_schema.tables
		WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, table := range tables {
		var n int
		query := fmt.Sprintf("SELECT count(*) FROM %s AS r WHERE r::text LIKE '%%' || $1 || '%%' OR r::text LIKE '%%' || $2 || '%%'", table)
		if err := pool.QueryRow(t.Context(), query, needle, hex.EncodeToString([]byte(needle))).Scan(&n); err != nil {
			t.Fatal(err)
		}
		found += n
	}
	return found
}
