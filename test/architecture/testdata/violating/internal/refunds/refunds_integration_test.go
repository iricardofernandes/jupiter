//go:build integration

package refunds

import (
	"testing"

	"example.com/violating/internal/ledger/store"
)

func TestBalance(t *testing.T) { _ = store.Balance() }
