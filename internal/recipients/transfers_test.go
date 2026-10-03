package recipients

import (
	"testing"
	"time"
)

// A weekly payout whose day is a holiday is paid on the next business day, though that
// is in the week after; and so a monthly one carried over into the next month.
func TestAScheduleAHolidayCarriesOver(t *testing.T) {
	day := func(s string) time.Time {
		d, err := time.Parse(time.DateOnly, s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	fridays := Transfers{Interval: Weekly, Day: 5}
	// Fridays 25 December 2026 and 1 January 2027 are holidays; the Mondays after are not.
	for date, want := range map[string]bool{"2026-12-18": true, "2026-12-25": false, "2026-12-28": true, "2026-12-29": false, "2027-01-04": true, "2027-01-11": false} {
		if got := fridays.Due(day(date)); got != want {
			t.Errorf("a Friday payout on %s: %t", date, got)
		}
	}
	// 28 February 2026 is a Saturday: paid on Monday 2 March.
	on28 := Transfers{Interval: Monthly, Day: 28}
	for date, want := range map[string]bool{"2026-02-27": false, "2026-03-02": true, "2026-03-27": false, "2026-03-30": true} {
		if got := on28.Due(day(date)); got != want {
			t.Errorf("a payout on the 28th, on %s: %t", date, got)
		}
	}
}
