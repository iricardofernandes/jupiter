package vault_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/vault"
)

func TestCardDataNeverPrintsTheNumber(t *testing.T) {
	c := vault.CardData{Number: "4242424242424242", ExpMonth: 3, ExpYear: 2030, CVC: "737"}
	w := vault.WireCardOf(c)
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("paying", "card", c, "wire", w)
	printed := []string{
		fmt.Sprintf("%v %+v %#v %s", w, w, w, w), fmt.Sprint(vault.TokenizeBody{Card: w}),
		fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s %q", c, c, c, c, c), fmt.Sprint(&c),
		fmt.Errorf("authorizing %v: %w", c, errors.New("boom")).Error(), logged.String(),
	}
	for _, out := range printed {
		if strings.Contains(out, "4242424242424242") || strings.Contains(out, "737") {
			t.Errorf("printed %q", out)
		}
	}
	if !strings.Contains(printed[2], "****4242") {
		t.Errorf("String() = %q, want the last four", printed[0])
	}
	if _, err := json.Marshal(c); err == nil {
		t.Error("CardData marshaled to JSON")
	}
}

func TestClientMapsTheVaultsAnswers(t *testing.T) {
	answers := map[string]struct {
		status int
		body   string
	}{
		"/v1/cards/tok_card":     {http.StatusBadRequest, `{"error":{"code":"expired_card","param":"exp_year","message":"x"}}`},
		"/v1/cards/tok_missing":  {http.StatusNotFound, `{"error":{"code":"resource_missing","message":"x"}}`},
		"/v1/cards/tok_conflict": {http.StatusConflict, `{"error":{"code":"request_key_conflict","message":"x"}}`},
		"/v1/cards/tok_broken":   {http.StatusInternalServerError, `{"error":{"code":"internal_error","message":"x"}}`},
		"/v1/cards/tok_html":     {http.StatusBadGateway, `<html>bad gateway</html>`},
		"/v1/cards/tok_refused":  {http.StatusBadRequest, `{"error":{"code":"invalid_request","message":"owner is required"}}`},
		"/v1/cards/tok_garbled":  {http.StatusOK, `{not json`},
		"/v1/cards/tok_ok":       {http.StatusOK, `{"token":"tok_ok","last4":"4242"}`},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := answers[r.URL.Path]
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	}))
	defer srv.Close()
	c := vault.NewClient(srv.URL, nil)
	ctx := t.Context()

	_, err := c.Card(ctx, "tok_card")
	var ce *vault.CardError
	if !errors.As(err, &ce) || ce.Code != "expired_card" || ce.Param != "exp_year" || !errors.Is(err, vault.ErrInvalidCard) {
		t.Errorf("a card error: %v", err)
	}
	for token, want := range map[string]error{
		"tok_missing": vault.ErrNotFound, "tok_conflict": vault.ErrConflict,
		"tok_broken": vault.ErrUnavailable, "tok_html": vault.ErrUnavailable, "tok_garbled": vault.ErrUnavailable,
	} {
		if _, err := c.Card(ctx, token); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", token, err, want)
		}
	}
	if _, err := c.Card(ctx, "tok_refused"); err == nil || errors.Is(err, vault.ErrUnavailable) || !strings.Contains(err.Error(), "owner is required") {
		t.Errorf("a refused request: %v", err)
	}
	if card, err := c.Card(ctx, "tok_ok"); err != nil || card.Last4 != "4242" {
		t.Errorf("Card = %+v, %v", card, err)
	}
	srv.Close()
	if _, err := c.Detokenize(ctx, "tok_ok", "o"); !errors.Is(err, vault.ErrUnavailable) {
		t.Errorf("a vault that is down: %v", err)
	}
}

func TestClientFromEnv(t *testing.T) {
	if _, err := vault.ClientFromEnv(func(string) string { return "" }); err == nil {
		t.Error("a client without a URL")
	}
	env := map[string]string{"JUPITER_VAULT_URL": "https://vault:8082", "JUPITER_VAULT_CA": "/nonexistent"}
	if _, err := vault.ClientFromEnv(func(k string) string { return env[k] }); err == nil {
		t.Error("a client without certificates")
	}
	env["JUPITER_VAULT_URL"] = "http://vault:8082"
	if _, err := vault.ClientFromEnv(func(k string) string { return env[k] }); err == nil || !strings.Contains(err.Error(), "https") {
		t.Errorf("a client over plain HTTP: %v", err)
	}
}
