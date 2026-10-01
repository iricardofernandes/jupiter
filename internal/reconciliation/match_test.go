package reconciliation

import (
	"testing"
	"time"
)

func day(d int) time.Time { return time.Date(2026, 10, d, 0, 0, 0, 0, time.UTC) }

func rec(id int64, side Side, key string, amount int64, d int) Record {
	return Record{ID: id, Side: side, Key: key, Direction: In, Amount: amount, Date: day(d)}
}

func kinds(o Outcome) map[string][]int64 {
	out := map[string][]int64{}
	for _, f := range o.Findings {
		out[f.Kind] = append(out[f.Kind], f.Record.ID)
	}
	return out
}

func TestExactMatchesIgnoreDates(t *testing.T) {
	o := Match(day(5), []Record{rec(1, Ours, "E1", 100, 5)}, []Record{rec(2, Theirs, "E1", 100, 6)}, nil)
	if len(o.Pairs) != 1 || o.Pairs[0] != (Pair{1, 2}) || len(o.Findings) != 0 {
		t.Fatalf("a delayed record: %+v", o)
	}
}

func TestBreaks(t *testing.T) {
	ours := []Record{
		rec(1, Ours, "A", 100, 5), // matched
		rec(2, Ours, "B", 200, 5), // missing at the counterparty
		rec(3, Ours, "C", 300, 5), // another amount there
		rec(4, Ours, "D", 400, 7), // dated after the day: waits
		rec(5, Ours, "PIX-20261005-AAAA", 500, 5),
	}
	theirs := []Record{
		rec(11, Theirs, "A", 100, 5),
		rec(12, Theirs, "A", 100, 5), // listed twice
		rec(13, Theirs, "C", 310, 5),
		rec(14, Theirs, "Z", 900, 5), // Jupiter has nothing of it
		rec(15, Theirs, "PIX-20261005-AAAB", 500, 6),
		rec(16, Theirs, "E", 600, 5), // matched before: a duplicate
	}
	matched := []Record{rec(9, Theirs, "E", 600, 4)}
	o := Match(day(5), ours, theirs, matched)
	got := kinds(o)
	want := map[string][]int64{
		MissingAtCounterparty: {2}, AmountMismatch: {3}, Duplicate: {12, 16}, MissingAtJupiter: {14}, ProbableMatch: {5},
	}
	for k, ids := range want {
		if len(got[k]) != len(ids) {
			t.Fatalf("%s: %v, want %v (all: %v)", k, got[k], ids, got)
		}
		for i := range ids {
			if got[k][i] != ids[i] {
				t.Fatalf("%s: %v, want %v", k, got[k], ids)
			}
		}
	}
	if len(o.Pairs) != 1 || o.Pairs[0] != (Pair{1, 11}) {
		t.Fatalf("pairs: %+v", o.Pairs)
	}
	for _, f := range o.Findings {
		if f.Kind == ProbableMatch && (f.Other == nil || f.Other.ID != 15 || f.Score != scoreKeyAlike+scoreAmount+scoreDirection+scoreNextDay) {
			t.Fatalf("the probable match: %+v", f)
		}
	}
}

// Two records of one amount on one day are not a probable match unless their keys are alike.
func TestUnrelatedRecordsDoNotPair(t *testing.T) {
	o := Match(day(5),
		[]Record{rec(1, Ours, "E1111111120261005AAAAAAAAAAA", 100, 5)},
		[]Record{rec(2, Theirs, "E1111111120261005BBBBBBBBBBB", 100, 5)}, nil)
	if got := kinds(o); len(got[ProbableMatch]) != 0 || len(got[MissingAtCounterparty]) != 1 || len(got[MissingAtJupiter]) != 1 {
		t.Fatalf("unrelated records: %v", got)
	}
}

func TestScore(t *testing.T) {
	a, b := rec(1, Ours, "K", 100, 5), rec(2, Theirs, "K", 100, 5)
	if s, _ := Score(a, b); s != 100 {
		t.Fatalf("identical records score %d", s)
	}
	b.Direction = Out
	if s, reasons := Score(a, b); s != 90 || reasons[len(reasons)-2] != "opposite directions" {
		t.Fatalf("opposite directions: %d %v", s, reasons)
	}
}
