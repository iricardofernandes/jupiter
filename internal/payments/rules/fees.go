package rules

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"time"

	"github.com/iricardofernandes/jupiter/internal/money"
)

// Fee is one row of the price table.
type Fee struct {
	Scheme, FinancedBy string
	From, To           int
	EffectiveFrom      time.Time
	Rate               money.Rate
	Source             string
}

//go:embed card_fees.csv
var feesCSV string

var fees = mustParseFees(feesCSV)

// CardFee is the price of a card payment of scheme in installments financed by
// financedBy ("" for a payment in one go), captured at at. ok is false when no row
// applies.
func CardFee(scheme, financedBy string, installments int, at time.Time) (Fee, bool) {
	if installments < 1 {
		installments = 1
	}
	var best Fee
	bestScore, found := -1, false
	for _, f := range fees {
		if f.EffectiveFrom.After(at) || !matches(f.Scheme, scheme) || !matches(f.FinancedBy, financedBy) ||
			installments < f.From || installments > f.To {
			continue
		}
		score := 0
		if f.FinancedBy != "*" {
			score++
		}
		if f.Scheme != "*" {
			score += 2
		}
		if score > bestScore || (score == bestScore && f.EffectiveFrom.After(best.EffectiveFrom)) {
			best, bestScore, found = f, score, true
		}
	}
	return best, found
}

func mustParseFees(data string) []Fee {
	rows, err := parseFees(data)
	if err != nil {
		panic(err)
	}
	return rows
}

func parseFees(data string) ([]Fee, error) {
	r, err := table(data, []string{"scheme", "financed_by", "installments_from", "installments_to", "effective_from", "rate", "source"})
	if err != nil {
		return nil, fmt.Errorf("rules: card fees: %w", err)
	}
	var out []Fee
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("rules: card fees: %w", err)
		}
		from, err1 := strconv.Atoi(rec[2])
		to, err2 := strconv.Atoi(rec[3])
		if err1 != nil || err2 != nil || from < 1 || to < from || to > 12 {
			return nil, fmt.Errorf("rules: card fees: installments %q to %q", rec[2], rec[3])
		}
		effective, err := time.Parse(time.DateOnly, rec[4])
		if err != nil {
			return nil, fmt.Errorf("rules: card fees: effective_from %q: %w", rec[4], err)
		}
		rate, err := money.ParseRate(rec[5])
		if err != nil || rec[5][0] == '-' {
			return nil, fmt.Errorf("rules: card fees: rate %q", rec[5])
		}
		if !slices.Contains([]string{"*", "merchant", "issuer"}, rec[1]) {
			return nil, fmt.Errorf("rules: card fees: financed_by %q", rec[1])
		}
		out = append(out, Fee{Scheme: rec[0], FinancedBy: rec[1], From: from, To: to, EffectiveFrom: effective, Rate: rate, Source: rec[6]})
	}
}
