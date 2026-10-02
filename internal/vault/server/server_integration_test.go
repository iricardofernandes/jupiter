//go:build integration

package server_test

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/platform/mtls"
	"github.com/iricardofernandes/jupiter/internal/platform/postgres/postgrestest"
	"github.com/iricardofernandes/jupiter/internal/platform/ratelimit"
	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/server"
	"github.com/iricardofernandes/jupiter/internal/vault/vaulttest"
)

var srv *postgrestest.Server

func TestMain(m *testing.M) {
	os.Exit(postgrestest.Main(m, &srv, server.Migrate))
}

const (
	visa  = "4242424242424242"
	owner = "mch_test/test"
)

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

func start(t *testing.T) (*vaulttest.Vault, *clock) {
	t.Helper()
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	return vaulttest.Start(t, srv.Pool(t), vaulttest.Options{Now: c.Now}), c
}

func card(number, cvc string) vault.CardData {
	return vault.CardData{Number: number, ExpMonth: 12, ExpYear: 2030, CVC: cvc}
}

func tokenize(t *testing.T, v *vaulttest.Vault, key string, c vault.CardData) vault.Card {
	t.Helper()
	got, err := v.Client.Tokenize(t.Context(), vault.TokenizeRequest{Owner: owner, RequestKey: key, Card: c})
	if err != nil {
		t.Fatalf("Tokenize: %v", err)
	}
	return got
}

func TestTokenizeAndDetokenize(t *testing.T) {
	v, _ := start(t)
	ctx := t.Context()
	c := tokenize(t, v, "req-1", card("4242 4242 4242 4242", "123"))
	if !strings.HasPrefix(c.Token, "tok_") || c.Owner != owner || c.Brand != "visa" || c.BIN != "42424242" ||
		c.Last4 != "4242" || c.ExpMonth != 12 || c.ExpYear != 2030 || len(c.Fingerprint) != 32 || c.PublishableKey != "" {
		t.Fatalf("card = %+v", c)
	}

	first, err := v.Client.Detokenize(ctx, c.Token, owner)
	if err != nil || first.Number != visa || first.CVC != "123" || first.ExpMonth != 12 || first.ExpYear != 2030 {
		t.Fatalf("first Detokenize = %v %q, %v", first, first.CVC, err)
	}
	second, err := v.Client.Detokenize(ctx, c.Token, owner)
	if err != nil || second.Number != visa || second.CVC != "" {
		t.Fatalf("second Detokenize = %v, cvc %q, %v; the security code must be given once", second, second.CVC, err)
	}

	for name, call := range map[string]func() error{
		"another owner": func() error { _, err := v.Client.Detokenize(ctx, c.Token, "mch_other/test"); return err },
		"no owner":      func() error { _, err := v.Client.Detokenize(ctx, c.Token, ""); return err },
		"unknown token": func() error { _, err := v.Client.Detokenize(ctx, "tok_00000000000000000000000000", owner); return err },
		"not a token":   func() error { _, err := v.Client.Card(ctx, "../../etc"); return err },
	} {
		if err := call(); !errors.Is(err, vault.ErrNotFound) {
			t.Errorf("%s: %v, want ErrNotFound", name, err)
		}
	}
}

func TestTokenizationIsIdempotentByRequestKey(t *testing.T) {
	v, _ := start(t)
	a := tokenize(t, v, "req-1", card(visa, "123"))
	again := tokenize(t, v, "req-1", card(visa, "123"))
	if again.Token != a.Token {
		t.Fatalf("a repeated request made a second token: %s and %s", a.Token, again.Token)
	}
	_, err := v.Client.Tokenize(t.Context(), vault.TokenizeRequest{Owner: owner, RequestKey: "req-1", Card: card("5555555555554444", "")})
	if !errors.Is(err, vault.ErrConflict) {
		t.Fatalf("a request key reused with another card: %v, want ErrConflict", err)
	}
	b := tokenize(t, v, "req-2", card(visa, ""))
	if b.Token == a.Token || b.Fingerprint != a.Fingerprint {
		t.Fatalf("the same card under a new key: tokens %s %s, fingerprints %s %s; want new token, same fingerprint",
			a.Token, b.Token, a.Fingerprint, b.Fingerprint)
	}
	other := tokenize(t, v, "req-3", card("5555555555554444", ""))
	if other.Fingerprint == a.Fingerprint {
		t.Fatal("two cards share a fingerprint")
	}
}

