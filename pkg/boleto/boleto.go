// Package boleto builds and reads the barcode and typed line (linha digitável) of a
// Brazilian collection boleto (boleto de cobrança).
//
// The barcode is 44 digits: the bank (3), the currency (1, 9 for the real), a check digit
// over the other 43 (mod 11), the due-date factor (4), the amount in centavos (10) and a
// free field the bank lays out (25). The typed line is the same content in five fields of
// 10, 11, 11, 1 and 14 digits, the first three each with a mod 10 check digit, the fourth
// the barcode's check digit, the fifth the factor and the amount.
//
// The research verified these algorithms against a real Bradesco barcode; it did not read
// FEBRABAN's collection barcode specification itself (docs/research, notes 03 §5).
package boleto

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

var ErrInvalid = errors.New("boleto: invalid")

// Real is the currency digit of the real.
const Real = '9'

// MaxAmount is the largest amount a barcode holds: ten digits of centavos.
const MaxAmount = 99_999_999_99

// Barcode is a boleto's 44-digit barcode.
type Barcode string

// Boleto is the parts of a barcode.
type Boleto struct {
	Bank      string // 3 digits
	Currency  byte
	Factor    int // the due-date factor; 0 for no due date
	Amount    int64
	FreeField string // 25 digits
}

// Barcode is the boleto's barcode, with its check digit.
func (b Boleto) Barcode() (Barcode, error) {
	switch {
	case len(b.Bank) != 3 || !digits(b.Bank):
		return "", fmt.Errorf("%w: bank %q is not 3 digits", ErrInvalid, b.Bank)
	case b.Currency < '0' || b.Currency > '9':
		return "", fmt.Errorf("%w: currency %q", ErrInvalid, b.Currency)
	case b.Factor != 0 && (b.Factor < 1000 || b.Factor > 9999):
		return "", fmt.Errorf("%w: due-date factor %d", ErrInvalid, b.Factor)
	case b.Amount < 0 || b.Amount > MaxAmount:
		return "", fmt.Errorf("%w: amount %d", ErrInvalid, b.Amount)
	case len(b.FreeField) != 25 || !digits(b.FreeField):
		return "", fmt.Errorf("%w: the free field is not 25 digits", ErrInvalid)
	}
	body := fmt.Sprintf("%s%c%04d%010d%s", b.Bank, b.Currency, b.Factor, b.Amount, b.FreeField)
	return Barcode(body[:4] + strconv.Itoa(barcodeDV(body)) + body[4:]), nil
}

// barcodeDV is the mod 11 check digit over the 43 digits other than itself: weights 2 to
// 9 from the right, cycling; 11 less the remainder, or 1 when that is 0, 10 or 11.
func barcodeDV(body string) int {
	sum, weight := 0, 2
	for i := len(body) - 1; i >= 0; i-- {
		sum += int(body[i]-'0') * weight
		if weight++; weight > 9 {
			weight = 2
		}
	}
	r := 11 - sum%11
	if r == 0 || r == 10 || r == 11 {
		return 1
	}
	return r
}

// mod10 is the check digit of a typed-line field: weights 2 and 1 from the right, the
// digits of each product summed, then 10 less the remainder, 0 for 10.
func mod10(s string) int {
	sum, weight := 0, 2
	for i := len(s) - 1; i >= 0; i-- {
		p := int(s[i]-'0') * weight
		sum += p/10 + p%10
		weight = 3 - weight
	}
	return (10 - sum%10) % 10
}

// Parse checks a barcode and returns its parts.
func (c Barcode) Parse() (Boleto, error) {
	s := string(c)
	if len(s) != 44 || !digits(s) {
		return Boleto{}, fmt.Errorf("%w: a barcode is 44 digits", ErrInvalid)
	}
	if want := barcodeDV(s[:4] + s[5:]); int(s[4]-'0') != want {
		return Boleto{}, fmt.Errorf("%w: check digit %c, want %d", ErrInvalid, s[4], want)
	}
	factor, _ := strconv.Atoi(s[5:9])
	amount, _ := strconv.ParseInt(s[9:19], 10, 64)
	return Boleto{Bank: s[:3], Currency: s[3], Factor: factor, Amount: amount, FreeField: s[19:]}, nil
}

// Line is the barcode's 47-digit typed line.
func (c Barcode) Line() string {
	s := string(c)
	f1 := s[0:4] + s[19:24]
	f2 := s[24:34]
	f3 := s[34:44]
	return fmt.Sprintf("%s%d%s%d%s%d%s%s", f1, mod10(f1), f2, mod10(f2), f3, mod10(f3), s[4:5], s[5:19])
}

// FormatLine spaces and dots a typed line as it is printed:
// 00000.00000 00000.000000 00000.000000 0 00000000000000.
func FormatLine(line string) string {
	if len(line) != 47 {
		return line
	}
	return line[0:5] + "." + line[5:10] + " " + line[10:15] + "." + line[15:21] + " " +
		line[21:26] + "." + line[26:32] + " " + line[32:33] + " " + line[33:]
}

// ParseLine reads a typed line, with or without its spaces and dots, checks its digits
// and returns its barcode.
func ParseLine(line string) (Barcode, error) {
	line = strings.NewReplacer(" ", "", ".", "").Replace(line)
	if len(line) != 47 || !digits(line) {
		return "", fmt.Errorf("%w: a typed line is 47 digits", ErrInvalid)
	}
	for _, f := range []struct{ body, dv string }{{line[0:9], line[9:10]}, {line[10:20], line[20:21]}, {line[21:31], line[31:32]}} {
		if strconv.Itoa(mod10(f.body)) != f.dv {
			return "", fmt.Errorf("%w: field %s has check digit %s", ErrInvalid, f.body, f.dv)
		}
	}
	barcode := Barcode(line[0:4] + line[32:33] + line[33:47] + line[4:9] + line[10:20] + line[21:31])
	if _, err := barcode.Parse(); err != nil {
		return "", err
	}
	return barcode, nil
}

func digits(s string) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// The due-date factor counts days from 7 October 1997. It reached 9999 on 21 February
// 2025 and started again at 1000 the next day; every 9000 days it does so again.
var base = time.Date(1997, 10, 7, 0, 0, 0, 0, time.UTC)

const cycle = 9000

// Factor is a due date's factor.
func Factor(due time.Time) (int, error) {
	d := days(base, due)
	if d < 1000 {
		return 0, fmt.Errorf("%w: due dates start on %s", ErrInvalid, base.AddDate(0, 0, 1000).Format(time.DateOnly))
	}
	return (d-1000)%cycle + 1000, nil
}

// DueDate is the date a factor stands for: of the dates it may mean, one per cycle, the
// one nearest near. FEBRABAN gives no rule for this; nearness to the day the boleto is
// read is what readers do.
func DueDate(factor int, near time.Time) (time.Time, error) {
	if factor < 1000 || factor > 9999 {
		return time.Time{}, fmt.Errorf("%w: due-date factor %d", ErrInvalid, factor)
	}
	first := base.AddDate(0, 0, factor)
	n := days(first, near) / cycle
	best := first
	for k := max(n-1, 0); k <= n+1; k++ {
		candidate := first.AddDate(0, 0, k*cycle)
		if abs(days(candidate, near)) < abs(days(best, near)) {
			best = candidate
		}
	}
	return best, nil
}

func days(from, to time.Time) int {
	y, m, d := to.Date()
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	y, m, d = from.Date()
	f := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return int(t.Sub(f).Hours() / 24)
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
