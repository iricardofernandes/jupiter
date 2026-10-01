// Package rules holds the receivables rules a scheme or regulator can change, as data
// with the date each takes effect (ADR 0006): when the network pays each installment,
// and the registry's deadlines.
package rules

import (
	_ "embed"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/pkg/bizday"
)

// Term is when a scheme pays the installments of a payment.
type Term struct {
	Scheme, FinancedBy string
	EffectiveFrom      time.Time
	FirstDays          int
	IntervalDays       int
	Verified           bool
	Source             string
}

// Deadline is a registry deadline: Days business days, or calendar days.
type Deadline struct {
	Rule          string
	EffectiveFrom time.Time
	Days          int
	Business      bool
	Verified      bool
	Source        string
}

// The deadlines the adapter keeps.
const (
	UpdateAfterSale      = "update_after_sale"
	SettlementNotice     = "settlement_notice"
	ReconcileDaily       = "reconcile_daily"
	ReconcileWeekly      = "reconcile_weekly"
	ReconcileFortnightly = "reconcile_fortnightly"
	FixDivergence        = "fix_divergence"
	ReleaseEffects       = "release_effects"
)

var (
	//go:embed settlement_terms.csv
	termsCSV string
	//go:embed registry_deadlines.csv
	deadlinesCSV string
	//go:embed anticipation_rates.csv
	ratesCSV string

	terms     = must(parseTerms(termsCSV))
	deadlines = must(parseDeadlines(deadlinesCSV))
	rates     = must(parseRates(ratesCSV))
)

// Rate is Jupiter's monthly rate for anticipations from a date.
type Rate struct {
	EffectiveFrom time.Time
	Monthly       string
	Source        string
}

// RateFor is the anticipation rate in effect at at.
func RateFor(at time.Time) Rate {
	var best Rate
	for _, r := range rates {
		if !r.EffectiveFrom.After(at) && (best.Monthly == "" || r.EffectiveFrom.After(best.EffectiveFrom)) {
			best = r
		}
	}
	if best.Monthly == "" {
		panic("rules: no anticipation rate in effect")
	}
	return best
}

func parseRates(data string) ([]Rate, error) {
	r, err := table(data, []string{"effective_from", "monthly_rate", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: anticipation rates %w", err)
	}
	var out []Rate
	err = rows(r, func(rec []string) error {
		from, err := time.Parse(time.DateOnly, rec[0])
		if err != nil {
			return err
		}
		if rate, err := strconv.ParseFloat(rec[1], 64); err != nil || rate <= 0 || rate >= 1 || strings.HasPrefix(rec[1], "-") {
			return fmt.Errorf("monthly_rate %q", rec[1])
		}
		out = append(out, Rate{EffectiveFrom: from, Monthly: rec[1], Source: rec[2]})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rules: anticipation rates: %w", err)
	}
	return out, nil
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TermFor is the most specific term in effect at at for scheme and financing.
func TermFor(scheme, financedBy string, at time.Time) (Term, bool) {
	var best Term
	bestScore, found := -1, false
	for _, t := range terms {
		if t.EffectiveFrom.After(at) || !matches(t.Scheme, scheme) || !matches(t.FinancedBy, financedBy) {
			continue
		}
		score := 0
		if t.FinancedBy != "*" {
			score++
		}
		if t.Scheme != "*" {
			score += 2
		}
		if score > bestScore || (score == bestScore && t.EffectiveFrom.After(best.EffectiveFrom)) {
			best, bestScore, found = t, score, true
		}
	}
	return best, found
}

// DeadlineFor is the deadline rule in effect at at.
func DeadlineFor(rule string, at time.Time) Deadline {
	var best Deadline
	for _, d := range deadlines {
		if d.Rule == rule && !d.EffectiveFrom.After(at) && (best.Rule == "" || d.EffectiveFrom.After(best.EffectiveFrom)) {
			best = d
		}
	}
	if best.Rule == "" {
		panic("rules: no deadline " + rule)
	}
	return best
}

// Due is the last day of the deadline counted from day.
func (d Deadline) Due(day time.Time) time.Time {
	if d.Business {
		return bizday.Add(day, d.Days)
	}
	return day.AddDate(0, 0, d.Days)
}

func matches(rule, value string) bool {
	return rule == "*" || rule == value
}

func table(data string, columns []string) (*csv.Reader, error) {
	var lines []string
	for line := range strings.SplitSeq(data, "\n") {
		if !strings.HasPrefix(line, "#") && strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	r := csv.NewReader(strings.NewReader(strings.Join(lines, "\n")))
	header, err := r.Read()
	if err != nil {
		return nil, fmt.Errorf("header: %w", err)
	}
	if !slices.Equal(header, columns) {
		return nil, fmt.Errorf("columns are %v, want %v", header, columns)
	}
	return r, nil
}

func rows(r *csv.Reader, each func([]string) error) error {
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err == nil {
			err = each(rec)
		}
		if err != nil {
			return err
		}
	}
}

func verified(s string) (bool, error) {
	if s != "cited" && s != "unverified" {
		return false, fmt.Errorf("verified must be cited or unverified, not %q", s)
	}
	return s == "cited", nil
}

func parseTerms(data string) ([]Term, error) {
	r, err := table(data, []string{"scheme", "financed_by", "effective_from", "first_days", "interval_days", "verified", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: settlement terms %w", err)
	}
	var out []Term
	err = rows(r, func(rec []string) error {
		from, err := time.Parse(time.DateOnly, rec[2])
		if err != nil {
			return err
		}
		first, err1 := strconv.Atoi(rec[3])
		interval, err2 := strconv.Atoi(rec[4])
		if err1 != nil || err2 != nil || first < 0 || interval < 1 {
			return fmt.Errorf("days %q and %q", rec[3], rec[4])
		}
		v, err := verified(rec[5])
		out = append(out, Term{Scheme: rec[0], FinancedBy: rec[1], EffectiveFrom: from, FirstDays: first, IntervalDays: interval, Verified: v, Source: rec[6]})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("rules: settlement terms: %w", err)
	}
	return out, nil
}

func parseDeadlines(data string) ([]Deadline, error) {
	r, err := table(data, []string{"rule", "effective_from", "days", "unit", "verified", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: registry deadlines %w", err)
	}
	var out []Deadline
	err = rows(r, func(rec []string) error {
		from, err := time.Parse(time.DateOnly, rec[1])
		if err != nil {
			return err
		}
		days, err := strconv.Atoi(rec[2])
		if err != nil || days < 1 || (rec[3] != "business" && rec[3] != "calendar") {
			return fmt.Errorf("%q %q days", rec[2], rec[3])
		}
		v, err := verified(rec[4])
		out = append(out, Deadline{Rule: rec[0], EffectiveFrom: from, Days: days, Business: rec[3] == "business", Verified: v, Source: rec[5]})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("rules: registry deadlines: %w", err)
	}
	return out, nil
}
