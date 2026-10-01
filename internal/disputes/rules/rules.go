// Package rules holds the dispute rules a network or regulator can change, as data with
// the date each takes effect (ADR 0006): how long each party has at each stage, the
// participants' liability cap, MED's windows, and the thresholds disputes and fraud are
// monitored against.
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

// Stages and windows the deadlines table keeps.
const (
	// Represent is how long the acquirer has to answer a chargeback.
	Represent = "represent"
	// IssuerResponse is how long the issuer has to answer a representment; past it, the
	// acquirer wins.
	IssuerResponse = "issuer_response"
	// PreArbitration is how long the acquirer has to accept or escalate a pre-arbitration.
	PreArbitration = "pre_arbitration"
	// Arbitration is how long the network takes to rule.
	Arbitration = "arbitration"
	// LiabilityCap is how long after authorization participants stay liable.
	LiabilityCap = "liability_cap"
	// MerchantMargin is how much earlier than the network's deadline the merchant's is.
	MerchantMargin = "merchant_margin"

	MEDRequest      = "med_request"
	MEDContestation = "med_contestation"
	MEDNotification = "med_notification"
	MEDBlock        = "med_block"
	MEDBlockApplied = "med_block_applied"
	MEDResponse     = "med_response"
)

// Pix is the network name MED's rows are kept under.
const Pix = "pix"

type Unit string

const (
	Calendar Unit = "calendar"
	Business Unit = "business"
	Minutes  Unit = "minutes"
)

// Deadline is one row of the table: Length units from the moment that starts it.
type Deadline struct {
	Network, Stage string
	EffectiveFrom  time.Time
	Length         int
	Unit           Unit
	Verified       string
	Source         string
}

// Due is when the deadline counted from start ends.
func (d Deadline) Due(start time.Time) time.Time {
	switch d.Unit {
	case Minutes:
		return start.Add(time.Duration(d.Length) * time.Minute)
	case Business:
		return bizday.Add(start, d.Length)
	default:
		return start.AddDate(0, 0, d.Length)
	}
}

// Before is when the deadline counted back from end starts: what is Length units before.
func (d Deadline) Before(end time.Time) time.Time {
	switch d.Unit {
	case Minutes:
		return end.Add(-time.Duration(d.Length) * time.Minute)
	case Business:
		return bizday.Add(end, -d.Length)
	default:
		return end.AddDate(0, 0, -d.Length)
	}
}

// Threshold is a monitoring program's line: excessive at or above Bps with at least
// MinimumCount events.
type Threshold struct {
	Program       string
	EffectiveFrom time.Time
	Bps           int64
	MinimumCount  int64
	Verified      string
	Source        string
}

// DisputeRatio is disputes and fraud reports over card transactions.
const DisputeRatio = "dispute_ratio"

var (
	//go:embed deadlines.csv
	deadlinesCSV string
	//go:embed monitoring.csv
	monitoringCSV string

	deadlines  = must(parseDeadlines(deadlinesCSV))
	thresholds = must(parseThresholds(monitoringCSV))
)

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// For is the deadline of a stage for a network in effect at at; an exact network
// outranks "*". It panics when none is: the table must cover every stage.
func For(network, stage string, at time.Time) Deadline {
	var best Deadline
	bestScore := -1
	for _, d := range deadlines {
		if d.Stage != stage || d.EffectiveFrom.After(at) || (d.Network != "*" && d.Network != network) {
			continue
		}
		score := 0
		if d.Network != "*" {
			score = 1
		}
		if score > bestScore || (score == bestScore && d.EffectiveFrom.After(best.EffectiveFrom)) {
			best, bestScore = d, score
		}
	}
	if bestScore < 0 {
		panic("rules: no deadline " + stage + " for " + network)
	}
	return best
}

// Has reports whether any row covers a stage for a network at at; the liability cap, for
// one, does not apply before it took effect.
func Has(network, stage string, at time.Time) bool {
	return slices.ContainsFunc(deadlines, func(d Deadline) bool {
		return d.Stage == stage && !d.EffectiveFrom.After(at) && (d.Network == "*" || d.Network == network)
	})
}

// ThresholdFor is a program's threshold in effect at at.
func ThresholdFor(program string, at time.Time) Threshold {
	var best Threshold
	for _, t := range thresholds {
		if t.Program == program && !t.EffectiveFrom.After(at) && (best.Program == "" || t.EffectiveFrom.After(best.EffectiveFrom)) {
			best = t
		}
	}
	if best.Program == "" {
		panic("rules: no threshold for " + program)
	}
	return best
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

func verified(s string) error {
	if s != "cited" && s != "unverified" && s != "policy" {
		return fmt.Errorf("verified must be cited, unverified or policy, not %q", s)
	}
	return nil
}

func parseDeadlines(data string) ([]Deadline, error) {
	r, err := table(data, []string{"network", "stage", "effective_from", "length", "unit", "verified", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: deadlines %w", err)
	}
	var out []Deadline
	err = rows(r, func(rec []string) error {
		from, err := time.Parse(time.DateOnly, rec[2])
		if err != nil {
			return err
		}
		length, err := strconv.Atoi(rec[3])
		unit := Unit(rec[4])
		if err != nil || length < 1 || (unit != Calendar && unit != Business && unit != Minutes) {
			return fmt.Errorf("%q %q", rec[3], rec[4])
		}
		if err := verified(rec[5]); err != nil {
			return err
		}
		out = append(out, Deadline{Network: rec[0], Stage: rec[1], EffectiveFrom: from, Length: length, Unit: unit, Verified: rec[5], Source: rec[6]})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rules: deadlines: %w", err)
	}
	return out, nil
}

func parseThresholds(data string) ([]Threshold, error) {
	r, err := table(data, []string{"program", "effective_from", "threshold_bps", "minimum_count", "verified", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: monitoring %w", err)
	}
	var out []Threshold
	err = rows(r, func(rec []string) error {
		from, err := time.Parse(time.DateOnly, rec[1])
		if err != nil {
			return err
		}
		bps, err1 := strconv.ParseInt(rec[2], 10, 64)
		count, err2 := strconv.ParseInt(rec[3], 10, 64)
		if err1 != nil || err2 != nil || bps < 1 || bps > 10_000 || count < 0 {
			return fmt.Errorf("%q %q", rec[2], rec[3])
		}
		if err := verified(rec[4]); err != nil {
			return err
		}
		out = append(out, Threshold{Program: rec[0], EffectiveFrom: from, Bps: bps, MinimumCount: count, Verified: rec[4], Source: rec[5]})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("rules: monitoring: %w", err)
	}
	return out, nil
}
