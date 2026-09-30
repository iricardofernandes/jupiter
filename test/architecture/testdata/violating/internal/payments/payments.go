package payments

import "example.com/violating/internal/ledger/store"

func Balance() int64 { return store.Balance() }
