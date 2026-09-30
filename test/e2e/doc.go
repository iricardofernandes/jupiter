// Package e2e holds the golden path: one flow that runs end to end on every push and
// grows with each phase, from a card payment in six installments through
// receivables, anticipation, settlement, payout and three-way reconciliation.
//
// Its tests carry the e2e build tag and run with `make test-e2e`. The first segment,
// intent → authorize → capture → ledger, arrives with phase 3; see docs/plan.md.
package e2e
