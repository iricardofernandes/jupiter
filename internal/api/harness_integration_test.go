//go:build integration

package api_test

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

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/events"
	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/payments"
	"github.com/iricardofernandes/jupiter/internal/platform/jobs"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/secretbox"
	"github.com/iricardofernandes/jupiter/internal/risk"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Main(m, &server, ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, risk.Migrate))
}

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
	api       *api.API
	events    *events.Service
	merchants *merchant.Service
	payments  *payments.Service
	ledger    *ledger.Ledger
	clock     *clock
	vault     *fakeVault
	risk      *risk.Service
	server    *httptest.Server
	keys      map[string]string
}

// harnessOption changes the harness before the API is built.
type harnessOption func(*payments.Config)

// withRail replaces the test rail, for rails that misbehave in ways the test rail does not.
func withRail(r payments.Rail) harnessOption {
	return func(c *payments.Config) { c.TestRail = r }
}

func newHarness(t *testing.T, apiVersion string, opts ...harnessOption) *harness {
	t.Helper()
	pool := server.Pool(t)
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	box, err := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatal(err)
	}
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{}
	h := &harness{t: t, pool: pool, clock: c, merchants: merchant.New(c.Now), vault: newFakeVault(), keys: map[string]string{}}
	h.events = events.New(events.Config{
		Box: box, Jobs: inserter, Render: api.RenderEvent, Now: c.Now,
		AllowPrivateNetworks: true, RetryBase: 20 * time.Millisecond,
	})
	h.ledger = ledger.New(ledger.WithClock(c.Now))
	h.risk = risk.New(risk.Config{Now: c.Now})
	cfg := payments.Config{Ledger: h.ledger, Events: h.events, TestRail: payments.NewTestRail(pool, c.Now, nil).WithCards(h.vault), Risk: h.risk, Now: c.Now}
	for _, opt := range opts {
		opt(&cfg)
	}
	h.payments = payments.New(cfg)
	h.api = api.New(api.Deps{Pool: pool, Merchants: h.merchants, Events: h.events, Payments: h.payments, Vault: h.vault, Risk: h.risk, Box: box, Now: c.Now})
	h.server = httptest.NewServer(h.api.Handler())
	t.Cleanup(h.server.Close)
	h.newMerchant(apiVersion)
	return h
}

// newMerchant replaces the harness's keys with those of a new merchant.
func (h *harness) newMerchant(apiVersion string) {
	h.t.Helper()
	err := postgres.InTx(h.t.Context(), h.pool, func(tx pgx.Tx) error {
		_, keys, err := h.merchants.Create(h.t.Context(), tx, "Loja Teste", apiVersion)
		for _, k := range keys {
			h.keys[k.Value[:8]] = k.Value
		}
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) startWorker() {
	h.t.Helper()
	workers := river.NewWorkers()
	h.events.RegisterWorkers(workers, h.pool)
	client, err := jobs.NewWorker(h.pool, jobs.WorkerConfig{Workers: workers, FetchPollInterval: 20 * time.Millisecond})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- jobs.Run(client)(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
}

type response struct {
	status int
	header http.Header
	body   []byte
}

func (r response) decode(t *testing.T, v any) {
	t.Helper()
	if err := json.Unmarshal(r.body, v); err != nil {
		t.Fatalf("decoding %s: %v", r.body, err)
	}
}

type call struct {
	method  string
	path    string
	key     string
	body    any
	headers map[string]string
}

func (h *harness) do(c call) response {
	h.t.Helper()
	resp, err := h.send(h.t.Context(), c)
	if err != nil {
		h.t.Fatalf("%s %s: %v", c.method, c.path, err)
	}
	return resp
}

func (h *harness) send(ctx context.Context, c call) (response, error) {
	var body io.Reader
	switch b := c.body.(type) {
	case nil:
	case string:
		body = bytes.NewBufferString(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			return response{}, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, c.method, h.server.URL+c.path, body)
	if err != nil {
		return response{}, err
	}
	key := c.key
	if key == "" {
		key = h.keys["sk_test_"]
	}
	if key != "-" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	return response{status: resp.StatusCode, header: resp.Header, body: raw}, err
}

func (h *harness) expect(c call, status int) response {
	h.t.Helper()
	resp := h.do(c)
	if resp.status != status {
		h.t.Fatalf("%s %s = %d %s, want %d", c.method, c.path, resp.status, resp.body, status)
	}
	return resp
}

var (
	specOnce sync.Once
	spec     *openapi3.T
	errSpec  error
)

func loadSpec(t *testing.T) *openapi3.T {
	t.Helper()
	specOnce.Do(func() {
		loader := openapi3.NewLoader()
		spec, errSpec = loader.LoadFromFile("../../api/openapi.yaml")
		if errSpec == nil {
			errSpec = spec.Validate(context.Background())
		}
	})
	if errSpec != nil {
		t.Fatalf("openapi.yaml: %v", errSpec)
	}
	return spec
}

// conforms checks a current-version response body against a schema of the OpenAPI
// document, so the document and the server cannot drift apart unnoticed.
func conforms(t *testing.T, schema string, body []byte) {
	t.Helper()
	ref, ok := loadSpec(t).Components.Schemas[schema]
	if !ok {
		t.Fatalf("no schema %s", schema)
	}
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if err := ref.Value.VisitJSON(v); err != nil {
		t.Fatalf("response does not conform to %s: %v\n%s", schema, err, body)
	}
}