func TestInvalidCardsAreRefusedByField(t *testing.T) {
	v, _ := start(t)
	tests := map[string]struct {
		card        vault.CardData
		code, param string
	}{
		"bad checksum": {card("4242424242424241", ""), "incorrect_number", "number"},
		"expired":      {vault.CardData{Number: visa, ExpMonth: 8, ExpYear: 2026}, "expired_card", "exp_month"},
		"bad month":    {vault.CardData{Number: visa, ExpMonth: 13, ExpYear: 2030}, "invalid_expiry_month", "exp_month"},
		"bad cvc":      {card(visa, "12"), "invalid_cvc", "cvc"},
	}
	for name, tt := range tests {
		_, err := v.Client.Tokenize(t.Context(), vault.TokenizeRequest{Owner: owner, RequestKey: name, Card: tt.card})
		var ce *vault.CardError
		if !errors.As(err, &ce) || ce.Code != tt.code || ce.Param != tt.param || !errors.Is(err, vault.ErrInvalidCard) {
			t.Errorf("%s: %v, want %s on %s", name, err, tt.code, tt.param)
		}
		if strings.Contains(fmt.Sprint(err), tt.card.Number) {
			t.Errorf("%s: the error repeats the card number: %v", name, err)
		}
	}
	if _, err := v.Client.Tokenize(t.Context(), vault.TokenizeRequest{Owner: owner, Card: card(visa, "")}); err == nil || errors.Is(err, vault.ErrUnavailable) {
		t.Errorf("a tokenization without a request key: %v, want a refusal", err)
	}
}

