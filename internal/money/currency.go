package money

import "fmt"

// Currency is an ISO 4217 currency with the number of decimal places its minor unit
// represents. The zero value is not a valid currency; every operation rejects it.
type Currency struct {
	code     string
	exponent uint8
}

// Currencies Jupiter knows. Accounts hold one currency each and the plan excludes FX, so
// the list stays short; USD, EUR and JPY exist so that nothing silently assumes BRL or an
// exponent of two.
var (
	BRL = Currency{code: "BRL", exponent: 2}
	USD = Currency{code: "USD", exponent: 2}
	EUR = Currency{code: "EUR", exponent: 2}
	JPY = Currency{code: "JPY", exponent: 0}
)

var currenciesByCode = map[string]Currency{
	BRL.code: BRL,
	USD.code: USD,
	EUR.code: EUR,
	JPY.code: JPY,
}

// CurrencyByCode returns the currency with the given upper-case ISO 4217 code.
func CurrencyByCode(code string) (Currency, error) {
	c, ok := currenciesByCode[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

// Code returns the ISO 4217 alphabetic code.
func (c Currency) Code() string { return c.code }

// Exponent returns the number of decimal places of the minor unit: 2 for BRL, 0 for JPY.
func (c Currency) Exponent() int { return int(c.exponent) }

// String returns the ISO 4217 code.
func (c Currency) String() string { return c.code }

func (c Currency) validate() error {
	if c.code == "" {
		return fmt.Errorf("%w: zero value", ErrUnknownCurrency)
	}
	return nil
}
