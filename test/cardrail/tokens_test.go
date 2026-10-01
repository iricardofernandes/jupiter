//go:build integration

package cardrail_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/acquirer"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

func (h *harness) admin(path string, body any) {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.files.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		h.t.Fatalf("POST %s = %d", path, resp.StatusCode)
	}
}

func (h *harness) tokenState(pm string) (status, reference, last4 string) {
	h.t.Helper()
	if err := h.pool.QueryRow(h.t.Context(), "SELECT network_token_status, network_token_reference, last4 FROM payments.payment_methods WHERE id = $1", pm).
		Scan(&status, &reference, &last4); err != nil {
		h.t.Fatal(err)
	}
	return status, reference, last4
}

// A live card is given a network token; payments carry the token and a cryptogram; when
// the bank replaces the card, the token follows it and the merchant sees the new card;
// a suspended token leaves payments to the card number.
func TestNetworkTokens(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4242424242424242")
	if n, err := h.connector.ProvisionNetworkTokens(t.Context(), h.payments, 10); err != nil || n != 1 {
		t.Fatalf("ProvisionNetworkTokens = %d, %v", n, err)
	}
	status, reference, _ := h.tokenState(pm)
	if status != "active" || reference == "" {
		t.Fatalf("after provisioning: %s %q", status, reference)
	}
	if n, _ := h.connector.ProvisionNetworkTokens(t.Context(), h.payments, 10); n != 0 {
		t.Fatalf("a card was tokenized twice")
	}
	if _, it := h.pay(map[string]any{"amount": 3000, "payment_method": pm}); str(it, "status") != "succeeded" {
		t.Fatalf("paying with the token: %v", it)
	}
	if c := h.network.Completions(); len(c) != 1 || !c[0].ViaToken || c[0].PAN != "4242424242424242" {
		t.Fatalf("the network saw %+v", c)
	}

	h.admin("/admin/cards/replace", map[string]any{"pan": "4242424242424242", "new_pan": "4000056655665556", "exp_month": 3, "exp_year": 2032})
	if _, _, last4 := h.tokenState(pm); last4 != "5556" {
		t.Fatalf("after the card was replaced the merchant sees ****%s", last4)
	}
	if _, it := h.pay(map[string]any{"amount": 4000, "payment_method": pm}); str(it, "status") != "succeeded" {
		t.Fatalf("paying after the replacement: %v", it)
	}
	var replaced bool
	for _, c := range h.network.Completions() {
		replaced = replaced || (c.Completed == 4000 && c.ViaToken && c.PAN == "4000056655665556")
	}
	if !replaced {
		t.Fatalf("the payment after the replacement did not reach the new card: %+v", h.network.Completions())
	}

	h.admin("/admin/tokens/"+reference+"/suspend", nil)
	if status, _, _ := h.tokenState(pm); status != "suspended" {
		t.Fatalf("after suspension: %s", status)
	}
	if _, it := h.pay(map[string]any{"amount": 5000, "payment_method": pm}); str(it, "status") != "succeeded" {
		t.Fatalf("paying with the number after the token was suspended: %v", it)
	}
	h.consistent()
}

func (h *harness) tokenEvent(e any, secret string) int {
	h.t.Helper()
	raw, _ := json.Marshal(e)
	req, _ := http.NewRequestWithContext(h.t.Context(), http.MethodPost, h.api.URL+acquirer.EventsPath, bytes.NewReader(raw))
	req.Header.Set(cardnet.EventSignatureHeader, cardnet.SignEvent(raw, secret, h.clock.Now()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Token events are signed, checked, and applied in the order they happened.
func TestTokenEvents(t *testing.T) {
	h := newHarness(t)
	pm := h.saveCard("4242424242424242")
	if _, err := h.connector.ProvisionNetworkTokens(t.Context(), h.payments, 10); err != nil {
		t.Fatal(err)
	}
	_, reference, _ := h.tokenState(pm)
	at := h.clock.Now().UTC()
	event := func(status, last4 string, when time.Time) cardnet.TokenEvent {
		return cardnet.TokenEvent{Type: "token.updated", Reference: reference, Status: status, Last4: last4, ExpMonth: 3, ExpYear: 2032, OccurredAt: when}
	}
	if code := h.tokenEvent(event("suspended", "1111", at), "not the secret"); code != http.StatusUnauthorized {
		t.Fatalf("an unsigned event: %d", code)
	}
	for _, bad := range []cardnet.TokenEvent{event("deleted", "1111", at), event("active", "11a1", at), event("active", "1111", time.Time{})} {
		if code := h.tokenEvent(bad, eventsSecret); code != http.StatusBadRequest {
			t.Fatalf("%+v: %d", bad, code)
		}
	}
	if code := h.tokenEvent(event("active", "2222", at.Add(time.Minute)), eventsSecret); code != http.StatusOK {
		t.Fatalf("an event: %d", code)
	}
	// An event from before, delivered late, changes nothing.
	if code := h.tokenEvent(event("suspended", "1111", at), eventsSecret); code != http.StatusOK {
		t.Fatalf("a late event: %d", code)
	}
	if status, _, last4 := h.tokenState(pm); status != "active" || last4 != "2222" {
		t.Fatalf("after a late event: %s ****%s", status, last4)
	}
}
