package money_test

import (
	"errors"
	"math/big"
	"testing"

	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/internal/money"
)

var currencies = []money.Currency{money.BRL, money.USD, money.EUR, money.JPY}

func genCurrency() *rapid.Generator[money.Currency] {
	return rapid.SampledFrom(currencies)
}

func genAmount(c money.Currency) *rapid.Generator[money.Amount] {
	return rapid.Custom(func(t *rapid.T) money.Amount {
		a, err := money.New(rapid.Int64().Draw(t, "minor"), c)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return a
	})
}

func genWeights() *rapid.Generator[[]int64] {
	return rapid.Custom(func(t *rapid.T) []int64 {
		weights := rapid.SliceOfN(rapid.Int64Range(0, 1<<63-1), 1, 64).Draw(t, "weights")
		if !hasPositive(weights) {
			weights[rapid.IntRange(0, len(weights)-1).Draw(t, "index")] = rapid.Int64Range(1, 1<<63-1).Draw(t, "positive")
		}
		return weights
	})
}

func hasPositive(weights []int64) bool {
	for _, w := range weights {
		if w > 0 {
			return true
		}
	}
	return false
}

// Allocation never loses or invents a minor unit, whatever the amount and weights.
func TestPropertyAllocationSumsToInput(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genCurrency().Draw(t, "currency")
		a := genAmount(c).Draw(t, "amount")
		weights := genWeights().Draw(t, "weights")

		shares, err := a.Allocate(weights...)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		if len(shares) != len(weights) {
			t.Fatalf("got %d shares for %d weights", len(shares), len(weights))
		}
		sum := new(big.Int)
		for _, s := range shares {
			if s.Currency() != c {
				t.Fatalf("share %v is not in %v", s, c)
			}
			sum.Add(sum, big.NewInt(s.Minor()))
		}
		if sum.Cmp(big.NewInt(a.Minor())) != 0 {
			t.Fatalf("shares sum to %v, want %d", sum, a.Minor())
		}
	})
}

// Each share is within one minor unit of its exact proportional value, and a zero weight
// receives nothing.
func TestPropertyAllocationIsProportional(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		a := genAmount(money.BRL).Draw(t, "amount")
		weights := genWeights().Draw(t, "weights")

		shares, err := a.Allocate(weights...)
		if err != nil {
			t.Fatalf("Allocate: %v", err)
		}
		total := new(big.Int)
		for _, w := range weights {
			total.Add(total, big.NewInt(w))
		}
		for i, s := range shares {
			if weights[i] == 0 && !s.IsZero() {
				t.Fatalf("zero weight %d received %v", i, s)
			}
			exact := new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(a.Minor()), big.NewInt(weights[i])), total)
			diff := new(big.Rat).Sub(new(big.Rat).SetInt64(s.Minor()), exact)
			if diff.Abs(diff).Cmp(big.NewRat(1, 1)) >= 0 {
				t.Fatalf("share %d = %d is not within 1 of %v", i, s.Minor(), exact.FloatString(4))
			}
		}
	})
}

// Arithmetic across currencies is always an error, never a coercion.
func TestPropertyMixingCurrenciesIsAnError(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ca := genCurrency().Draw(t, "currency a")
		cb := genCurrency().Filter(func(c money.Currency) bool { return c != ca }).Draw(t, "currency b")
		a := genAmount(ca).Draw(t, "a")
		b := genAmount(cb).Draw(t, "b")

		if _, err := a.Add(b); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Add error = %v, want ErrCurrencyMismatch", err)
		}
		if _, err := a.Sub(b); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Sub error = %v, want ErrCurrencyMismatch", err)
		}
		if _, err := a.Cmp(b); !errors.Is(err, money.ErrCurrencyMismatch) {
			t.Fatalf("Cmp error = %v, want ErrCurrencyMismatch", err)
		}
	})
}

// Add and Sub agree with unbounded integer arithmetic: they return the exact result or
// ErrOverflow, never a wrapped value.
func TestPropertyAddSubMatchBigInt(t *testing.T) {
	minInt64, maxInt64 := big.NewInt(-1<<63), big.NewInt(1<<63-1)
	fits := func(v *big.Int) bool { return v.Cmp(minInt64) >= 0 && v.Cmp(maxInt64) <= 0 }

	rapid.Check(t, func(t *rapid.T) {
		a := genAmount(money.BRL).Draw(t, "a")
		b := genAmount(money.BRL).Draw(t, "b")
		ops := []struct {
			name string
			got  func() (money.Amount, error)
			want *big.Int
		}{
			{"Add", func() (money.Amount, error) { return a.Add(b) }, new(big.Int).Add(big.NewInt(a.Minor()), big.NewInt(b.Minor()))},
			{"Sub", func() (money.Amount, error) { return a.Sub(b) }, new(big.Int).Sub(big.NewInt(a.Minor()), big.NewInt(b.Minor()))},
		}
		for _, op := range ops {
			got, err := op.got()
			if !fits(op.want) {
				if !errors.Is(err, money.ErrOverflow) {
					t.Fatalf("%s(%d, %d) = %v, %v; want ErrOverflow", op.name, a.Minor(), b.Minor(), got, err)
				}
				continue
			}
			if err != nil || got.Minor() != op.want.Int64() {
				t.Fatalf("%s(%d, %d) = %v, %v; want %v", op.name, a.Minor(), b.Minor(), got, err, op.want)
			}
		}
	})
}

// Formatting and parsing are inverses.
func TestPropertyDecimalRoundTrips(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := genCurrency().Draw(t, "currency")
		a := genAmount(c).Draw(t, "amount")

		parsed, err := money.Parse(a.Decimal(), c)
		if err != nil {
			t.Fatalf("Parse(%q): %v", a.Decimal(), err)
		}
		if parsed != a {
			t.Fatalf("Parse(Decimal(%v)) = %v", a, parsed)
		}
	})
}

// Rounding never moves a result by a whole minor unit or more from the exact product.
func TestPropertyMulRateStaysWithinOneUnit(t *testing.T) {
	modes := []money.RoundingMode{
		money.HalfEven, money.HalfUp, money.HalfDown, money.Down, money.Up, money.Floor, money.Ceiling,
	}
	rapid.Check(t, func(t *rapid.T) {
		minor := rapid.Int64Range(-1<<40, 1<<40).Draw(t, "minor")
		a, err := money.New(minor, money.BRL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		num := rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, "rate numerator")
		rateString := new(big.Rat).SetFrac64(num, 10_000).FloatString(4)
		rate, err := money.ParseRate(rateString)
		if err != nil {
			t.Fatalf("ParseRate(%q): %v", rateString, err)
		}
		mode := rapid.SampledFrom(modes).Draw(t, "mode")

		got, err := a.MulRate(rate, mode)
		if err != nil {
			t.Fatalf("MulRate: %v", err)
		}
		exact := new(big.Rat).SetFrac64(minor*num, 10_000)
		diff := new(big.Rat).Sub(new(big.Rat).SetInt64(got.Minor()), exact)
		if diff.Abs(diff).Cmp(big.NewRat(1, 1)) >= 0 {
			t.Fatalf("%d × %s (%v) = %d, too far from %s", minor, rateString, mode, got.Minor(), exact.FloatString(4))
		}
	})
}
