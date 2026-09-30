// Package card holds the rules a card must pass before the vault stores it.
package card

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/vault"
)

// Number is a card number that passed its checks: digits only, a length its brand
// allows, and a valid Luhn check digit.
type Number struct {
	digits string
	brand  string
}

func ParseNumber(s string) (Number, error) {
	digits := strings.NewReplacer(" ", "", "-", "").Replace(s)
	if len(digits) < 12 || len(digits) > 19 || strings.Trim(digits, "0123456789") != "" || !luhn(digits) {
		return Number{}, invalid("incorrect_number", "number")
	}
	b := brandOf(digits)
	if !slices.Contains(b.lengths, len(digits)) {
		return Number{}, invalid("incorrect_number", "number")
	}
	return Number{digits: digits, brand: b.name}, nil
}

func (n Number) String() string { return n.digits }

func (n Number) Brand() string { return n.brand }

// BIN is the part of the number PCI DSS allows to be shown beside the last four: the
// first eight digits of a number of sixteen or more, since eight-digit BINs, and the
// first six of a shorter one.
func (n Number) BIN() string {
	if len(n.digits) >= 16 {
		return n.digits[:8]
	}
	return n.digits[:6]
}

func (n Number) Last4() string { return n.digits[len(n.digits)-4:] }

func luhn(digits string) bool {
	sum := 0
	for i := range len(digits) {
		d := int(digits[len(digits)-1-i] - '0')
		if i%2 == 1 {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
	}
	return sum%10 == 0
}

type brand struct {
	name    string
	lengths []int
	// ranges are inclusive, compared on the leading digits of the number, as many as
	// the bounds have.
	ranges [][2]string
}

// Order matters: Elo and Hipercard take ranges inside what would otherwise be Visa or
// Discover. Elo publishes no range table the research could find; these are the ranges
// open-source card libraries (Braintree's credit-card-type) use, and are unverified.
var brands = []brand{
	{name: "elo", lengths: []int{16}, ranges: [][2]string{
		{"401178", "401179"},
		{"431274", "431274"},
		{"438935", "438935"},
		{"451416", "451416"},
		{"457393", "457393"},
		{"457631", "457632"},
		{"504175", "504175"},
		{"506699", "506778"},
		{"509000", "509999"},
		{"627780", "627780"},
		{"636297", "636297"},
		{"636368", "636368"},
		{"650031", "650033"},
		{"650035", "650051"},
		{"650405", "650439"},
		{"650485", "650538"},
		{"650541", "650598"},
		{"650700", "650718"},
		{"650720", "650727"},
		{"650901", "650978"},
		{"651652", "651679"},
		{"655000", "655019"},
		{"655021", "655058"},
	}},
	{name: "hipercard", lengths: []int{13, 16, 19}, ranges: [][2]string{
		{"606282", "606282"}, {"384100", "384100"}, {"384140", "384140"}, {"384160", "384160"},
	}},
	{name: "amex", lengths: []int{15}, ranges: [][2]string{{"34", "34"}, {"37", "37"}}},
	{name: "mastercard", lengths: []int{16}, ranges: [][2]string{{"51", "55"}, {"2221", "2720"}}},
	{name: "visa", lengths: []int{13, 16, 19}, ranges: [][2]string{{"4", "4"}}},
}

var unknownBrand = brand{name: "unknown", lengths: []int{12, 13, 14, 15, 16, 17, 18, 19}}

func brandOf(digits string) brand {
	for _, b := range brands {
		for _, r := range b.ranges {
			prefix := digits[:len(r[0])]
			if prefix >= r[0] && prefix <= r[1] {
				return b
			}
		}
	}
	return unknownBrand
}

// maxYearsAhead bounds an expiry date: issuers print at most a few years ahead, so a
// date further out is a typing mistake.
const maxYearsAhead = 20

// CheckExpiry accepts a card through the last day of its expiry month.
func CheckExpiry(month, year int, now time.Time) error {
	if month < 1 || month > 12 {
		return invalid("invalid_expiry_month", "exp_month")
	}
	current := now.UTC().Year()
	switch {
	case year < 1000 || year > current+maxYearsAhead:
		return invalid("invalid_expiry_year", "exp_year")
	case year < current:
		return invalid("expired_card", "exp_year")
	case year == current && month < int(now.UTC().Month()):
		return invalid("expired_card", "exp_month")
	}
	return nil
}

// CheckCVC accepts no code, since a stored card is charged again without one, or one
// of the length the brand prints.
func CheckCVC(n Number, cvc string) error {
	if cvc == "" {
		return nil
	}
	want := 3
	if n.brand == "amex" {
		want = 4
	}
	if len(cvc) != want {
		return invalid("invalid_cvc", "cvc")
	}
	if _, err := strconv.ParseUint(cvc, 10, 16); err != nil {
		return invalid("invalid_cvc", "cvc")
	}
	return nil
}

func invalid(code, param string) error {
	return &vault.CardError{Code: code, Param: param}
}
