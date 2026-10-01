package reconciliation

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The matching, documented in docs/reconciliation.md. Within one stream of one
// counterparty, each of Jupiter's records is matched to one of the counterparty's:
//
//  1. Exact: the same key, amount and direction. Matched at once, whatever their dates.
//  2. The same key and direction for another amount: an amount mismatch.
//  3. A record whose key, amount and direction were matched already: a duplicate.
//  4. Of the rest, a pair of keys alike, of the same amount and direction, no more than
//     two days apart: a probable match, which a person confirms.
//  5. Anything left, dated on or before the day reconciled: missing on the other side.
//
// Only rule 1 resolves anything without a person. A record dated after the day waits.

type Side string

const (
	Ours   Side = "jupiter"
	Theirs Side = "counterparty"
)

type Direction string

const (
	In  Direction = "in"
	Out Direction = "out"
)

// Record is a movement of money as one side recorded it.
type Record struct {
	ID           int64
	Counterparty string
	Stream       string
	Side         Side
	// Identity names the record on its side: Jupiter's object, the counterparty's line.
	Identity string
	// Key is what both sides know the movement by.
	Key       string
	Direction Direction
	Amount    int64
	Date      time.Time
	Merchant  string
	Reference string
}

// Kinds of break.
const (
	MissingAtCounterparty = "missing_at_counterparty"
	MissingAtJupiter      = "missing_at_jupiter"
	Duplicate             = "duplicate"
	AmountMismatch        = "amount_mismatch"
	ProbableMatch         = "probable_match"
	KindDivergence        = "divergence"
)

// RuleExact is the rule exact matches are made by.
const RuleExact = "exact"

// Scores out of 100: a key counts most, then the amount, the direction and the date.
const (
	scoreKey       = 50
	scoreKeyAlike  = 20
	scoreAmount    = 30
	scoreDirection = 10
	scoreSameDay   = 10
	scoreNextDay   = 5
	probableAt     = 60
	probableDays   = 2
	// alikeFrom is the shortest key compared for likeness, and alikeEdits how many
	// characters two keys of one length may differ by and still be alike: a mistyped or
	// truncated reference, not two movements numbered by one institution.
	alikeFrom  = 8
	alikeEdits = 2
)

type Pair struct {
	Ours, Theirs int64
}

// Finding is a break the matching found: its kind, the record, the other side's record
// it concerns, if any, and how alike the two are.
type Finding struct {
	Kind    string
	Record  Record
	Other   *Record
	Score   int
	Reasons []string
}

type Outcome struct {
	Pairs    []Pair
	Findings []Finding
}

type signature struct {
	key       string
	direction Direction
	amount    int64
}

func signatureOf(r Record) signature { return signature{r.Key, r.Direction, r.Amount} }

// Match matches a stream's unmatched records, as of day. matched are records of the
// stream matched before that share a key with them: what makes a record a duplicate.
func Match(day time.Time, ours, theirs, matched []Record) Outcome {
	ours, theirs = sorted(ours), sorted(theirs)
	used := map[int64]bool{}
	seen := map[Side]map[signature]bool{Ours: {}, Theirs: {}}
	for _, r := range matched {
		seen[r.Side][signatureOf(r)] = true
	}
	out := Outcome{Pairs: exact(ours, theirs, used, seen)}
	out.Findings = append(out.Findings, mismatches(ours, theirs, used)...)
	out.Findings = append(out.Findings, duplicates(day, ours, theirs, used, seen)...)
	out.Findings = append(out.Findings, probables(day, ours, theirs, used)...)
	out.Findings = append(out.Findings, missing(day, ours, theirs, used)...)
	return out
}

// exact pairs records of the same key, amount and direction.
func exact(ours, theirs []Record, used map[int64]bool, seen map[Side]map[signature]bool) []Pair {
	bySignature := map[signature][]Record{}
	for _, t := range theirs {
		bySignature[signatureOf(t)] = append(bySignature[signatureOf(t)], t)
	}
	var out []Pair
	for _, o := range ours {
		candidates := bySignature[signatureOf(o)]
		if len(candidates) == 0 {
			continue
		}
		t := candidates[0]
		bySignature[signatureOf(o)] = candidates[1:]
		used[o.ID], used[t.ID] = true, true
		seen[Ours][signatureOf(o)], seen[Theirs][signatureOf(t)] = true, true
		out = append(out, Pair{Ours: o.ID, Theirs: t.ID})
	}
	return out
}

// duplicates are the records left whose key, amount and direction were matched already.
func duplicates(day time.Time, ours, theirs []Record, used map[int64]bool, seen map[Side]map[signature]bool) []Finding {
	var out []Finding
	for _, r := range append(slices.Clone(ours), theirs...) {
		if !used[r.ID] && !r.Date.After(day) && seen[r.Side][signatureOf(r)] {
			used[r.ID] = true
			out = append(out, Finding{Kind: Duplicate, Record: r, Score: 100, Reasons: []string{"its key, amount and direction were matched already"}})
		}
	}
	return out
}

