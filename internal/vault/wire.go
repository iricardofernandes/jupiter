package vault

import (
	"fmt"
	"log/slog"
	"net/http"
)

// The vault's HTTP interface. Internal routes are served only over mTLS to APIIdentity;
// PublicTokensPath is the one route a web page calls, with a publishable key.
const (
	CardsPath        = "/v1/cards"
	PublicTokensPath = "/v1/tokens"
)

// WireCard carries a card number over the wire and is never logged.
type WireCard struct {
	Number   string `json:"number"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
	CVC      string `json:"cvc,omitempty"`
}

func (w WireCard) String() string { return w.Data().String() }

func (w WireCard) GoString() string { return w.String() }

func (w WireCard) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(w.String())) }

func (w WireCard) LogValue() slog.Value { return slog.StringValue(w.String()) }

func (w WireCard) Data() CardData {
	return CardData(w)
}

func WireCardOf(c CardData) WireCard {
	return WireCard(c)
}

type TokenizeBody struct {
	Owner      string   `json:"owner"`
	RequestKey string   `json:"request_key"`
	Card       WireCard `json:"card"`
}

type OwnerBody struct {
	Owner string `json:"owner"`
}

// PublicToken is the answer to a web page: enough to show the customer which card they
// entered, and the token its server passes on to Jupiter.
type PublicToken struct {
	ID       string `json:"id"`
	Object   string `json:"object"`
	Brand    string `json:"brand"`
	Last4    string `json:"last4"`
	ExpMonth int    `json:"exp_month"`
	ExpYear  int    `json:"exp_year"`
}

type ErrorBody struct {
	Error WireError `json:"error"`
}

type WireError struct {
	Code    string `json:"code"`
	Param   string `json:"param,omitempty"`
	Message string `json:"message"`
}

// Error codes beside the CardError codes, which travel as they are.
const (
	CodeNotFound       = "resource_missing"
	CodeConflict       = "request_key_conflict"
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized"
	CodeForbidden      = "forbidden"
	CodeRateLimited    = "rate_limited"
	CodeInternal       = "internal_error"
)

// StatusOf is the HTTP status the vault answers an error code with.
func StatusOf(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeInternal:
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}
