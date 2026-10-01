// Package rules holds the rules a scheme or regulator can change, as data with the date
// each takes effect (ADR 0006), and answers them as of a given day.
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
)

type Presence string

const (
	CardPresent    Presence = "card_present"
	CardNotPresent Presence = "card_not_present"
)

type Initiator string

const (
	Customer Initiator = "customer"
	Merchant Initiator = "merchant"
)

type Kind string

const (
	Final     Kind = "final"
	Estimated Kind = "estimated"
)

// Authorization describes an authorization for the validity lookup.
type Authorization struct {
	Scheme    string
	Presence  Presence
	Initiator Initiator
	Kind      Kind
}

// Validity is one row of the table.
type Validity struct {
	Scheme, Presence, Initiator, Kind string
	EffectiveFrom                     time.Time
	Duration                          time.Duration
	Verified                          bool
	Source                            string
}

//go:embed authorization_validity.csv
var validityCSV string

var validities = mustParse(validityCSV)

// AuthorizationValidity is how long a, authorized at at, stays valid, and the rule
// that says so.
func AuthorizationValidity(a Authorization, at time.Time) Validity {
	var best Validity
	bestScore := -1
	for _, v := range validities {
		if v.EffectiveFrom.After(at) || !matches(v.Scheme, a.Scheme) || !matches(v.Presence, string(a.Presence)) ||
			!matches(v.Initiator, string(a.Initiator)) || !matches(v.Kind, string(a.Kind)) {
			continue
		}
		score := specificity(v)
		if score > bestScore || (score == bestScore && v.EffectiveFrom.After(best.EffectiveFrom)) {
			best, bestScore = v, score
		}
	}
	return best
}

func matches(rule, value string) bool {
	return rule == "*" || rule == value
}

// specificity ranks a rule by how many of its columns are exact, the scheme first.
func specificity(v Validity) int {
	score := 0
	for i, column := range []string{v.Kind, v.Initiator, v.Presence, v.Scheme} {
		if column != "*" {
			score += 1 << i
		}
	}
	return score
}

func mustParse(data string) []Validity {
	rows, err := parse(data)
	if err != nil {
		panic(err)
	}
	return rows
}

// table reads a rule file: comment lines start with #, and the header must be columns.
func table(data string, columns []string) (*csv.Reader, error) {
	lines := make([]string, 0)
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

func parse(data string) ([]Validity, error) {
	r, err := table(data, []string{"scheme", "presence", "initiator", "kind", "effective_from", "validity_hours", "verified", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: authorization validity %w", err)
	}
	var out []Validity
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("rules: authorization validity: %w", err)
		}
		from, err := time.Parse(time.DateOnly, rec[4])
		if err != nil {
			return nil, fmt.Errorf("rules: effective_from %q: %w", rec[4], err)
		}
		hours, err := strconv.Atoi(rec[5])
		if err != nil || hours <= 0 {
			return nil, fmt.Errorf("rules: validity_hours %q", rec[5])
		}
		if rec[6] != "cited" && rec[6] != "unverified" {
			return nil, fmt.Errorf("rules: verified must be cited or unverified, not %q", rec[6])
		}
		out = append(out, Validity{
			Scheme: rec[0], Presence: rec[1], Initiator: rec[2], Kind: rec[3], EffectiveFrom: from,
			Duration: time.Duration(hours) * time.Hour, Verified: rec[6] == "cited", Source: rec[7],
		})
	}
	return out, nil
}
