// Package money represents amounts of money as integer minor units with an explicit
// currency.
//
// Amounts never pass through floating point. Arithmetic between different currencies is
// an error rather than a coercion, overflow is an error rather than a wrap, and every
// conversion that can lose precision names its rounding mode. Splitting an amount uses
// largest-remainder allocation, so a minor unit is never lost or invented.
package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	// ErrUnknownCurrency is returned for a currency Jupiter does not know, including the
	// zero value of Currency.
	ErrUnknownCurrency = errors.New("money: unknown currency")
	// ErrCurrencyMismatch is returned when an operation combines two currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	// ErrOverflow is returned when a result does not fit in 64-bit minor units.
	ErrOverflow = errors.New("money: overflow")
	// ErrInvalidAmount is returned when a decimal string is not a valid amount.
	ErrInvalidAmount = errors.New("money: invalid amount")
)

// Amount is a quantity of money in a currency's minor unit (centavos for BRL). It is an
// immutable value: every operation returns a new Amount. The zero value is invalid,
// because it has no currency.
type Amount struct {
	minor    int64
	currency Currency
}

// New returns an amount of minor units in currency c.
func New(minor int64, c Currency) (Amount, error) {
	if err := c.validate(); err != nil {
		return Amount{}, err
	}
	return Amount{minor: minor, currency: c}, nil
}

// Zero returns a zero amount in currency c.
func Zero(c Currency) (Amount, error) { return New(0, c) }

// Minor returns the amount in minor units.
func (a Amount) Minor() int64 { return a.minor }

// Currency returns the amount's currency.
func (a Amount) Currency() Currency { return a.currency }

// IsZero reports whether the amount is zero.
func (a Amount) IsZero() bool { return a.minor == 0 }

// IsPositive reports whether the amount is greater than zero.
func (a Amount) IsPositive() bool { return a.minor > 0 }

// IsNegative reports whether the amount is less than zero.
func (a Amount) IsNegative() bool { return a.minor < 0 }

// Add returns a + b.
func (a Amount) Add(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	sum := a.minor + b.minor
	// Overflow happened if both operands share a sign that the result does not.
	if (a.minor >= 0) == (b.minor >= 0) && (sum >= 0) != (a.minor >= 0) {
		return Amount{}, fmt.Errorf("%w: %v + %v", ErrOverflow, a, b)
	}
	return Amount{minor: sum, currency: a.currency}, nil
}

// Sub returns a - b.
func (a Amount) Sub(b Amount) (Amount, error) {
	if err := a.sameCurrency(b); err != nil {
		return Amount{}, err
	}
	diff := a.minor - b.minor
	// Overflow happened if the operands differ in sign and the result has b's sign.
	if (a.minor >= 0) != (b.minor >= 0) && (diff >= 0) != (a.minor >= 0) {
		return Amount{}, fmt.Errorf("%w: %v - %v", ErrOverflow, a, b)
	}
	return Amount{minor: diff, currency: a.currency}, nil
}

// Neg returns -a.
func (a Amount) Neg() (Amount, error) {
	if err := a.currency.validate(); err != nil {
		return Amount{}, err
	}
	if a.minor == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: -(%v)", ErrOverflow, a)
	}
	return Amount{minor: -a.minor, currency: a.currency}, nil
}

// Cmp returns -1, 0 or +1 as a is less than, equal to or greater than b.
func (a Amount) Cmp(b Amount) (int, error) {
	if err := a.sameCurrency(b); err != nil {
		return 0, err
	}
	switch {
	case a.minor < b.minor:
		return -1, nil
	case a.minor > b.minor:
		return 1, nil
	default:
		return 0, nil
	}
}

func (a Amount) sameCurrency(b Amount) error {
	if err := a.currency.validate(); err != nil {
		return err
	}
	if err := b.currency.validate(); err != nil {
		return err
	}
	if a.currency != b.currency {
		return fmt.Errorf("%w: %v and %v", ErrCurrencyMismatch, a.currency, b.currency)
	}
	return nil
}

// Decimal formats the amount in major units with exactly the currency's number of
// decimal places, such as "600.00" or "-0.05". It is the form wire protocols use.
func (a Amount) Decimal() string {
	// Formatting the unsigned magnitude handles math.MinInt64, whose negation overflows.
	magnitude := uint64(a.minor) //nolint:gosec // two's complement reinterpretation is the point
	if a.minor < 0 {
		magnitude = -magnitude
	}
	digits := strconv.FormatUint(magnitude, 10)
	exponent := a.currency.Exponent()

	var b strings.Builder
	if a.minor < 0 {
		b.WriteByte('-')
	}
	if exponent == 0 {
		b.WriteString(digits)
		return b.String()
	}
	if len(digits) <= exponent {
		digits = strings.Repeat("0", exponent-len(digits)+1) + digits
	}
	b.WriteString(digits[:len(digits)-exponent])
	b.WriteByte('.')
	b.WriteString(digits[len(digits)-exponent:])
	return b.String()
}

// String formats the amount for people and logs, such as "BRL 600.00".
func (a Amount) String() string {
	return a.currency.code + " " + a.Decimal()
}

// Parse reads a decimal string in major units, such as "600.00", "600.5" or "-0.01".
// A string with more decimal places than the currency's minor unit is rejected rather
// than rounded. Signs other than a leading "-", exponents and separators are rejected.
func Parse(s string, c Currency) (Amount, error) {
	if err := c.validate(); err != nil {
		return Amount{}, err
	}
	digits, negative, err := parseDigits(s, c.Exponent())
	if err != nil {
		return Amount{}, err
	}
	magnitude, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
	switch {
	case !negative && magnitude <= math.MaxInt64:
		return Amount{minor: int64(magnitude), currency: c}, nil
	case negative && magnitude <= math.MaxInt64+1:
		return Amount{minor: int64(-magnitude), currency: c}, nil //nolint:gosec // bounded by the case condition
	default:
		return Amount{}, fmt.Errorf("%w: %q", ErrOverflow, s)
	}
}

// parseDigits validates s and returns its digits scaled to exponent decimal places.
func parseDigits(s string, exponent int) (digits string, negative bool, err error) {
	invalid := fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	unsigned, negative := strings.CutPrefix(s, "-")
	whole, fraction, hasPoint := strings.Cut(unsigned, ".")
	if !isDigits(whole) || (hasPoint && !isDigits(fraction)) {
		return "", false, invalid
	}
	if len(fraction) > exponent {
		return "", false, invalid
	}
	return whole + fraction + strings.Repeat("0", exponent-len(fraction)), negative, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
