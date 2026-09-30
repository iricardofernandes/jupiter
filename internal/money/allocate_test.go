package money_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/money"
)

func minors(amounts []money.Amount) []int64 {
	out := make([]int64, len(amounts))
	for i, a := range amounts {
		out[i] = a.Minor()
	}
	return out
}

func TestAllocate(t *testing.T) {
	tests := []struct {
		name    string
		minor   int64
		weights []int64
		want    []int64
	}{
		{"even split", 60000, []int64{1, 1, 1, 1, 1, 1}, []int64{10000, 10000, 10000, 10000, 10000, 10000}},
		{"remainder goes to the earliest ties", 100, []int64{1, 1, 1}, []int64{34, 33, 33}},
		{"remainder goes to the largest fraction", 100, []int64{1, 2}, []int64{33, 67}},
		{"marketplace commission 12.5%", 10001, []int64{125, 875}, []int64{1250, 8751}},
		{"zero weight gets nothing", 7, []int64{0, 1, 1}, []int64{0, 4, 3}},
		{"negative amounts mirror positive ones", -100, []int64{1, 1, 1}, []int64{-34, -33, -33}},
		{"zero amount", 0, []int64{3, 7}, []int64{0, 0}},
		{"single share", 12345, []int64{9}, []int64{12345}},
		{"no overflow at the extremes", -1 << 63, []int64{1, 1}, []int64{-1 << 62, -1 << 62}},
		{"huge weights", 1<<63 - 1, []int64{1<<63 - 1, 1<<63 - 1}, []int64{1 << 62, 1<<62 - 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shares, err := mustNew(t, tt.minor, money.BRL).Allocate(tt.weights...)
			if err != nil {
				t.Fatalf("Allocate: %v", err)
			}
			if got := minors(shares); !slices.Equal(got, tt.want) {
				t.Fatalf("Allocate(%d, %v) = %v, want %v", tt.minor, tt.weights, got, tt.want)
			}
			for _, s := range shares {
				if s.Currency() != money.BRL {
					t.Fatalf("share %v changed currency", s)
				}
			}
		})
	}
}

func TestAllocateRejectsInvalidWeights(t *testing.T) {
	a := mustNew(t, 100, money.BRL)
	for _, weights := range [][]int64{nil, {}, {0, 0}, {1, -1}} {
		if _, err := a.Allocate(weights...); !errors.Is(err, money.ErrInvalidWeights) {
			t.Errorf("Allocate(%v) error = %v, want ErrInvalidWeights", weights, err)
		}
	}
}
