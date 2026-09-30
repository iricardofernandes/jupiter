//go:build integration

package merchant_test

import (
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/merchant"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
)

var server *postgrestest.Server

func TestMain(m *testing.M) { os.Exit(postgrestest.Main(m, &server, merchant.Migrate)) }

func create(t *testing.T, pool *pgxpool.Pool, s *merchant.Service) (merchant.Merchant, map[string]merchant.IssuedKey) {
	t.Helper()
	var m merchant.Merchant
	var keys []merchant.IssuedKey
	err := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
		var err error
		m, keys, err = s.Create(t.Context(), tx, "Loja da Esquina", "2026-09-30")
		return err
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	byPrefix := map[string]merchant.IssuedKey{}
	for _, k := range keys {
		byPrefix[k.Value[:8]] = k
	}
	return m, byPrefix
}

func TestCreateIssuesSecretAndPublishableKeysForBothModes(t *testing.T) {
	pool := server.Pool(t)
	s := merchant.New(nil)
	m, keys := create(t, pool, s)

	for prefix, wantLive := range map[string]bool{"sk_test_": false, "pk_test_": false, "sk_live_": true, "pk_live_": true} {
		k, ok := keys[prefix]
		if !ok {
			t.Fatalf("no %s key in %v", prefix, keys)
		}
		p, err := s.Authenticate(t.Context(), pool, k.Value)
		if err != nil {
			t.Fatalf("Authenticate(%s…): %v", prefix, err)
		}
		if p.Merchant != m.ID || p.Livemode != wantLive || p.APIVersion != "2026-09-30" {
			t.Errorf("%s principal = %+v", prefix, p)
		}
	}
	if !slices.Equal(keys["sk_test_"].Scopes, merchant.AllScopes) || len(keys["pk_test_"].Scopes) != 0 {
		t.Errorf("secret scopes %v, publishable scopes %v", keys["sk_test_"].Scopes, keys["pk_test_"].Scopes)
	}
}

func TestNoKeyValueIsStored(t *testing.T) {
	pool := server.Pool(t)
	_, keys := create(t, pool, merchant.New(nil))

	var dump strings.Builder
	rows, err := pool.Query(t.Context(), "SELECT row_to_json(k)::text FROM merchant.api_keys k")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		dump.WriteString(row)
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	for _, k := range keys {
		if strings.Contains(dump.String(), k.Value) || strings.Contains(dump.String(), k.Value[8:]) {
			t.Fatalf("the api_keys table contains key %s…", k.Value[:12])
		}
	}
}

func TestAuthenticateRejects(t *testing.T) {
	pool := server.Pool(t)
	s := merchant.New(nil)
	_, keys := create(t, pool, s)
	secret := keys["sk_test_"]

	revoked := keys["sk_live_"]
	p, err := s.Authenticate(t.Context(), pool, secret.Value)
	if err != nil {
		t.Fatal(err)
	}
	livePrincipal := p
	livePrincipal.Livemode = true
	if err := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
		_, changed, revokeErr := s.RevokeKey(t.Context(), tx, livePrincipal, revoked.ID)
		if !changed {
			t.Error("the first revocation reported no change")
		}
		return revokeErr
	}); err != nil {
		t.Fatalf("RevokeKey: %v", err)
	}

	for name, value := range map[string]string{
		"empty":            "",
		"malformed":        "sk_test_short",
		"wrong mode label": strings.Replace(secret.Value, "_test_", "_prod_", 1),
		"unknown":          "sk_test_" + strings.Repeat("A", 43),
		"revoked":          revoked.Value,
	} {
		if _, err := s.Authenticate(t.Context(), pool, value); !errors.Is(err, merchant.ErrInvalidKey) {
			t.Errorf("%s: error = %v, want ErrInvalidKey", name, err)
		}
	}
}

func TestRestrictedKeysCannotExceedTheirIssuer(t *testing.T) {
	pool := server.Pool(t)
	s := merchant.New(nil)
	_, keys := create(t, pool, s)
	secret, err := s.Authenticate(t.Context(), pool, keys["sk_test_"].Value)
	if err != nil {
		t.Fatal(err)
	}

	issue := func(p merchant.Principal, scopes ...merchant.Scope) (merchant.IssuedKey, error) {
		var k merchant.IssuedKey
		txErr := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
			var issueErr error
			k, issueErr = s.CreateRestrictedKey(t.Context(), tx, p, "reader", scopes)
			return issueErr
		})
		return k, txErr
	}
	reader, err := issue(secret, merchant.ScopeEventsRead)
	if err != nil {
		t.Fatalf("CreateRestrictedKey: %v", err)
	}
	if !strings.HasPrefix(reader.Value, "rk_test_") {
		t.Fatalf("restricted key = %s…", reader.Value[:8])
	}
	readerPrincipal, err := s.Authenticate(t.Context(), pool, reader.Value)
	if err != nil {
		t.Fatal(err)
	}
	if !readerPrincipal.Can(merchant.ScopeEventsRead) || readerPrincipal.Can(merchant.ScopeEventsWrite) {
		t.Fatalf("restricted principal scopes = %v", readerPrincipal.Scopes)
	}
	if _, err := issue(readerPrincipal, merchant.ScopeAPIKeysWrite); !errors.Is(err, merchant.ErrInvalid) {
		t.Errorf("escalating through a restricted key: error = %v, want ErrInvalid", err)
	}
	if _, err := issue(secret, "payments:everything"); !errors.Is(err, merchant.ErrInvalid) {
		t.Errorf("unknown scope: error = %v, want ErrInvalid", err)
	}
	if _, err := issue(secret); !errors.Is(err, merchant.ErrInvalid) {
		t.Errorf("no scope: error = %v, want ErrInvalid", err)
	}
}

func TestListKeysPaginatesAndSeparatesModes(t *testing.T) {
	pool := server.Pool(t)
	s := merchant.New(nil)
	_, keys := create(t, pool, s)
	p, err := s.Authenticate(t.Context(), pool, keys["sk_test_"].Value)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if txErr := postgres.InTx(t.Context(), pool, func(tx pgx.Tx) error {
			_, issueErr := s.CreateRestrictedKey(t.Context(), tx, p, "extra", []merchant.Scope{merchant.ScopeEventsRead})
			return issueErr
		}); txErr != nil {
			t.Fatal(txErr)
		}
	}

	var seen []string
	r := page.Request{Limit: 2}
	for {
		items, more, listErr := s.ListKeys(t.Context(), pool, p, r)
		if listErr != nil {
			t.Fatal(listErr)
		}
		for _, k := range items {
			if k.Livemode {
				t.Fatalf("a test-mode list returned live key %s", k.ID)
			}
			seen = append(seen, k.ID.String())
		}
		if !more {
			break
		}
		r.StartingAfter = items[len(items)-1].ID.String()
	}
	if len(seen) != 5 {
		t.Fatalf("listed %d test keys, want 5 (secret, publishable, 3 restricted): %v", len(seen), seen)
	}
	if !slices.IsSortedFunc(seen, func(a, b string) int { return strings.Compare(b, a) }) {
		t.Fatalf("keys not newest first: %v", seen)
	}

	back, _, err := s.ListKeys(t.Context(), pool, p, page.Request{Limit: 2, EndingBefore: seen[4]})
	if err != nil {
		t.Fatal(err)
	}
	if len(back) != 2 || back[0].ID.String() != seen[2] || back[1].ID.String() != seen[3] {
		t.Fatalf("ending_before page = %v, want %v", back, seen[2:4])
	}
}
