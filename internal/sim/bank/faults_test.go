package bank_test

import (
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/bank"
)

// Credits from other institutions reach the statement, a line each with an id of its
// own; a fault loses a line, writes it twice, or puts it on the next business day.
func TestStatementFaults(t *testing.T) {
	now := time.Date(2026, 10, 2, 15, 0, 0, 0, time.UTC) // a Friday
	s := bank.New(bank.Config{
		Now: func() time.Time { return now }, Host: "bank.example",
		Clients: []bank.Client{{Token: token, TaxID: "11222333000181", Name: "Jupiter", Agreement: "C", Account: account}},
		Faults: func(e bank.Event) bank.Fault {
			return bank.Fault{Drop: e.Reference == "lost", Duplicate: e.Reference == "twice", Delay: e.Reference == "late"}
		},
	})
	for _, ref := range []string{"kept", "lost", "twice", "late"} {
		if err := s.Credit("11222333000181", "2026-10-02", 100, ref, "CREDITO"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Credit("99999999000191", "2026-10-02", 100, "x", "CREDITO"); err == nil {
		t.Fatal("a credit to no account")
	}
	friday, _ := s.Statement(token, "2026-10-02")
	monday, _ := s.Statement(token, "2026-10-05")
	if len(friday) != 3 || friday[0].Reference != "kept" || friday[1].ID == friday[2].ID || len(monday) != 1 || monday[0].Reference != "late" {
		t.Fatalf("Friday %+v, Monday %+v", friday, monday)
	}
}