// missing are the records left that are due by day.
func missing(day time.Time, ours, theirs []Record, used map[int64]bool) []Finding {
	var out []Finding
	for _, r := range append(slices.Clone(ours), theirs...) {
		if used[r.ID] || r.Date.After(day) {
			continue
		}
		kind := MissingAtCounterparty
		if r.Side == Theirs {
			kind = MissingAtJupiter
		}
		out = append(out, Finding{Kind: kind, Record: r, Reasons: []string{"nothing on the other side with its key"}})
	}
	return out
}

// mismatches pairs records of the same key and direction but another amount.
func mismatches(ours, theirs []Record, used map[int64]bool) []Finding {
	type keyed struct {
		key       string
		direction Direction
	}
	byKey := map[keyed][]Record{}
	for _, t := range theirs {
		if !used[t.ID] {
			byKey[keyed{t.Key, t.Direction}] = append(byKey[keyed{t.Key, t.Direction}], t)
		}
	}
	var out []Finding
	for _, o := range ours {
		k := keyed{o.Key, o.Direction}
		if used[o.ID] || len(byKey[k]) == 0 {
			continue
		}
		t := byKey[k][0]
		byKey[k] = byKey[k][1:]
		used[o.ID], used[t.ID] = true, true
		score, reasons := Score(o, t)
		out = append(out, Finding{Kind: AmountMismatch, Record: o, Other: &t, Score: score, Reasons: reasons})
	}
	return out
}

// probables pairs each of Jupiter's remaining records due by day with the counterparty's
// most like it, if it is alike enough.
func probables(day time.Time, ours, theirs []Record, used map[int64]bool) []Finding {
	var out []Finding
	for _, o := range ours {
		if used[o.ID] || o.Date.After(day) {
			continue
		}
		best, bestScore := -1, 0
		var bestReasons []string
		for i, t := range theirs {
			if used[t.ID] || t.Amount != o.Amount || t.Direction != o.Direction || daysApart(o.Date, t.Date) > probableDays || !alike(o.Key, t.Key) {
				continue
			}
			if score, reasons := Score(o, t); score > bestScore {
				best, bestScore, bestReasons = i, score, reasons
			}
		}
		if best < 0 || bestScore < probableAt {
			continue
		}
		t := theirs[best]
		used[o.ID], used[t.ID] = true, true
		out = append(out, Finding{Kind: ProbableMatch, Record: o, Other: &t, Score: bestScore, Reasons: bestReasons})
	}
	return out
}

// Score is how alike two records are, out of 100, and why.
func Score(a, b Record) (int, []string) {
	score := 0
	var reasons []string
	switch {
	case a.Key == b.Key:
		score += scoreKey
		reasons = append(reasons, "the same key")
	case alike(a.Key, b.Key):
		score += scoreKeyAlike
		reasons = append(reasons, "keys alike")
	default:
		reasons = append(reasons, "different keys")
	}
	if a.Amount == b.Amount {
		score += scoreAmount
		reasons = append(reasons, "the same amount")
	} else {
		reasons = append(reasons, fmt.Sprintf("amounts %d apart", abs(a.Amount-b.Amount)))
	}
	if a.Direction == b.Direction {
		score += scoreDirection
	} else {
		reasons = append(reasons, "opposite directions")
	}
	switch d := daysApart(a.Date, b.Date); d {
	case 0:
		score += scoreSameDay
		reasons = append(reasons, "the same day")
	case 1:
		score += scoreNextDay
		reasons = append(reasons, "a day apart")
	default:
		reasons = append(reasons, fmt.Sprintf("%d days apart", d))
	}
	return score, reasons
}

func sorted(records []Record) []Record {
	out := slices.Clone(records)
	slices.SortFunc(out, func(a, b Record) int { return cmp.Or(a.Date.Compare(b.Date), cmp.Compare(a.ID, b.ID)) })
	return out
}

func daysApart(a, b time.Time) int {
	d := int(a.Sub(b).Hours() / 24)
	if d < 0 {
		return -d
	}
	return d
}

// alike says whether two different keys may name one movement: one is the other cut
// short, or they differ in a character or two.
func alike(a, b string) bool {
	if a == b || len(a) < alikeFrom || len(b) < alikeFrom {
		return false
	}
	if strings.HasPrefix(a, b) || strings.HasPrefix(b, a) {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	edits := 0
	for i := range len(a) {
		if a[i] != b[i] {
			edits++
		}
	}
	return edits <= alikeEdits
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
