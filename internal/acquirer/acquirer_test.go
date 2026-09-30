package acquirer

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/pkg/cardnet"
)

func TestBackoff(t *testing.T) {
	for attempts, want := range map[int]time.Duration{1: 5 * time.Second, 2: 10 * time.Second, 4: 40 * time.Second, 7: 5 * time.Minute, 30: 5 * time.Minute} {
		if got := backoff(attempts); got != want {
			t.Errorf("backoff(%d) = %v, want %v", attempts, got, want)
		}
	}
}

func TestReferences(t *testing.T) {
	if got := rrnPrefix(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)); got != "6274" {
		t.Errorf("rrnPrefix = %s", got)
	}
	a, b := merchantCode("mch_a"), merchantCode("mch_b")
	if len(a) != 15 || a == b || a != merchantCode("mch_a") {
		t.Errorf("merchantCode = %s, %s", a, b)
	}
	if got := expiry(vault.CardData{ExpMonth: 3, ExpYear: 2030}); got != "3003" {
		t.Errorf("expiry = %s", got)
	}
}

func TestDeclineCodes(t *testing.T) {
	for rc, want := range map[string]string{
		cardnet.DoNotHonour: "do_not_honor", cardnet.InsufficientFunds: "insufficient_funds", cardnet.ExpiredCard: "expired_card",
		cardnet.NotPermittedToCardholder: "transaction_not_allowed", cardnet.IssuerUnavailable: "issuer_not_available",
		cardnet.InvalidTransaction: "invalid_transaction", cardnet.InvalidAmount: "invalid_amount",
		cardnet.InvalidCardNumber: "incorrect_number", "Z9": "generic_decline",
	} {
		if got := declineCode(rc); got != want {
			t.Errorf("declineCode(%s) = %s, want %s", rc, got, want)
		}
	}
}

func TestNewNeedsItsDependencies(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("a connector without a pool, address or card source")
	}
}

func TestConfigurationChecks(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://network.example/files": true, "http://127.0.0.1:8584": true, "http://localhost:8584": true,
		"http://network.example": false, "ftp://127.0.0.1": false, "/relative": false,
	} {
		if err := checkClearingURL(raw); (err == nil) != ok {
			t.Errorf("checkClearingURL(%q) = %v", raw, err)
		}
	}
	env := func(values map[string]string) func(string) string { return func(k string) string { return values[k] } }
	if c, err := FromEnv(t.Context(), env(nil), nil, nil, nil); c != nil || err != nil {
		t.Errorf("without an address: %v, %v", c, err)
	}
	if _, err := FromEnv(t.Context(), env(map[string]string{"JUPITER_CARDNET_ADDR": "network.example:8583"}), nil, nil, nil); err == nil {
		t.Error("plain TCP to another machine was accepted")
	}
	if _, err := New(Config{Pool: &pgxpool.Pool{}, Addr: "x:1", Cards: nopCards{}, AcquirerID: "12/3"}); err == nil {
		t.Error("an acquirer id with a slash was accepted")
	}
}

type nopCards struct{}

func (nopCards) Detokenize(context.Context, string, string) (vault.CardData, error) {
	return vault.CardData{}, nil
}
