package page_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

func TestValidate(t *testing.T) {
	valid := []page.Request{{Limit: 1}, {Limit: 100}, {Limit: 10, StartingAfter: "x"}, {Limit: 10, EndingBefore: "y"}}
	for _, r := range valid {
		if err := r.Validate(); err != nil {
			t.Errorf("%+v: %v", r, err)
		}
	}
	invalid := []page.Request{{Limit: 0}, {Limit: 101}, {Limit: -1}, {Limit: 10, StartingAfter: "x", EndingBefore: "y"}}
	for _, r := range invalid {
		if err := r.Validate(); !errors.Is(err, page.ErrInvalid) {
			t.Errorf("%+v: error = %v, want ErrInvalid", r, err)
		}
	}
}

func TestTrimForward(t *testing.T) {
	r := page.Request{Limit: 2}
	items, more := page.Trim([]int{9, 8, 7}, r)
	if !slices.Equal(items, []int{9, 8}) || !more {
		t.Fatalf("Trim = %v, %t", items, more)
	}
	items, more = page.Trim([]int{9, 8}, r)
	if !slices.Equal(items, []int{9, 8}) || more {
		t.Fatalf("Trim = %v, %t", items, more)
	}
}

// With ending_before the query walks oldest first; Trim keeps the rows nearest the
// cursor and restores newest-first order.
func TestTrimBackward(t *testing.T) {
	items, more := page.Trim([]int{4, 5, 6}, page.Request{Limit: 2, EndingBefore: "3"})
	if !slices.Equal(items, []int{5, 4}) || !more {
		t.Fatalf("Trim = %v, %t", items, more)
	}
}
