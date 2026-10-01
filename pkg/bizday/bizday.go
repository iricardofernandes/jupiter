// Package bizday answers which days Brazil's financial system works: every weekday that
// is not a national holiday. The holidays are those of the national banking calendar
// (the one ANBIMA publishes): the fixed national holidays, Carnival Monday and Tuesday,
// Good Friday and Corpus Christi, which move with Easter, and Black Consciousness Day
// from 2024 (Lei 14.759/2023). State and municipal holidays are not included; the
// financial system settles on those days.
package bizday

import "time"

// IsBusinessDay reports whether the financial system works on d's calendar day.
func IsBusinessDay(d time.Time) bool {
	if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	return !IsHoliday(d)
}

// IsHoliday reports whether d's calendar day is a national holiday.
func IsHoliday(d time.Time) bool {
	y, m, day := d.Date()
	for _, h := range fixed {
		if h.month == m && h.day == day && y >= h.since {
			return true
		}
	}
	easter := Easter(y)
	for _, offset := range movable {
		e := easter.AddDate(0, 0, offset)
		if e.Month() == m && e.Day() == day {
			return true
		}
	}
	return false
}

// Next is d if it is a business day, or else the first business day after it.
func Next(d time.Time) time.Time {
	for !IsBusinessDay(d) {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

// Add moves n business days from d: forward for a positive n, backward for a negative
// one. Add(d, 0) is d, business day or not.
func Add(d time.Time, n int) time.Time {
	step := 1
	if n < 0 {
		step, n = -1, -n
	}
	for n > 0 {
		d = d.AddDate(0, 0, step)
		if IsBusinessDay(d) {
			n--
		}
	}
	return d
}

// Between counts the business days after from up to and including to; negative when to
// is before from.
func Between(from, to time.Time) int {
	from, to = day(from), day(to)
	sign := 1
	if to.Before(from) {
		from, to, sign = to, from, -1
	}
	n := 0
	for d := from.AddDate(0, 0, 1); !d.After(to); d = d.AddDate(0, 0, 1) {
		if IsBusinessDay(d) {
			n++
		}
	}
	return sign * n
}

func day(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// Easter is Easter Sunday of year y in the Gregorian calendar (the anonymous Gregorian
// algorithm), at midnight UTC.
func Easter(y int) time.Time {
	a := y % 19
	b, c := y/100, y%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return time.Date(y, time.Month(month), day, 0, 0, 0, 0, time.UTC)
}

type fixedHoliday struct {
	month time.Month
	day   int
	since int
}

var fixed = []fixedHoliday{
	{time.January, 1, 0},      // Confraternização Universal
	{time.April, 21, 0},       // Tiradentes
	{time.May, 1, 0},          // Dia do Trabalho
	{time.September, 7, 0},    // Independência
	{time.October, 12, 0},     // Nossa Senhora Aparecida
	{time.November, 2, 0},     // Finados
	{time.November, 15, 0},    // Proclamação da República
	{time.November, 20, 2024}, // Dia Nacional de Zumbi e da Consciência Negra
	{time.December, 25, 0},    // Natal
}

// movable are days from Easter Sunday: Carnival Monday and Tuesday, Good Friday and
// Corpus Christi.
var movable = []int{-48, -47, -2, 60}
