package money

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

var (
	ErrInvalidRate  = errors.New("money: invalid rate")
	ErrRoundingMode = errors.New("money: invalid rounding mode")
)

type Rate struct {
	value *big.Rat // never mutated after construction
	scale int
}

func ParseRate(s string) (Rate, error) {
	unsigned, _ := strings.CutPrefix(s, "-")
	whole, fraction, hasPoint := strings.Cut(unsigned, ".")
	if !isDigits(whole) || (hasPoint && !isDigits(fraction)) {
		return Rate{}, fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	value, ok := new(big.Rat).SetString(s)
	if !ok {
		return Rate{}, fmt.Errorf("%w: %q", ErrInvalidRate, s)
	}
	return Rate{value: value, scale: len(fraction)}, nil
}

func (r Rate) String() string {
	if r.value == nil {
		return "<invalid rate>"
	}
	return r.value.FloatString(r.scale)
}

// The zero value is deliberately not a mode, so a caller cannot round by omission.
type RoundingMode int

const (
	_        RoundingMode = iota
	HalfEven              // nearest; ties to the even unit (banker's rounding)
	HalfUp                // nearest; ties away from zero
	HalfDown              // nearest; ties toward zero
	Down                  // toward zero (truncation)
	Up                    // away from zero
	Floor                 // toward negative infinity
	Ceiling               // toward positive infinity
)

func (m RoundingMode) valid() bool { return m >= HalfEven && m <= Ceiling }

func (m RoundingMode) String() string {
	switch m {
	case HalfEven:
		return "HalfEven"
	case HalfUp:
		return "HalfUp"
	case HalfDown:
		return "HalfDown"
	case Down:
		return "Down"
	case Up:
		return "Up"
	case Floor:
		return "Floor"
	case Ceiling:
		return "Ceiling"
	default:
		return fmt.Sprintf("RoundingMode(%d)", int(m))
	}
}

func (a Amount) MulRate(rate Rate, mode RoundingMode) (Amount, error) {
	if err := a.currency.validate(); err != nil {
		return Amount{}, err
	}
	if rate.value == nil {
		return Amount{}, fmt.Errorf("%w: zero value", ErrInvalidRate)
	}
	// The mode is checked even when the product is exact, so a missing mode is caught
	// on the first call rather than on the first inexact one.
	if !mode.valid() {
		return Amount{}, fmt.Errorf("%w: %v", ErrRoundingMode, mode)
	}
	exact := new(big.Rat).Mul(new(big.Rat).SetInt64(a.minor), rate.value)
	rounded, err := round(exact, mode)
	if err != nil {
		return Amount{}, err
	}
	if !rounded.IsInt64() {
		return Amount{}, fmt.Errorf("%w: %v × %v", ErrOverflow, a, rate)
	}
	return Amount{minor: rounded.Int64(), currency: a.currency}, nil
}

func round(x *big.Rat, mode RoundingMode) (*big.Int, error) {
	// Truncated division gives the quotient toward zero and a remainder with x's sign.
	quotient, remainder := new(big.Int).QuoRem(x.Num(), x.Denom(), new(big.Int))
	if remainder.Sign() == 0 {
		return quotient, nil
	}
	awayFromZero := big.NewInt(int64(x.Sign()))

	// Compare twice the remainder's magnitude with the denominator to find which side of
	// one half the discarded fraction lies on.
	half := new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(x.Denom())

	var bumpAway bool
	switch mode {
	case HalfEven:
		bumpAway = half > 0 || (half == 0 && quotient.Bit(0) == 1)
	case HalfUp:
		bumpAway = half >= 0
	case HalfDown:
		bumpAway = half > 0
	case Down:
		bumpAway = false
	case Up:
		bumpAway = true
	case Floor:
		bumpAway = x.Sign() < 0
	case Ceiling:
		bumpAway = x.Sign() > 0
	default:
		return nil, fmt.Errorf("%w: %v", ErrRoundingMode, mode)
	}
	if bumpAway {
		quotient.Add(quotient, awayFromZero)
	}
	return quotient, nil
}
