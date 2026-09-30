//go:build integration

// Package risk_test runs the load generator's card-testing burst against the API.
package risk_test

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

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
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
	"github.com/iricardofernandes/jupiter/pkg/loadgen"
)

var server *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Templates(m, &server, map[string][]postgrestest.MigrateFunc{
		postgrestest.DefaultTemplate: {ledger.Migrate, merchant.Migrate, events.Migrate, payments.Migrate, api.Migrate, jobs.Migrate, risk.Migrate},
		vaulttest.Template:           {vaulttest.Migrate},
	}))
}

// goodCards are test cards the test rail approves: a legitimate merchant's customers.
var goodCards = []loadgen.Card{
	{Number: "4242424242424242", ExpMonth: 12, ExpYear: 2030},
	{Number: "4000056655665556", ExpMonth: 11, ExpYear: 2029},
	{Number: "5555555555554444", ExpMonth: 10, ExpYear: 2031},
	{Number: "2223003122003222", ExpMonth: 9, ExpYear: 2030},
	{Number: "378282246310005", ExpMonth: 8, ExpYear: 2030},
	{Number: "6362970000457013", ExpMonth: 7, ExpYear: 2030},
	{Number: "6062825624254001", ExpMonth: 6, ExpYear: 2030},
}

// Phase 6 exit criterion: a card-testing burst from the load generator is detected and
// throttled, a legitimate merchant paying at the same time is not, and the decision log
// explains every block.
func TestACardTestingBurstIsThrottled(t *testing.T) {
	ctx := t.Context()
	pool := server.Pool(t)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	box, _ := secretbox.FromBase64(base64.StdEncoding.EncodeToString(key))
	inserter, err := jobs.NewInserter(pool, nil)
	if err != nil {
		t.Fatal(err)
	}
	cardVault := vaulttest.Start(t, server.PoolFrom(t, vaulttest.Template), vaulttest.Options{})
	eventService := events.New(events.Config{Box: box, Jobs: inserter, Render: api.RenderEvent})
	engine := risk.New(risk.Config{})
	paymentService := payments.New(payments.Config{
		Ledger: ledger.New(), Events: eventService, Risk: engine,
		TestRail: payments.NewTestRail(pool, nil, nil).WithCards(cardVault.Client),
	})
	merchants := merchant.New(nil)
	jupiter := httptest.NewServer(api.New(api.Deps{
		Pool: pool, Merchants: merchants, Events: eventService, Payments: paymentService, Vault: cardVault.Client, Risk: engine, Box: box,
	}).Handler())
	defer jupiter.Close()
	newMerchant := func(name string) string {
		var secret string
		err := postgres.InTx(ctx, pool, func(tx pgx.Tx) error {
			_, keys, err := merchants.Create(ctx, tx, name, api.CurrentVersion)
			for _, k := range keys {
				if k.Kind == merchant.Secret && !k.Livemode {
					secret = k.Value
				}
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return secret
	}
	tester := loadgen.Client{BaseURL: jupiter.URL, Key: newMerchant("Loja Atacada")}
	honest := loadgen.Client{BaseURL: jupiter.URL, Key: newMerchant("Loja Tranquila")}

	var burst, steady *loadgen.Report
	var wg sync.WaitGroup
	wg.Go(func() { burst = loadgen.CardTesting(ctx, tester, 200, 8, 42) })
	wg.Go(func() { steady = loadgen.Steady(ctx, honest, goodCards, 30, 2) })
	wg.Wait()
	t.Logf("card testing: %s", burst)
	t.Logf("steady:       %s", steady)

	if len(burst.Errors) > 0 || len(steady.Errors) > 0 {
		t.Fatalf("errors: %v %v", burst.Errors, steady.Errors)
	}
	if burst.Blocked < 150 || burst.Succeeded > 0 {
		t.Fatalf("the burst was not throttled: %s", burst)
	}
	if steady.Succeeded != 30 {
		t.Fatalf("the honest merchant was affected: %s", steady)
	}

	// The decision log explains every block.
	var blocks, throttled int
	after := ""
	for {
		page := decisions(t, jupiter.URL, tester.Key, after)
		for _, d := range page.Data {
			blocks++
			if len(d.Rules) == 0 {
				t.Fatalf("decision %s blocked without a rule", d.ID)
			}
			for _, r := range d.Rules {
				if r.Description == "" {
					t.Fatalf("decision %s: rule %s has no explanation", d.ID, r.ID)
				}
				if r.ID == "card_testing_throttle" {
					throttled++
				}
			}
			after = d.ID
		}
		if !page.HasMore {
			break
		}
	}
	if blocks != burst.Blocked {
		t.Fatalf("the log has %d blocks, the load generator saw %d", blocks, burst.Blocked)
	}
	if throttled == 0 {
		t.Fatal("no block came from the card-testing throttle")
	}
}

type decisionPage struct {
	HasMore bool `json:"has_more"`
	Data    []struct {
		ID    string `json:"id"`
		Rules []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"rules"`
	} `json:"data"`
}

func decisions(t *testing.T, base, key, after string) decisionPage {
	t.Helper()
	url := base + "/v1/risk/decisions?action=block&limit=100"
	if after != "" {
		url += "&starting_after=" + after
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var page decisionPage
	if resp.StatusCode != http.StatusOK || json.Unmarshal(raw, &page) != nil {
		t.Fatalf("GET %s = %d %s", url, resp.StatusCode, raw)
	}
	return page
}
