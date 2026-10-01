package rules

import (
	"testing"
	"time"
)

func day(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestTables(t *testing.T) {
	term, ok := TermFor("visa", "merchant", day("2026-10-01"))
	if !ok || term.FirstDays != 30 || term.IntervalDays != 30 || term.Verified {
		t.Fatalf("term = %+v, %t", term, ok)
	}
	// Friday 2 October 2026 plus one business day is Monday 5 October.
	if due := DeadlineFor(UpdateAfterSale, day("2026-10-02")).Due(day("2026-10-02")); !due.Equal(day("2026-10-05")) {
		t.Fatalf("update due %s", due.Format(time.DateOnly))
	}
	if d := DeadlineFor(ReconcileWeekly, day("2026-10-02")); d.Business || d.Due(day("2026-10-02")) != day("2026-10-09") {
		t.Fatalf("weekly = %+v", d)
	}
	if d := DeadlineFor(ReleaseEffects, day("2026-10-01")); d.Verified || d.Days != 2 {
		t.Fatalf("release effects = %+v", d)
	}
}

func TestMalformedTables(t *testing.T) {
	for name, data := range map[string]string{
		"columns":  "scheme,days\n*,30\n",
		"interval": "scheme,financed_by,effective_from,first_days,interval_days,verified,source\n*,*,2000-01-01,30,0,cited,x\n",
		"verified": "scheme,financed_by,effective_from,first_days,interval_days,verified,source\n*,*,2000-01-01,30,30,maybe,x\n",
	} {
		if _, err := parseTerms(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := parseDeadlines("rule,effective_from,days,unit,verified,source\nx,2000-01-01,1,weeks,cited,y\n"); err == nil {
		t.Error("a deadline in weeks parsed")
	}
}
