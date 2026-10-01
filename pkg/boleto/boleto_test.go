package boleto_test

import (
	"errors"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/boleto"
)

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

// A Bradesco boleto's barcode, from the research (notes 03 §5): due 1 November 2008,
// R$ 1,240.20.
const bradesco = "23797404300001240200448056168623793601105800"

func TestARealBarcode(t *testing.T) {
	b, err := boleto.Barcode(bradesco).Parse()
	if err != nil {
		t.Fatal(err)
	}
	if b.Bank != "237" || b.Currency != boleto.Real || b.Factor != 4043 || b.Amount != 124020 || b.FreeField != "0448056168623793601105800" {
		t.Fatalf("parts: %+v", b)
	}
	again, err := b.Barcode()
	if err != nil || again != bradesco {
		t.Fatalf("rebuilt %s, %v", again, err)
	}
	if due, err := boleto.DueDate(b.Factor, date("2008-10-20")); err != nil || !due.Equal(date("2008-11-01")) {
		t.Fatalf("due %s, %v", due, err)
	}
	line := boleto.Barcode(bradesco).Line()
	if len(line) != 47 {
		t.Fatalf("line %s", line)
	}
	back, err := boleto.ParseLine(boleto.FormatLine(line))
	if err != nil || back != bradesco {
		t.Fatalf("the typed line %s read back as %s, %v", boleto.FormatLine(line), back, err)
	}
}

func TestCheckDigitsAreChecked(t *testing.T) {
	wrong := bradesco[:4] + "8" + bradesco[5:]
	if _, err := boleto.Barcode(wrong).Parse(); !errors.Is(err, boleto.ErrInvalid) {
		t.Fatalf("a wrong barcode check digit: %v", err)
	}
	line := boleto.Barcode(bradesco).Line()
	for _, i := range []int{9, 20, 31, 32, 40} {
		b := []byte(line)
		b[i] = '0' + (b[i]-'0'+1)%10
		if _, err := boleto.ParseLine(string(b)); err == nil {
			t.Errorf("digit %d changed, still read", i)
		}
	}
}

// The factor counted to 9999 on 21 February 2025 and started again at 1000 the next day.
func TestTheFactorRollover(t *testing.T) {
	for day, want := range map[string]int{
		"2000-07-03": 1000, "2008-11-01": 4043, "2025-02-21": 9999, "2025-02-22": 1000, "2025-02-23": 1001,
		"2026-10-01": 1586, "2049-10-13": 9999, "2049-10-14": 1000,
	} {
		f, err := boleto.Factor(date(day))
		if err != nil || f != want {
			t.Errorf("%s: factor %d, %v; want %d", day, f, err, want)
		}
	}
	// 1586 meant 2 March 2002 too; read in 2026 it is 2026.
	if due, _ := boleto.DueDate(1586, date("2026-09-30")); !due.Equal(date("2026-10-01")) {
		t.Errorf("1586 read in 2026 is %s", due.Format(time.DateOnly))
	}
	if due, _ := boleto.DueDate(9999, date("2025-02-01")); !due.Equal(date("2025-02-21")) {
		t.Errorf("9999 read in early 2025 is %s", due.Format(time.DateOnly))
	}
	if _, err := boleto.Factor(date("1999-01-01")); err == nil {
		t.Error("a due date before factor 1000")
	}
}

func FuzzParseLine(f *testing.F) {
	f.Add(boleto.Barcode(bradesco).Line())
	f.Add("")
	f.Fuzz(func(t *testing.T, line string) {
		barcode, err := boleto.ParseLine(line)
		if err != nil {
			return
		}
		if _, err := barcode.Parse(); err != nil {
			t.Fatalf("a typed line read into a barcode that does not parse: %v", err)
		}
	})
}
