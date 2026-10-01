package bizday_test

import (
	"testing"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
)

func date(s string) time.Time {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return d
}

func TestEaster(t *testing.T) {
	for year, want := range map[int]string{
		2024: "2024-03-31", 2025: "2025-04-20", 2026: "2026-04-05", 2027: "2027-03-28", 2038: "2038-04-25", 2000: "2000-04-23",
	} {
		if got := bizday.Easter(year).Format(time.DateOnly); got != want {
			t.Errorf("Easter %d = %s, want %s", year, got, want)
		}
	}
}

func TestHolidays2026(t *testing.T) {
	holidays := []string{
		"2026-01-01", "2026-02-16", "2026-02-17", "2026-04-03", "2026-04-21", "2026-05-01", "2026-06-04",
		"2026-09-07", "2026-10-12", "2026-11-02", "2026-11-20", "2026-12-25",
	}
	for _, h := range holidays {
		if !bizday.IsHoliday(date(h)) || bizday.IsBusinessDay(date(h)) {
			t.Errorf("%s is a holiday", h)
		}
	}
	// 15 November 2026 is a Sunday; Ash Wednesday is a business day.
	for _, d := range []string{"2026-02-18", "2026-11-19", "2026-11-23"} {
		if !bizday.IsBusinessDay(date(d)) {
			t.Errorf("%s is a business day", d)
		}
	}
	if bizday.IsHoliday(date("2023-11-20")) {
		t.Error("20 November became a national holiday in 2024")
	}
	count := 0
	for d := date("2026-01-01"); d.Year() == 2026; d = d.AddDate(0, 0, 1) {
		if bizday.IsBusinessDay(d) {
			count++
		}
	}
	// 261 weekdays, less the 12 holidays that fall on one (15 November is a Sunday).
	if count != 249 {
		t.Errorf("2026 has %d business days", count)
	}
}

func TestArithmetic(t *testing.T) {
	// Friday before Carnival 2026: the next business day is Ash Wednesday.
	if got := bizday.Add(date("2026-02-13"), 1); !got.Equal(date("2026-02-18")) {
		t.Errorf("Add = %s", got.Format(time.DateOnly))
	}
	if got := bizday.Add(date("2026-02-18"), -1); !got.Equal(date("2026-02-13")) {
		t.Errorf("Add back = %s", got.Format(time.DateOnly))
	}
	if got := bizday.Next(date("2026-04-03")); !got.Equal(date("2026-04-06")) {
		t.Errorf("Next after Good Friday = %s", got.Format(time.DateOnly))
	}
	if got := bizday.Next(date("2026-04-06")); !got.Equal(date("2026-04-06")) {
		t.Errorf("Next of a business day = %s", got.Format(time.DateOnly))
	}
	if n := bizday.Between(date("2026-02-13"), date("2026-02-20")); n != 3 {
		t.Errorf("Between = %d", n)
	}
	if n := bizday.Between(date("2026-02-20"), date("2026-02-13")); n != -3 {
		t.Errorf("Between backwards = %d", n)
	}
}
