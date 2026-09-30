package money

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

var (
	ErrUnknownCurrency  = errors.New("money: unknown currency")
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrOverflow         = errors.New("money: overflow")
	ErrInvalidAmount    = errors.New("money: invalid amount")
)

type Amount struct {
	minor    int64
	currency Currency
}

func New(minor int64, c Currency) (Amount, error) {
	if err := c.validate(); err != nil {
		return Amount{}, err
	}
	return Amount{minor: minor, currency: c}, nil
}

func Zero(c Currency) (Amount, error) { return New(0, c) }

func (a Amount) Minor() int64 { return a.minor }

func (a Amount) Currency() Currency { return a.currency }

func (a Amount) IsZero() bool { return a.minor == 0 }

func (a Amount) IsPositive() bool { return a.minor > 0 }

func (a Amount) IsNegative() bool { return a.minor < 0 }

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

func (a Amount) Neg() (Amount, error) {
	if err := a.currency.validate(); err != nil {
		return Amount{}, err
	}
	if a.minor == math.MinInt64 {
		return Amount{}, fmt.Errorf("%w: -(%v)", ErrOverflow, a)
	}
	return Amount{minor: -a.minor, currency: a.currency}, nil
}

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

func (a Amount) String() string {
	return a.currency.code + " " + a.Decimal()
}

// Parse rejects more decimal places than the currency has instead of rounding them.
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
