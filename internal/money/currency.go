package money

import "fmt"

type Currency struct {
	code     string
	exponent uint8
}

// USD, EUR and JPY exist so that nothing silently assumes BRL or an exponent of two.
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

func CurrencyByCode(code string) (Currency, error) {
	c, ok := currenciesByCode[code]
	if !ok {
		return Currency{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, code)
	}
	return c, nil
}

func (c Currency) Code() string { return c.code }

func (c Currency) Exponent() int { return int(c.exponent) }

func (c Currency) String() string { return c.code }

func (c Currency) validate() error {
	if c.code == "" {
		return fmt.Errorf("%w: zero value", ErrUnknownCurrency)
	}
	return nil
}