// The vault's own database holds the number only encrypted, and the security code not
// at all.
func TestTheVaultDatabaseHoldsNoCardInTheClear(t *testing.T) {
	v, _ := start(t)
	c := tokenize(t, v, "req-1", card(visa, "737"))
	if scan(t, v.Pool, c.BIN) == 0 {
		t.Fatal("the scan does not find even the BIN, which is stored in the clear")
	}
	if n := scan(t, v.Pool, visa); n > 0 {
		t.Fatalf("the card number appears in the clear in %d rows of the vault database", n)
	}
	var columns int
	err := v.Pool.QueryRow(t.Context(), `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'vault' AND (column_name LIKE '%cvc%' OR column_name LIKE '%cvv%' OR column_name LIKE '%security%')`).Scan(&columns)
	if err != nil || columns != 0 {
		t.Fatalf("the vault schema has %d columns for a security code (%v)", columns, err)
	}
	// Moving a ciphertext to another row does not open it: it is bound to its token.
	d := tokenize(t, v, "req-2", card("5555555555554444", ""))
	if _, err := v.Pool.Exec(t.Context(), `UPDATE vault.cards SET encrypted_number = (SELECT encrypted_number FROM vault.cards WHERE token = $1),
		wrapped_key = (SELECT wrapped_key FROM vault.cards WHERE token = $1) WHERE token = $2`, c.Token, d.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Client.Detokenize(t.Context(), d.Token, owner); !errors.Is(err, vault.ErrUnavailable) {
		t.Fatalf("a ciphertext copied to another row opened: %v", err)
	}
}

// scan counts rows, in every table of every schema, whose text form holds needle as
// text or as hex bytes.
func scan(t *testing.T, pool *pgxpool.Pool, needle string) int {
	t.Helper()
	rows, err := pool.Query(t.Context(), `SELECT quote_ident(table_schema) || '.' || quote_ident(table_name)
		FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, table := range tables {
		var n int
		if err := pool.QueryRow(t.Context(), "SELECT count(*) FROM "+table+" AS r WHERE r::text LIKE '%' || $1 || '%' OR r::text LIKE '%' || $2 || '%'",
			needle, hex.EncodeToString([]byte(needle))).Scan(&n); err != nil {
			t.Fatal(err)
		}
		found += n
	}
	return found
}

func TestSecurityCodesLiveOnlyInMemoryAndExpire(t *testing.T) {
	v, clk := start(t)
	ctx := t.Context()
	expiring := tokenize(t, v, "req-1", card(visa, "123"))
	clk.Advance(31 * time.Minute)
	if got, err := v.Client.Detokenize(ctx, expiring.Token, owner); err != nil || got.CVC != "" {
		t.Fatalf("a security code outlived its time: %q, %v", got.CVC, err)
	}

	kept := tokenize(t, v, "req-2", card(visa, "456"))
	restarted := server.New(server.Config{Pool: v.Pool, KMS: v.KMS, Now: clk.Now})
	if got, err := restarted.Detokenize(ctx, kept.Token, owner); err != nil || got.CVC != "" || got.Number != visa {
		t.Fatalf("after a restart: %v cvc %q, %v; the security code must be gone", got, got.CVC, err)
	}
}

const pk = "pk_test_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefg"

func publicTokenize(t *testing.T, v *vaulttest.Vault, key string, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, v.PublicURL+vault.PublicTokensPath, bytes.NewReader(raw))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("the public route does not allow cross-origin calls")
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestWebPageTokensWaitForTheirMerchantToClaimThem(t *testing.T) {
	v, clk := start(t)
	ctx := t.Context()
	body := map[string]any{"number": visa, "exp_month": 12, "exp_year": 2030, "cvc": "123"}
	if status, _ := publicTokenize(t, v, "", body); status != http.StatusUnauthorized {
		t.Fatalf("without a publishable key: %d", status)
	}
	if status, _ := publicTokenize(t, v, "sk_test_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefg", body); status != http.StatusUnauthorized {
		t.Fatalf("with a secret key: %d", status)
	}
	if status, out := publicTokenize(t, v, pk, map[string]any{"number": "4242424242424241", "exp_month": 12, "exp_year": 2030}); status != http.StatusBadRequest {
		t.Fatalf("with a bad number: %d %v", status, out)
	}
	status, out := publicTokenize(t, v, pk, body)
	token, _ := out["id"].(string)
	if status != http.StatusOK || !strings.HasPrefix(token, "tok_") || out["last4"] != "4242" || out["brand"] != "visa" {
		t.Fatalf("POST %s = %d %v", vault.PublicTokensPath, status, out)
	}
	if _, isThere := out["bin"]; isThere {
		t.Error("the web page is told the BIN")
	}

	c, err := v.Client.Card(ctx, token)
	if err != nil || c.Owner != "" || c.PublishableKey != pk {
		t.Fatalf("Card = %+v, %v; want unclaimed, with its publishable key", c, err)
	}
	if _, err := v.Client.Detokenize(ctx, token, owner); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("an unclaimed token was detokenized: %v", err)
	}
	claimed, err := v.Client.Claim(ctx, token, owner)
	if err != nil || claimed.Owner != owner || claimed.PublishableKey != "" {
		t.Fatalf("Claim = %+v, %v", claimed, err)
	}
	if _, err := v.Client.Claim(ctx, token, owner); err != nil {
		t.Fatalf("claiming again for the same owner: %v", err)
	}
	if _, err := v.Client.Claim(ctx, token, "mch_other/test"); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("another owner claimed a claimed token: %v", err)
	}
	if got, err := v.Client.Detokenize(ctx, token, owner); err != nil || got.Number != visa || got.CVC != "123" {
		t.Fatalf("Detokenize after the claim = %v %q, %v", got, got.CVC, err)
	}

	_, late := publicTokenize(t, v, pk, body)
	lateToken, _ := late["id"].(string)
	clk.Advance(61 * time.Minute)
	if _, err := v.Client.Claim(ctx, lateToken, owner); !errors.Is(err, vault.ErrNotFound) {
		t.Fatalf("a token was claimed after its window: %v", err)
	}
	if n, err := v.Service.PurgeUnclaimed(ctx); err != nil || n != 1 {
		t.Fatalf("PurgeUnclaimed = %d, %v; want the one late token", n, err)
	}
	if _, err := v.Client.Card(ctx, token); err != nil {
		t.Fatalf("the purge removed a claimed token: %v", err)
	}
}

func TestCORSPreflight(t *testing.T) {
	v, _ := start(t)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodOptions, v.PublicURL+vault.PublicTokensPath, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || !strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("preflight = %d %v", resp.StatusCode, resp.Header)
	}
}

// Exit criterion: the vault refuses any caller without the API's client certificate.
func TestTheVaultRefusesCallersWithoutTheAPICertificate(t *testing.T) {
	v, _ := start(t)
	c := tokenize(t, v, "req-1", card(visa, ""))
	stranger, err := mtls.NewPKI("not jupiter")
	if err != nil {
		t.Fatal(err)
	}
	noCert := vault.NewClient(v.URL, mtls.ClientConfig(emptyCert(), v.PKI.Pool()))
	refused := map[string]*vault.Client{
		"no certificate":                vault.NewClient(v.URL, nil),
		"no client certificate":         noCert,
		"another identity":              vaulttest.ClientFor(t, v.PKI, v.URL, "spiffe://jupiter/reports"),
		"the API's identity, wrong CA":  vaulttest.ClientFor(t, stranger, v.URL, vault.APIIdentity),
		"the API's identity, plain URL": vault.NewClient(strings.Replace(v.URL, "https://", "http://", 1), nil),
	}
	for name, client := range refused {
		t.Run(name, func(t *testing.T) {
			if got, err := client.Detokenize(t.Context(), c.Token, owner); !errors.Is(err, vault.ErrUnavailable) || got.Number != "" {
				t.Fatalf("Detokenize = %v, %v; want the connection refused", got, err)
			}
		})
	}
	// The public listener has no route to the card data.
	resp, err := http.Post(v.PublicURL+vault.CardsPath+"/"+c.Token+"/detokenize", "application/json", strings.NewReader(`{"owner":"`+owner+`"}`)) //nolint:noctx // a test
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("the public listener detokenized a card")
	}
}

// Exit criterion: rotating the key-encryption key completes while tokenization and
// detokenization continue under load, and the old key can then be retired.
func TestKeyRotationUnderLoad(t *testing.T) {
	v, _ := start(t)
	ctx := t.Context()
	const existing = 300
	tokens := make([]string, 0, existing)
	for i := range existing {
		tokens = append(tokens, tokenize(t, v, fmt.Sprintf("before-%d", i), card(visa, "")).Token)
	}

	var ops, failures atomic.Int64
	var firstFailure atomic.Value
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	made := []string{}
	for w := range 8 {
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				c, err := v.Client.Tokenize(ctx, vault.TokenizeRequest{Owner: owner, RequestKey: fmt.Sprintf("load-%d-%d", w, i), Card: card("5555555555554444", "")})
				if err == nil {
					mu.Lock()
					made = append(made, c.Token)
					mu.Unlock()
					var got vault.CardData
					got, err = v.Client.Detokenize(ctx, tokens[(w*37+i)%existing], owner)
					if err == nil && got.Number != visa {
						err = fmt.Errorf("detokenized %s", got)
					}
				}
				if err != nil {
					failures.Add(1)
					firstFailure.CompareAndSwap(nil, err.Error())
				}
				ops.Add(1)
			}
		})
	}

	time.Sleep(100 * time.Millisecond)
	if err := v.KMS.Add("k2", vaulttest.Key()); err != nil {
		t.Fatal(err)
	}
	if err := v.KMS.Activate("k2"); err != nil {
		t.Fatal(err)
	}
	before := ops.Load()
	moved := 0
	for {
		n, err := v.Service.Rewrap(ctx, 5)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		moved += n
	}
	during := ops.Load() - before
	close(stop)
	wg.Wait()
	// A tokenization that read the active key just before the switch may store its card
	// under the old key after the pass above; the runbook's final pass moves it.
	if _, err := v.Service.RewrapAll(ctx, 100); err != nil {
		t.Fatal(err)
	}

	if failures.Load() > 0 {
		t.Fatalf("%d of %d operations failed during the rotation; first: %v", failures.Load(), ops.Load(), firstFailure.Load())
	}
	if during < 50 || moved < existing {
		t.Fatalf("the rotation moved %d data keys while %d operations ran; want at least %d, under load", moved, during, existing)
	}
	usage, err := v.Service.KeyUsage(ctx)
	if err != nil || usage["k1"] != 0 || usage["k2"] != int64(existing+len(made)) {
		t.Fatalf("KeyUsage = %v, %v; want every card under k2", usage, err)
	}
	if err := v.Service.CheckRetirable(ctx, "k1"); err != nil {
		t.Fatal(err)
	}
	if err := v.Service.CheckRetirable(ctx, "k2"); !errors.Is(err, server.ErrKeyInUse) {
		t.Fatalf("the active key is retirable: %v", err)
	}
	if err := v.KMS.Remove("k1"); err != nil {
		t.Fatal(err)
	}
	for _, token := range append(tokens, made...) {
		if _, err := v.Client.Detokenize(ctx, token, owner); err != nil {
			t.Fatalf("%s cannot be read after the old key was retired: %v", token, err)
		}
	}
	t.Logf("rewrapped %d data keys while %d tokenize+detokenize pairs ran", moved, during)
}

func TestAKeyInUseIsNotRetirable(t *testing.T) {
	v, _ := start(t)
	tokenize(t, v, "req-1", card(visa, ""))
	if err := v.Service.CheckRetirable(t.Context(), "k1"); !errors.Is(err, server.ErrKeyInUse) {
		t.Fatalf("CheckRetirable = %v, want ErrKeyInUse", err)
	}
}

func emptyCert() tls.Certificate { return tls.Certificate{} }

func TestTheWorkerMayReadCardsButNotSaveThem(t *testing.T) {
	v, _ := start(t)
	c := tokenize(t, v, "req-1", card(visa, "123"))
	worker := vaulttest.ClientFor(t, v.PKI, v.URL, vault.WorkerIdentity)
	if got, err := worker.Detokenize(t.Context(), c.Token, owner); err != nil || got.Number != visa {
		t.Fatalf("the worker's Detokenize = %v, %v", got, err)
	}
	if _, err := worker.Card(t.Context(), c.Token); err != nil {
		t.Fatalf("the worker's Card: %v", err)
	}
	_, err := worker.Tokenize(t.Context(), vault.TokenizeRequest{Owner: owner, RequestKey: "w", Card: card(visa, "")})
	if err == nil || errors.Is(err, vault.ErrUnavailable) || !strings.Contains(err.Error(), "may not") {
		t.Fatalf("the worker tokenized: %v", err)
	}
}

func TestOnlyTheWorkerStoresNetworkTokens(t *testing.T) {
	v, _ := start(t)
	c := tokenize(t, v, "req-1", card(visa, ""))
	nt := vault.NetworkToken{Number: "4895370000000013", ExpMonth: 12, ExpYear: 2030, Reference: "DNITHE1"}
	if err := v.Client.StoreNetworkToken(t.Context(), c.Token, owner, nt); err == nil || !strings.Contains(err.Error(), "may not") {
		t.Fatalf("the API stored a network token: %v", err)
	}
	worker := vaulttest.ClientFor(t, v.PKI, v.URL, vault.WorkerIdentity)
	if err := worker.StoreNetworkToken(t.Context(), c.Token, owner, nt); err != nil {
		t.Fatalf("the worker's StoreNetworkToken: %v", err)
	}
}

func TestARepeatedRequestKeyAnswersFromTheFirstCard(t *testing.T) {
	v, clk := start(t)
	thisMonth := vault.CardData{Number: visa, ExpMonth: 9, ExpYear: 2026}
	first := tokenize(t, v, "req-1", thisMonth)
	clk.Advance(20 * time.Hour) // into October: the card has expired since
	if again := tokenize(t, v, "req-1", thisMonth); again.Token != first.Token {
		t.Fatalf("the retry got %s, want %s", again.Token, first.Token)
	}
	// However long after: a retry must always find its card, so a request key is never
	// released.
	clk.Advance(365 * 24 * time.Hour)
	if again := tokenize(t, v, "req-1", thisMonth); again.Token != first.Token {
		t.Fatalf("a year later the retry got %s, want %s", again.Token, first.Token)
	}
}

func TestThePublicRouteIsRateLimited(t *testing.T) {
	v, _ := start(t)
	body := map[string]any{"number": visa, "exp_month": 12, "exp_year": 2030}
	limited := 0
	for range 30 {
		if status, _ := publicTokenize(t, v, pk, body); status == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Fatal("thirty cards in a burst from one address were all accepted")
	}
}

// Behind a trusted proxy, each browser has a bucket of its own, named by the proxy's
// X-Forwarded-For; a client cannot name itself past the proxy.
func TestThePublicRouteLimitsEachClientBehindAProxy(t *testing.T) {
	trusted, err := ratelimit.ParseTrusted("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{now: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	v := vaulttest.Start(t, srv.Pool(t), vaulttest.Options{Now: c.Now, Clients: ratelimit.Clients{Trusted: trusted}})
	body := map[string]any{"number": visa, "exp_month": 12, "exp_year": 2030}
	from := func(forwarded string) int {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, v.PublicURL+vault.PublicTokensPath, bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+pk)
		req.Header.Set("X-Forwarded-For", forwarded)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	for range 20 {
		if status := from("203.0.113.1"); status != http.StatusOK {
			t.Fatalf("within one browser's burst: %d", status)
		}
	}
	if from("203.0.113.1") != http.StatusTooManyRequests {
		t.Fatal("a browser past its burst was let through")
	}
	if from("198.51.100.7, 203.0.113.1") != http.StatusTooManyRequests {
		t.Fatal("a browser named itself past the proxy")
	}
	if from("203.0.113.2") != http.StatusOK {
		t.Fatal("another browser behind the same proxy was refused")
	}
}
