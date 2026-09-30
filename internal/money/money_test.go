package money_test

import (
	"errors"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/money"
)

func mustNew(t *testing.T, minor int64, c money.Currency) money.Amount {
	t.Helper()
	a, err := money.New(minor, c)
	if err != nil {
		t.Fatalf("New(%d, %v): %v", minor, c, err)
	}
	return a
}

func TestCurrencyByCode(t *testing.T) {
	c, err := money.CurrencyByCode("BRL")
	if err != nil {
		t.Fatalf("CurrencyByCode(BRL): %v", err)
	}
	if c != money.BRL || c.Code() != "BRL" || c.Exponent() != 2 {
		t.Fatalf("got %v with exponent %d, want BRL with exponent 2", c, c.Exponent())
	}

	for _, code := range []string{"", "brl", "XXX", "BRLL"} {
		if _, err := money.CurrencyByCode(code); !errors.Is(err, money.ErrUnknownCurrency) {
			t.Errorf("CurrencyByCode(%q) error = %v, want ErrUnknownCurrency", code, err)
		}
	}
}

func TestNewRejectsZeroCurrency(t *testing.T) {
	if _, err := money.New(100, money.Currency{}); !errors.Is(err, money.ErrUnknownCurrency) {
		t.Fatalf("New with zero currency error = %v, want ErrUnknownCurrency", err)
	}
}

func TestZeroValueAmountIsInvalid(t *testing.T) {
	var zero money.Amount
	if _, err := zero.Add(zero); !errors.Is(err, money.ErrUnknownCurrency) {
		t.Fatalf("zero Amount Add error = %v, want ErrUnknownCurrency", err)
	}
}

func TestAddAndSub(t *testing.T) {
	a := mustNew(t, 60000, money.BRL)
	b := mustNew(t, 150, money.BRL)

	sum, err := a.Add(b)
	if err != nil || sum.Minor() != 60150 || sum.Currency() != money.BRL {
		t.Fatalf("Add = %v, %v; want 601.50 BRL", sum, err)
	}
	diff, err := b.Sub(a)
	if err != nil || diff.Minor() != -59850 {
		t.Fatalf("Sub = %v, %v; want -598.50 BRL", diff, err)
	}
}

func TestOverflowIsAnError(t *testing.T) {
	maxAmount := mustNew(t, 1<<63-1, money.BRL)
	minAmount := mustNew(t, -1<<63, money.BRL)
	one := mustNew(t, 1, money.BRL)

	if _, err := maxAmount.Add(one); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("max+1 error = %v, want ErrOverflow", err)
	}
	if _, err := minAmount.Sub(one); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("min-1 error = %v, want ErrOverflow", err)
	}
	if _, err := minAmount.Neg(); !errors.Is(err, money.ErrOverflow) {
		t.Errorf("-min error = %v, want ErrOverflow", err)
	}
}

func TestMixingCurrenciesIsAnError(t *testing.T) {
	brl := mustNew(t, 100, money.BRL)
	usd := mustNew(t, 100, money.USD)

	if _, err := brl.Add(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Add error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Sub(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Sub error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := brl.Cmp(usd); !errors.Is(err, money.ErrCurrencyMismatch) {
		t.Errorf("Cmp error = %v, want ErrCurrencyMismatch", err)
	}
}

func TestSign(t *testing.T) {
	tests := []struct {
		minor                          int64
		isZero, isPositive, isNegative bool
	}{
		{0, true, false, false},
		{1, false, true, false},
		{-1, false, false, true},
	}
	for _, tt := range tests {
		a := mustNew(t, tt.minor, money.BRL)
		if a.IsZero() != tt.isZero || a.IsPositive() != tt.isPositive || a.IsNegative() != tt.isNegative {
			t.Errorf("%v: IsZero=%t IsPositive=%t IsNegative=%t", a, a.IsZero(), a.IsPositive(), a.IsNegative())
		}
	}
}

func TestCmp(t *testing.T) {
	small := mustNew(t, 1, money.BRL)
	large := mustNew(t, 2, money.BRL)
	for _, tt := range []struct {
		a, b money.Amount
		want int
	}{{small, large, -1}, {large, small, 1}, {small, small, 0}} {
		got, err := tt.a.Cmp(tt.b)
		if err != nil || got != tt.want {
			t.Errorf("%v.Cmp(%v) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
}

func TestFormatting(t *testing.T) {
	tests := []struct {
		minor    int64
		currency money.Currency
		decimal  string
		str      string
	}{
		{60000, money.BRL, "600.00", "BRL 600.00"},
		{5, money.BRL, "0.05", "BRL 0.05"},
		{-5, money.BRL, "-0.05", "BRL -0.05"},
		{-123456, money.USD, "-1234.56", "USD -1234.56"},
		{500, money.JPY, "500", "JPY 500"},
		{-1 << 63, money.BRL, "-92233720368547758.08", "BRL -92233720368547758.08"},
	}
	for _, tt := range tests {
		a := mustNew(t, tt.minor, tt.currency)
		if got := a.Decimal(); got != tt.decimal {
			t.Errorf("Decimal(%d %v) = %q, want %q", tt.minor, tt.currency, got, tt.decimal)
		}
		if got := a.String(); got != tt.str {
			t.Errorf("String(%d %v) = %q, want %q", tt.minor, tt.currency, got, tt.str)
		}
	}
}

func TestParse(t *testing.T) {
	valid := []struct {
		in       string
		currency money.Currency
		minor    int64
	}{
		{"600.00", money.BRL, 60000},
		{"600", money.BRL, 60000},
		{"600.5", money.BRL, 60050},
		{"0.01", money.BRL, 1},
		{"-0.01", money.BRL, -1},
		{"92233720368547758.07", money.BRL, 1<<63 - 1},
		{"-92233720368547758.08", money.BRL, -1 << 63},
		{"500", money.JPY, 500},
	}
	for _, tt := range valid {
		a, err := money.Parse(tt.in, tt.currency)
		if err != nil || a.Minor() != tt.minor || a.Currency() != tt.currency {
			t.Errorf("Parse(%q, %v) = %v, %v; want %d", tt.in, tt.currency, a, err, tt.minor)
		}
	}

	invalid := []struct {
		in       string
		currency money.Currency
	}{
		{"", money.BRL},
		{"1.001", money.BRL}, // more precision than the currency has: never rounded silently
		{"1.", money.BRL},
		{".5", money.BRL},
		{"+1.00", money.BRL},
		{"1,00", money.BRL},
		{"1e2", money.BRL},
		{" 1.00", money.BRL},
		{"--1", money.BRL},
		{"1.5", money.JPY},
		{"92233720368547758.08", money.BRL},
	}
	for _, tt := range invalid {
		if _, err := money.Parse(tt.in, tt.currency); !errors.Is(err, money.ErrInvalidAmount) && !errors.Is(err, money.ErrOverflow) {
			t.Errorf("Parse(%q, %v) error = %v, want ErrInvalidAmount or ErrOverflow", tt.in, tt.currency, err)
		}
	}

	if _, err := money.Parse("1.00", money.Currency{}); !errors.Is(err, money.ErrUnknownCurrency) {
		t.Errorf("Parse with zero currency error = %v, want ErrUnknownCurrency", err)
	}
}
