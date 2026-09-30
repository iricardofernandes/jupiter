package money_test

import (
	"errors"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/money"
)

func mustRate(t *testing.T, s string) money.Rate {
	t.Helper()
	r, err := money.ParseRate(s)
	if err != nil {
		t.Fatalf("ParseRate(%q): %v", s, err)
	}
	return r
}

func TestParseRate(t *testing.T) {
	for _, s := range []string{"0", "1", "0.0249", "-0.5", "12.345678901234567890"} {
		if got := mustRate(t, s).String(); got != s {
			t.Errorf("ParseRate(%q).String() = %q", s, got)
		}
	}
	for _, s := range []string{"", "1/3", "1e-2", ".5", "1.", "+1", "0x10", "NaN", "Inf", " 1"} {
		if _, err := money.ParseRate(s); !errors.Is(err, money.ErrInvalidRate) {
			t.Errorf("ParseRate(%q) error = %v, want ErrInvalidRate", s, err)
		}
	}
}

func TestZeroValueRateIsInvalid(t *testing.T) {
	var zero money.Rate
	if _, err := mustNew(t, 100, money.BRL).MulRate(zero, money.HalfEven); !errors.Is(err, money.ErrInvalidRate) {
		t.Fatalf("MulRate with zero Rate error = %v, want ErrInvalidRate", err)
	}
}

// Each row multiplies an amount by a rate whose exact product has a known fractional
// part, so every rounding mode is checked on ties, non-ties and both signs.
func TestMulRateRoundingModes(t *testing.T) {
	type want struct{ halfEven, halfUp, halfDown, down, up, floor, ceiling int64 }
	tests := []struct {
		minor int64
		rate  string
		want  want
	}{
		{25, "0.1", want{2, 3, 2, 2, 3, 2, 3}},         // 2.5
		{35, "0.1", want{4, 4, 3, 3, 4, 3, 4}},         // 3.5
		{-25, "0.1", want{-2, -3, -2, -2, -3, -3, -2}}, // -2.5
		{26, "0.1", want{3, 3, 3, 2, 3, 2, 3}},         // 2.6
		{24, "0.1", want{2, 2, 2, 2, 3, 2, 3}},         // 2.4
		{-24, "0.1", want{-2, -2, -2, -2, -3, -3, -2}}, // -2.4
		{20, "0.1", want{2, 2, 2, 2, 2, 2, 2}},         // exact
		{60000, "0.0249", want{1494, 1494, 1494, 1494, 1494, 1494, 1494}},
		{10001, "0.0249", want{249, 249, 249, 249, 250, 249, 250}}, // 249.0249
	}
	modes := []struct {
		mode money.RoundingMode
		pick func(want) int64
	}{
		{money.HalfEven, func(w want) int64 { return w.halfEven }},
		{money.HalfUp, func(w want) int64 { return w.halfUp }},
		{money.HalfDown, func(w want) int64 { return w.halfDown }},
		{money.Down, func(w want) int64 { return w.down }},
		{money.Up, func(w want) int64 { return w.up }},
		{money.Floor, func(w want) int64 { return w.floor }},
		{money.Ceiling, func(w want) int64 { return w.ceiling }},
	}
	for _, tt := range tests {
		for _, m := range modes {
			got, err := mustNew(t, tt.minor, money.BRL).MulRate(mustRate(t, tt.rate), m.mode)
			if err != nil {
				t.Errorf("%d × %s (%v): %v", tt.minor, tt.rate, m.mode, err)
				continue
			}
			if want := m.pick(tt.want); got.Minor() != want {
				t.Errorf("%d × %s (%v) = %d, want %d", tt.minor, tt.rate, m.mode, got.Minor(), want)
			}
		}
	}
}

func TestMulRateRequiresARoundingMode(t *testing.T) {
	var unspecified money.RoundingMode
	if _, err := mustNew(t, 100, money.BRL).MulRate(mustRate(t, "0.5"), unspecified); !errors.Is(err, money.ErrRoundingMode) {
		t.Fatalf("MulRate with unspecified mode error = %v, want ErrRoundingMode", err)
	}
}

func TestMulRateOverflow(t *testing.T) {
	if _, err := mustNew(t, 1<<62, money.BRL).MulRate(mustRate(t, "2"), money.HalfEven); !errors.Is(err, money.ErrOverflow) {
		t.Fatalf("MulRate overflow error = %v, want ErrOverflow", err)
	}
}
