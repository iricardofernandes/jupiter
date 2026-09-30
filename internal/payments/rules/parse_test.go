package rules

import (
	"testing"
	"time"
)

func TestParseRefusesMalformedTables(t *testing.T) {
	header := "scheme,presence,initiator,kind,effective_from,validity_hours,verified,source\n"
	for name, data := range map[string]string{
		"no header":     "",
		"wrong columns": "scheme,days\nvisa,10\n",
		"bad date":      header + "visa,*,*,*,2024-13-01,24,cited,x\n",
		"zero hours":    header + "visa,*,*,*,2024-01-01,0,cited,x\n",
		"bad verified":  header + "visa,*,*,*,2024-01-01,24,maybe,x\n",
		"short row":     header + "visa,*,*\n",
	} {
		if _, err := parse(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	rows, err := parse("# a comment\n" + header + "visa,*,*,*,2024-01-01,24,cited,x\n")
	if err != nil || len(rows) != 1 {
		t.Fatalf("parse = %v, %v", rows, err)
	}
}

func TestSpecificityOutranksDate(t *testing.T) {
	header := "scheme,presence,initiator,kind,effective_from,validity_hours,verified,source\n"
	rows, err := parse(header +
		"visa,card_not_present,customer,final,2024-04-13,240,cited,specific\n" +
		"visa,*,*,*,2027-01-01,48,cited,newer but general\n" +
		"visa,card_not_present,customer,final,2026-01-01,120,cited,a later change to the specific rule\n")
	if err != nil {
		t.Fatal(err)
	}
	saved := validities
	validities = rows
	defer func() { validities = saved }()
	a := Authorization{Scheme: "visa", Presence: CardNotPresent, Initiator: Customer, Kind: Final}
	at, _ := time.Parse(time.DateOnly, "2027-06-01")
	if got := AuthorizationValidity(a, at); got.Source != "a later change to the specific rule" {
		t.Fatalf("picked %q", got.Source)
	}
	merchant := Authorization{Scheme: "visa", Presence: CardNotPresent, Initiator: Merchant, Kind: Final}
	if got := AuthorizationValidity(merchant, at); got.Source != "newer but general" {
		t.Fatalf("with no specific row, picked %q", got.Source)
	}
}
