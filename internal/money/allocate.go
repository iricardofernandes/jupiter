package money

import (
	"errors"
	"fmt"
	"math/big"
	"slices"
)

// ErrInvalidWeights is returned when allocation weights are empty, negative or all zero.
var ErrInvalidWeights = errors.New("money: invalid allocation weights")

// Allocate splits the amount into one share per weight, in proportion to the weights,
// using the largest-remainder method: every share first receives the floor of its exact
// proportion, and the minor units left over go one each to the shares with the largest
// fractional remainders, earliest weight first on a tie. The shares always sum to the
// amount, a zero weight always receives zero, and a negative amount is split as its
// magnitude and negated, so its shares mirror those of the positive amount.
func (a Amount) Allocate(weights ...int64) ([]Amount, error) {
	if err := a.currency.validate(); err != nil {
		return nil, err
	}
	total, err := totalWeight(weights)
	if err != nil {
		return nil, err
	}

	// The arithmetic runs in big integers because amount × weight overflows int64.
	magnitude := new(big.Int).Abs(big.NewInt(a.minor))
	shares := make([]*big.Int, len(weights))
	remainders := make([]*big.Int, len(weights))
	allocated := new(big.Int)
	for i, w := range weights {
		shares[i], remainders[i] = new(big.Int).QuoRem(
			new(big.Int).Mul(magnitude, big.NewInt(w)), total, new(big.Int))
		allocated.Add(allocated, shares[i])
	}

	// The leftover is below len(weights) and never exceeds the number of non-zero
	// remainders, so a zero weight is never handed a unit.
	leftover := new(big.Int).Sub(magnitude, allocated).Int64()
	for _, i := range byLargestRemainder(remainders)[:leftover] {
		shares[i].Add(shares[i], big.NewInt(1))
	}

	out := make([]Amount, len(shares))
	for i, s := range shares {
		if a.minor < 0 {
			s.Neg(s)
		}
		// Every share lies between zero and the amount, so it fits in int64.
		out[i] = Amount{minor: s.Int64(), currency: a.currency}
	}
	return out, nil
}

func totalWeight(weights []int64) (*big.Int, error) {
	total := new(big.Int)
	for _, w := range weights {
		if w < 0 {
			return nil, fmt.Errorf("%w: negative weight %d", ErrInvalidWeights, w)
		}
		total.Add(total, big.NewInt(w))
	}
	if total.Sign() == 0 {
		return nil, fmt.Errorf("%w: no positive weight in %v", ErrInvalidWeights, weights)
	}
	return total, nil
}

// byLargestRemainder returns the indices of remainders ordered from the largest remainder
// to the smallest, with ties kept in index order.
func byLargestRemainder(remainders []*big.Int) []int {
	order := make([]int, len(remainders))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(i, j int) int {
		return remainders[j].Cmp(remainders[i])
	})
	return order
}
