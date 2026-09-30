//go:build integration

package api_test

import (
	"encoding/hex"
	"net/http"
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/api"
	"github.com/iricardofernandes/jupiter/internal/api/openapi"
	"github.com/iricardofernandes/jupiter/internal/vault"
)

func cardBody(number string) map[string]any {
	return map[string]any{"type": "card", "card": map[string]any{"number": number, "exp_month": 12, "exp_year": 2030, "cvc": "123"}}
}

func (h *harness) createMethod(body any, headers map[string]string) openapi.PaymentMethod {
	h.t.Helper()
	resp := h.expect(call{method: "POST", path: "/v1/payment_methods", body: body, headers: headers}, http.StatusOK)
	conforms(h.t, "PaymentMethod", resp.body)
	var pm openapi.PaymentMethod
	resp.decode(h.t, &pm)
	return pm
}

// apiRowsContaining counts the rows of the API's database whose text form holds needle,
// as text or hex bytes.
func (h *harness) apiRowsContaining(needle string) int {
	h.t.Helper()
	rows, err := h.pool.Query(h.t.Context(), `SELECT quote_ident(table_schema) || '.' || quote_ident(table_name)
		FROM information_schema.tables WHERE table_type = 'BASE TABLE' AND table_schema NOT IN ('pg_catalog', 'information_schema')`)
	if err != nil {
		h.t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			h.t.Fatal(err)
		}
		tables = append(tables, name)
	}
	found := 0
	for _, table := range tables {
		var n int
		if err := h.pool.QueryRow(h.t.Context(), "SELECT count(*) FROM "+table+" AS r WHERE r::text LIKE '%' || $1 || '%' OR r::text LIKE '%' || $2 || '%'",
			needle, hex.EncodeToString([]byte(needle))).Scan(&n); err != nil {
			h.t.Fatal(err)
		}
		found += n
	}
	return found
}

func TestSavingACardByNumber(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	pm := h.createMethod(cardBody("4242 4242 4242 4242"), map[string]string{"Idempotency-Key": "card-1"})
	if !strings.HasPrefix(pm.Id, "pm_") || pm.Type != "card" || pm.Card.Bin != "42424242" || pm.Card.Last4 != "4242" ||
		pm.Card.ExpMonth != 12 || len(pm.Card.Fingerprint) != 16 || pm.Livemode {
		t.Fatalf("payment method = %+v", pm)
	}
	var got openapi.PaymentMethod
	h.expect(call{method: "GET", path: "/v1/payment_methods/" + pm.Id}, http.StatusOK).decode(t, &got)
	if got != pm {
		t.Fatalf("GET = %+v, want %+v", got, pm)
	}

	again := h.createMethod(cardBody("4242424242424242"), map[string]string{"Idempotency-Key": "card-1"})
	if again.Id != pm.Id {
		t.Fatalf("a retry with the same Idempotency-Key saved a second card: %s and %s", pm.Id, again.Id)
	}
	h.expect(call{
		method: "POST", path: "/v1/payment_methods", body: cardBody("5555555555554444"),
		headers: map[string]string{"Idempotency-Key": "card-1"},
	}, http.StatusUnprocessableEntity)

	other := h.createMethod(cardBody("4242424242424242"), nil)
	if other.Id == pm.Id || other.Card.Fingerprint != pm.Card.Fingerprint {
		t.Fatalf("the same card saved again: %+v; want a new payment method with the same fingerprint", other)
	}
	h.newMerchant(api.CurrentVersion)
	elsewhere := h.createMethod(cardBody("4242424242424242"), nil)
	if elsewhere.Card.Fingerprint == pm.Card.Fingerprint {
		t.Fatal("two merchants see the same fingerprint for a card: they could match their customers")
	}
	h.expect(call{method: "GET", path: "/v1/payment_methods/" + pm.Id}, http.StatusNotFound)

	if n := h.apiRowsContaining("4242424242424242"); n > 0 {
		t.Fatalf("the card number is in %d rows of the API's database", n)
	}
	if h.apiRowsContaining("42424242") == 0 {
		t.Fatal("the scan does not find even the BIN, which the API stores")
	}
}

func TestInvalidPaymentMethodRequests(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	resp := h.expect(call{method: "POST", path: "/v1/payment_methods", body: cardBody(badNumber)}, http.StatusPaymentRequired)
	var e openapi.ErrorResponse
	resp.decode(t, &e)
	if e.Error.Type != openapi.CardError || e.Error.Code != "incorrect_number" || e.Error.Param == nil || *e.Error.Param != "card[number]" {
		t.Fatalf("error = %+v", e.Error)
	}
	if strings.Contains(string(resp.body), badNumber) {
		t.Fatal("the error repeats the card number")
	}
	for name, body := range map[string]any{
		"both":            map[string]any{"type": "card", "card": map[string]any{"token": "tok_x", "number": "4242424242424242"}},
		"no expiry":       map[string]any{"type": "card", "card": map[string]any{"number": "4242424242424242"}},
		"another type":    map[string]any{"type": "pix", "card": map[string]any{"token": "tok_x"}},
		"unknown":         map[string]any{"type": "card", "card": map[string]any{"token": "tok_x"}, "customer": "c"},
		"no token":        map[string]any{"type": "card", "card": map[string]any{"token": "tok_00000000000000000000000000"}},
		"empty token":     map[string]any{"type": "card", "card": map[string]any{"token": ""}},
		"number as token": map[string]any{"type": "card", "card": map[string]any{"token": "4242424242424242"}},
	} {
		resp := h.do(call{method: "POST", path: "/v1/payment_methods", body: body})
		if resp.status != http.StatusBadRequest || strings.Contains(string(resp.body), "4242424242424242") {
			t.Errorf("%s: %d %s, want 400 without the number", name, resp.status, resp.body)
		}
	}
	h.expect(call{method: "GET", path: "/v1/payment_methods/pm_nope"}, http.StatusNotFound)
	h.expect(call{method: "POST", path: "/v1/payment_intents", body: map[string]any{
		"amount": 1000, "currency": "brl", "payment_method": "pm_01jv0000000000000000000000",
	}}, http.StatusBadRequest)
}

