package page

import (
	"errors"
	"fmt"
	"slices"
)

const (
	DefaultLimit = 10
	MaxLimit     = 100
)

var ErrInvalid = errors.New("page: invalid request")

// Request selects one page of a list ordered newest first. StartingAfter moves toward
// older objects, EndingBefore toward newer ones.
type Request struct {
	Limit         int
	StartingAfter string
	EndingBefore  string
}

func (r Request) Validate() error {
	if r.Limit < 1 || r.Limit > MaxLimit {
		return fmt.Errorf("%w: limit must be between 1 and %d", ErrInvalid, MaxLimit)
	}
	if r.StartingAfter != "" && r.EndingBefore != "" {
		return fmt.Errorf("%w: starting_after and ending_before cannot be combined", ErrInvalid)
	}
	return nil
}

// Fetch is how many rows to query: one more than the limit reveals whether there are
// more.
func (r Request) Fetch() int32 {
	return int32(r.Limit + 1) //nolint:gosec // Validate bounds Limit by MaxLimit
}

// Trim turns the rows of a Fetch-sized query into a page and whether more follow.
func Trim[T any](rows []T, r Request) ([]T, bool) {
	more := len(rows) > r.Limit
	if more {
		rows = rows[:r.Limit]
	}
	if r.EndingBefore != "" {
		rows = slices.Clone(rows)
		slices.Reverse(rows)
	}
	return rows, more
}
