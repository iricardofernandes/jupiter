package card_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"pgregory.net/rapid"

	"github.com/iricardofernandes/jupiter/internal/vault"
	"github.com/iricardofernandes/jupiter/internal/vault/card"
)

func TestNumber(t *testing.T) {
	tests := []struct {
		in, want, brand, bin string
	}{
		{"4242424242424242", "4242424242424242", "visa", "42424242"},
		{"4242 4242 4242 4242", "4242424242424242", "visa", "42424242"},
		{"4242-4242-4242-4242", "4242424242424242", "visa", "42424242"},
		{"5555555555554444", "5555555555554444", "mastercard", "55555555"},
		{"2223003122003222", "2223003122003222", "mastercard", "22230031"},
		{"378282246310005", "378282246310005", "amex", "378282"},
		{"6362970000457013", "6362970000457013", "elo", "63629700"},
		{"4011780000000006", "4011780000000006", "elo", "40117800"},
		{"6062825624254001", "6062825624254001", "hipercard", "60628256"},
		{"4222222222222", "4222222222222", "visa", "422222"},
		{"6011111111111117", "6011111111111117", "unknown", "60111111"},
	}
	for _, tt := range tests {
		n, err := card.ParseNumber(tt.in)
		if err != nil {
			t.Errorf("ParseNumber(%q): %v", tt.in, err)
			continue
		}
		if n.String() != tt.want || n.Brand() != tt.brand || n.BIN() != tt.bin || n.Last4() != tt.want[len(tt.want)-4:] {
			t.Errorf("ParseNumber(%q) = %s %s %s %s, want %s %s %s", tt.in, n, n.Brand(), n.BIN(), n.Last4(), tt.want, tt.brand, tt.bin)
		}
	}
}

func TestNumberRejects(t *testing.T) {
	for _, in := range []string{
		"", "4242424242424241", "4242", "42424242424242424242", "4242x42424242424",
		"378282246310005 1", "５５５５５５５５５５５５４４４４",
		"4242424242424",
	} {
		if _, err := card.ParseNumber(in); !isCardError(err, "incorrect_number") {
			t.Errorf("ParseNumber(%q) = %v, want incorrect_number", in, err)
		}
	}
	// A Mastercard has sixteen digits; a Luhn-valid number of another length is refused.
	if _, err := card.ParseNumber("5555555555554"); !isCardError(err, "incorrect_number") {
		t.Errorf("a thirteen-digit Mastercard was accepted: %v", err)
	}
}

func TestLuhnProperty(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		body := rapid.StringMatching(`[1-9][0-9]{11,17}`).Draw(t, "body")
		valid := body + strconv.Itoa(checkDigit(body))
		n, err := card.ParseNumber(valid)
		if err != nil && !isCardError(err, "incorrect_number") {
			t.Fatalf("ParseNumber(%s): %v", valid, err)
		}
		if err == nil && n.String() != valid {
			t.Fatalf("ParseNumber(%s) = %s", valid, n)
		}
		// Changing any one digit breaks the checksum.
		i := rapid.IntRange(0, len(valid)-1).Draw(t, "position")
		d := rapid.IntRange(1, 9).Draw(t, "delta")
		changed := []byte(valid)
		changed[i] = byte('0' + (int(changed[i]-'0')+d)%10)
		if _, err := card.ParseNumber(string(changed)); err == nil {
			t.Fatalf("%s passed Luhn after changing digit %d of %s", changed, i, valid)
		}
	})
}

func checkDigit(body string) int {
	sum := 0
	for i := len(body) - 1; i >= 0; i-- {
		d := int(body[i] - '0')
		if (len(body)-i)%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return (10 - sum%10) % 10
}

func TestExpiry(t *testing.T) {
	now := time.Date(2026, 9, 30, 23, 0, 0, 0, time.UTC)
	valid := [][2]int{{9, 2026}, {12, 2026}, {1, 2027}, {9, 2046}}
	for _, v := range valid {
		if err := card.CheckExpiry(v[0], v[1], now); err != nil {
			t.Errorf("CheckExpiry(%d, %d): %v", v[0], v[1], err)
		}
	}
	invalid := map[[2]int]string{
		{0, 2027}:  "invalid_expiry_month",
		{13, 2027}: "invalid_expiry_month",
		{8, 2026}:  "expired_card",
		{12, 2025}: "expired_card",
		{1, 2047}:  "invalid_expiry_year",
		{1, 27}:    "invalid_expiry_year",
	}
	for v, code := range invalid {
		if err := card.CheckExpiry(v[0], v[1], now); !isCardError(err, code) {
			t.Errorf("CheckExpiry(%d, %d) = %v, want %s", v[0], v[1], err, code)
		}
	}
}

func TestCVC(t *testing.T) {
	visa, _ := card.ParseNumber("4242424242424242")
	amex, _ := card.ParseNumber("378282246310005")
	for _, tt := range []struct {
		n     card.Number
		cvc   string
		valid bool
	}{
		{visa, "123", true},
		{visa, "", true},
		{visa, "1234", false},
		{visa, "12a", false},
		{amex, "1234", true},
		{amex, "123", false},
	} {
		err := card.CheckCVC(tt.n, tt.cvc)
		if (err == nil) != tt.valid || (err != nil && !isCardError(err, "invalid_cvc")) {
			t.Errorf("CheckCVC(%s, %q) = %v, want valid %v", tt.n.Brand(), tt.cvc, err, tt.valid)
		}
	}
}

func isCardError(err error, code string) bool {
	var ce *vault.CardError
	return errors.As(err, &ce) && ce.Code == code && errors.Is(err, vault.ErrInvalidCard)
}