func TestPayingWithSavedCards(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	tests := []struct {
		number      string
		status      openapi.PaymentIntentStatus
		declineCode string
	}{
		{"4242424242424242", "succeeded", ""},
		{"5555555555554444", "succeeded", ""},
		{"4000000000000002", "requires_payment_method", "generic_decline"},
		{"4000000000009995", "requires_payment_method", "insufficient_funds"},
		{"4000002500003155", "requires_action", ""},
		{"4111111111111111", "requires_payment_method", "test_mode_live_card"},
	}
	for _, tt := range tests {
		pm := h.createMethod(cardBody(tt.number), nil)
		status := http.StatusOK
		if tt.declineCode != "" {
			status = http.StatusPaymentRequired
		}
		it := h.createIntent(map[string]any{"amount": 5000, "payment_method": pm.Id, "confirm": true}, status)
		wantStatus(t, it, tt.status)
		if tt.declineCode != "" && (it.LastPaymentError == nil || it.LastPaymentError.DeclineCode == nil || *it.LastPaymentError.DeclineCode != tt.declineCode) {
			t.Errorf("%s: last_payment_error = %+v, want %s", tt.number, it.LastPaymentError, tt.declineCode)
		}
	}
	h.consistent()
}

func TestTheSecurityCodeReachesOnlyTheFirstAuthorization(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	pm := h.createMethod(cardBody("4242424242424242"), nil)
	var token string
	for tok, c := range h.vault.cards {
		if c.data.Number == "4242424242424242" {
			token = tok
		}
	}
	h.createIntent(map[string]any{"amount": 1000, "payment_method": pm.Id, "confirm": true}, http.StatusOK)
	if cvc := h.vault.cards[token].data.CVC; cvc != "" {
		t.Fatalf("the security code is still held after the authorization: %q", cvc)
	}
	it := h.createIntent(map[string]any{"amount": 2000, "payment_method": pm.Id, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "succeeded")
}

func TestTokensFromWebPages(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	card := vault.CardData{Number: "4242424242424242", ExpMonth: 12, ExpYear: 2030, CVC: "123"}
	token := h.vault.public(h.keys["pk_test_"], card)
	pm := h.createMethod(map[string]any{"type": "card", "card": map[string]any{"token": token}}, nil)
	if pm.Card.Last4 != "4242" {
		t.Fatalf("payment method = %+v", pm)
	}
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": pm.Id, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "succeeded")

	liveToken := h.vault.public(h.keys["pk_live_"], card)
	wrongKind := h.vault.public(h.keys["sk_test_"], card)
	forged := h.vault.public("pk_test_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefg", card)
	h.newMerchant(api.CurrentVersion)
	othersToken := h.vault.public(h.keys["pk_test_"], card)
	for name, tok := range map[string]string{
		"a live-mode key's token":        liveToken,
		"a token made with a secret key": wrongKind,
		"a forged key's token":           forged,
		"another merchant's token":       token,
	} {
		resp := h.do(call{method: "POST", path: "/v1/payment_methods", body: map[string]any{"type": "card", "card": map[string]any{"token": tok}}})
		if resp.status != http.StatusBadRequest || !strings.Contains(string(resp.body), "resource_missing") {
			t.Errorf("%s: %d %s, want resource_missing", name, resp.status, resp.body)
		}
	}
	h.createMethod(map[string]any{"type": "card", "card": map[string]any{"token": othersToken}}, nil)
}

func TestAVaultOutageIsRetryable(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	h.vault.down.Store(true)
	resp := h.expect(call{
		method: "POST", path: "/v1/payment_methods", body: cardBody("4242424242424242"),
		headers: map[string]string{"Idempotency-Key": "outage"},
	}, http.StatusServiceUnavailable)
	if !strings.Contains(string(resp.body), "vault_unavailable") {
		t.Fatalf("body = %s", resp.body)
	}
	h.vault.down.Store(false)
	pm := h.createMethod(cardBody("4242424242424242"), map[string]string{"Idempotency-Key": "outage"})

	// Down when the authorization needs the card: the outcome is unknown, and the
	// resolver finishes the payment once the vault is back.
	h.vault.down.Store(true)
	it := h.createIntent(map[string]any{"amount": 1000, "payment_method": pm.Id, "confirm": true}, http.StatusOK)
	wantStatus(t, it, "processing")
	h.vault.down.Store(false)
	h.resolve()
	h.resolve()
	wantStatus(t, h.getIntent(it.Id), "succeeded")
	h.consistent()
}

func TestPaymentMethodScopes(t *testing.T) {
	h := newHarness(t, api.CurrentVersion)
	var key openapi.ApiKey
	h.expect(call{method: "POST", path: "/v1/api_keys", body: map[string]any{"scopes": []string{"payment_intents:write"}}}, http.StatusOK).decode(t, &key)
	h.expect(call{method: "POST", path: "/v1/payment_methods", key: *key.Value, body: cardBody("4242424242424242")}, http.StatusForbidden)
	h.expect(call{method: "POST", path: "/v1/payment_methods", key: h.keys["pk_test_"], body: cardBody("4242424242424242")}, http.StatusForbidden)
}
