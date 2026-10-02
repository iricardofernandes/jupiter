// Package vault is how the rest of Jupiter reaches the card vault: the client, and the
// types that cross the wire. The vault itself (storage, encryption, keys) lives in its
// subpackages, which only the vault binary imports, so card numbers are stored and
// decrypted in one process with its own database.
package vault

import (
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/iricardofernandes/jupiter/internal/id"
)

// The URI SANs of the client certificates the vault's internal listener admits. The API
// may do everything; the worker, whose rail calls and request recovery need only to read
// and claim cards, may not tokenize.
const (
	APIIdentity    = "spiffe://jupiter/api"
	WorkerIdentity = "spiffe://jupiter/worker"
)

// TokenPrefix marks card tokens. A token's suffix is 122 random bits, not a UUIDv7:
// it must reveal nothing, not even when it was made.
var TokenPrefix = id.MustPrefix("tok")

var (
	ErrInvalidCard = errors.New("vault: invalid card")
	ErrNotFound    = errors.New("vault: no such card")
	// ErrConflict answers a tokenization that repeats a request key with another card.
	ErrConflict    = errors.New("vault: request key already used with another card")
	ErrUnavailable = errors.New("vault: unavailable")
)

// CardError says which field of a card is wrong. It wraps ErrInvalidCard.
type CardError struct {
	Code  string
	Param string
}

func (e *CardError) Error() string {
	return fmt.Sprintf("vault: invalid card: %s (%s)", e.Code, e.Param)
}

func (e *CardError) Unwrap() error { return ErrInvalidCard }

// Card is what the vault says about a token without revealing the number. BIN is the
// first eight digits of a number of sixteen or more, and the first six of a shorter one.
type Card struct {
	Token string `json:"token"`
	// Owner is who may use the token; it is empty for a token made from a web page and
	// not yet claimed, which then carries the publishable key it was made with.
	Owner          string `json:"owner,omitempty"`
	PublishableKey string `json:"publishable_key,omitempty"`
	Brand          string `json:"brand"`
	BIN            string `json:"bin"`
	Last4          string `json:"last4"`
	ExpMonth       int    `json:"exp_month"`
	ExpYear        int    `json:"exp_year"`
	Fingerprint    string `json:"fingerprint"`
	// ClientIP is the address a web page sent the card from, as the vault saw it; empty
	// for a card the merchant's server sent.
	ClientIP  string    `json:"client_ip,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// CardData is a card in the clear. It exists only in memory, on its way into the vault
// or out of it to a rail, and prints redacted, so a log line or an error that formats
// it never carries the number or the security code.
type CardData struct {
	Number   string
	ExpMonth int
	ExpYear  int
	CVC      string
	// NetworkToken is the card network's token for the card, when it issued one.
	NetworkToken *NetworkToken
}

// NetworkToken is a token a card network issued for a card: a number of its own that
// it maps to the card, and the reference it names it by.
type NetworkToken struct {
	Number    string `json:"number"`
	ExpMonth  int    `json:"exp_month"`
	ExpYear   int    `json:"exp_year"`
	Reference string `json:"reference"`
}

func (c CardData) String() string {
	return fmt.Sprintf("card ****%s %02d/%d", last4(c.Number), c.ExpMonth, c.ExpYear)
}

func (c CardData) GoString() string { return c.String() }

func (c CardData) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

func (c CardData) LogValue() slog.Value { return slog.StringValue(c.String()) }

func (c CardData) MarshalJSON() ([]byte, error) {
	return nil, errors.New("vault: CardData is never marshaled; use the wire types")
}

func last4(number string) string {
	if len(number) < 4 {
		return ""
	}
	return number[len(number)-4:]
}

// TokenizeRequest stores a card for Owner. RequestKey makes it idempotent: repeating it
// with the same card returns the same token, and with another card fails with
// ErrConflict.
type TokenizeRequest struct {
	Owner      string
	RequestKey string
	Card       CardData
}
