// Package pixapi holds the types of the Banco Central's API Pix, the interface every
// receiving PSP offers its users, generated from the official OpenAPI specification in
// api/bacen-pix (version 2.10.0, commit 55b60d7 of github.com/bacen/pix-api; see its
// README) by oapi-codegen, and a few helpers to use them. Jupiter's Pix connector is a
// client of this API; the Pix simulator serves it.
package pixapi

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is the API Pix specification the types were generated from.
const Version = "2.10.0"

// New allocates the value a pointer field points to and returns it. Most of the
// generated types nest anonymous structs, which cannot be named to be allocated:
//
//	cal := pixapi.New(&cob.Calendario)
//	cal.Expiracao = 3600
func New[T any](field **T) *T {
	*field = new(T)
	return *field
}

// Append adds a zero element to a pointer-to-slice field and returns it.
func Append[T any](field **[]T) *T {
	if *field == nil {
		*field = &[]T{}
	}
	**field = append(**field, *new(T))
	return &(**field)[len(**field)-1]
}

// Ptr returns a pointer to v, for the optional fields.
func Ptr[T any](v T) *T { return &v }

var (
	ErrValor = errors.New("pixapi: not an amount")

	valorPattern = regexp.MustCompile(`^\d{1,10}\.\d{2}$`)
	txidPattern  = regexp.MustCompile(`^[a-zA-Z0-9]{26,35}$`)
	e2eIDPattern = regexp.MustCompile(`^E[0-9A-Z]{8}\d{12}[a-zA-Z0-9]{11}$`)
	idPattern    = regexp.MustCompile(`^[a-zA-Z0-9]{1,35}$`)
)

// FormatValor writes centavos as the API's amounts: reais with a dot and two decimals.
func FormatValor(minor int64) string {
	return fmt.Sprintf("%d.%02d", minor/100, minor%100)
}

// ParseValor reads an amount of the API into centavos.
func ParseValor(s string) (int64, error) {
	if !valorPattern.MatchString(s) {
		return 0, fmt.Errorf("%w: %q", ErrValor, s)
	}
	reais, cents, _ := strings.Cut(s, ".")
	r, err := strconv.ParseInt(reais, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrValor, s)
	}
	c, _ := strconv.ParseInt(cents, 10, 64)
	return r*100 + c, nil
}

// ValidTxID reports whether s is a txid a charge may have: 26 to 35 letters and digits.
func ValidTxID(s string) bool { return txidPattern.MatchString(s) }

// ValidEndToEndID reports whether s has the shape of an endToEndId: E, the ISPB of the
// participant that made it, the minute in UTC (yyyyMMddHHmm) and 11 letters or digits.
func ValidEndToEndID(s string) bool { return e2eIDPattern.MatchString(s) }

// ValidID reports whether s may name a return (devolução) or a payload location.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// ErrorType is the type URI of an API Pix problem (RFC 7807).
func ErrorType(name string) string { return "https://pix.bcb.gov.br/api/v2/error/" + name }

// OAuth scopes.
const (
	ScopeCobWrite     = "cob.write"
	ScopeCobRead      = "cob.read"
	ScopeCobVWrite    = "cobv.write"
	ScopeCobVRead     = "cobv.read"
	ScopePixWrite     = "pix.write"
	ScopePixRead      = "pix.read"
	ScopeWebhookWrite = "webhook.write"
	ScopeWebhookRead  = "webhook.read"
)
