package payments

import (
	"errors"
	"testing"
)

// Every pair of statuses is either an allowed transition, listed here, or rejected.
func TestEveryTransition(t *testing.T) {
	allowed := map[[2]Status]bool{
		{RequiresPaymentMethod, RequiresConfirmation}: true,
		{RequiresPaymentMethod, Processing}:           true,
		{RequiresPaymentMethod, Canceled}:             true,
		{RequiresConfirmation, RequiresPaymentMethod}: true,
		{RequiresConfirmation, Processing}:            true,
		{RequiresConfirmation, Canceled}:              true,
		{Processing, RequiresPaymentMethod}:           true,
		{Processing, RequiresAction}:                  true,
		{Processing, RequiresCapture}:                 true,
		{Processing, Succeeded}:                       true,
		{Processing, Canceled}:                        true,
		{RequiresAction, Processing}:                  true,
		{RequiresAction, RequiresPaymentMethod}:       true,
		{RequiresAction, Canceled}:                    true,
		{RequiresCapture, Processing}:                 true,
	}
	for _, from := range Statuses {
		for _, to := range Statuses {
			want := allowed[[2]Status{from, to}]
			if got := CanTransition(from, to); got != want {
				t.Errorf("%s → %s: allowed = %t, want %t", from, to, got, want)
			}
			if err := checkTransition(from, to); (err == nil) != want || (err != nil && !errors.Is(err, ErrInvalidState)) {
				t.Errorf("%s → %s: checkTransition = %v", from, to, err)
			}
		}
	}
}

func TestTerminalStatusesHaveNoWayOut(t *testing.T) {
	for _, s := range []Status{Succeeded, Canceled} {
		if len(transitions[s]) != 0 {
			t.Errorf("%s is terminal but allows %v", s, transitions[s])
		}
	}
}
