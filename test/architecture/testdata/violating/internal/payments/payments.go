// Package payments reaches past the ledger module's public interface: the forbidden
// import the architecture test must catch.
package payments

import "example.com/violating/internal/ledger/store"

// Balance returns the ledger's balance through its private package.
func Balance() int64 { return store.Balance() }
