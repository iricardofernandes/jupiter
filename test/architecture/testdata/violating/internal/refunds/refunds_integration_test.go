//go:build integration

package refunds

import (
	"testing"

	"example.com/violating/internal/ledger/store"
)

// A violation hidden behind the integration build tag must still be caught.
func TestBalance(t *testing.T) { _ = store.Balance() }
