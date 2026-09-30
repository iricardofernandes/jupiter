// Package store is private to the ledger module.
package store

// Balance is reached into from outside the module by the payments fixture.
func Balance() int64 { return 0 }
