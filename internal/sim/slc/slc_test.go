package slc_test

import (
	"errors"
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/internal/sim/slc"
)

func TestADaysSettlement(t *testing.T) {
	now := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC) // a Tuesday
	s := slc.New(slc.Config{Now: func() time.Time { return now }, Participants: []slc.Participant{{Token: "t", TaxID: "11222333000181", ISPB: "30000001"}}})
	grade := slc.Grade{Date: "2026-11-03", Entries: []slc.Entry{
		{ID: "u1/1", Holder: "11222333000262", Arrangement: "VCC", SettlementDate: "2026-11-03", Beneficiary: "11222333000262", Amount: 9000, Domicile: slc.Domicile{ISPB: "30000001", Account: "rp_1"}},
		{ID: "u1/2", Holder: "11222333000262", Arrangement: "VCC", SettlementDate: "2026-11-03", Beneficiary: "33000167000101", Contract: "loan", Amount: 500, Domicile: slc.Domicile{ISPB: "33000167", Account: "loans"}},
	}}
	if g, err := s.Submit("t", grade); err != nil || g.Status != "accepted" || g.Total != 9500 {
		t.Fatalf("submitting: %+v, %v", g, err)
	}
	if _, err := s.Submit("t", grade); err != nil {
		t.Fatalf("the same grade again: %v", err)
	}
	grade.Entries[0].Amount = 1
	if _, err := s.Submit("t", grade); !errors.Is(err, slc.ErrConflict) {
		t.Fatalf("another grade for the day: %v", err)
	}
	s.Tick()
	if g, _ := s.GradeOf("t", "2026-11-03"); g.Status != "settled" || g.Credited != 9000 || g.PaidOther != 500 {
		t.Fatalf("settled: %+v", g)
	}
	if _, err := s.Submit("t", slc.Grade{Date: "2026-11-04", Entries: grade.Entries}); !errors.Is(err, slc.ErrInvalid) {
		t.Fatalf("tomorrow's grade today: %v", err)
	}
}

func TestAnticipationReports(t *testing.T) {
	now := time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC) // Tuesday
	s := slc.New(slc.Config{Now: func() time.Time { return now }, Participants: []slc.Participant{{Token: "t", TaxID: "11222333000181", ISPB: "30000001"}}})
	if err := s.ReportAnticipations("t", []slc.Report{
		{ID: "on time", Amount: 100, AnticipatedOn: "2026-10-05"},
		{ID: "late", Amount: 100, AnticipatedOn: "2026-10-02"}, // Friday: due Monday
	}); err != nil {
		t.Fatal(err)
	}
	reports, late := s.Reports("t")
	if len(reports) != 2 || len(late) != 1 || late[0].Report != "late" || late[0].Due != "2026-10-05" {
		t.Fatalf("reports %+v, late %+v", reports, late)
	}
}
