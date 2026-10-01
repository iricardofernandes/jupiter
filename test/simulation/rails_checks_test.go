//go:build simulation

package simulation_test

import (
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/iricardofernandes/jupiter/internal/ledger"
	"github.com/iricardofernandes/jupiter/internal/platform/page"
)

// check verifies every module's invariants and, once drained, that nothing is left open
// and that reconciliation found exactly the breaks the faults made.
func (r *rails) check(final bool) {
	ctx := r.t.Context()
	if _, err := r.ledger.ApplyQueued(ctx, r.pool, 100_000); err != nil {
		r.t.Fatal(err)
	}
	report, err := r.ledger.Check(ctx, r.pool, ledger.CheckOptions{ClearingGrace: 1000 * time.Hour, ExpiryGrace: 1000 * time.Hour})
	if err != nil {
		r.t.Fatal(err)
	}
	for _, v := range report.Violations {
		r.t.Errorf("seed %d: ledger: %+v", r.seed, v)
	}
	violations, err := r.payments.Check(ctx, r.pool)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, v := range violations {
		r.t.Errorf("seed %d: payments: %+v", r.seed, v)
	}
	found, err := r.receiv.Check(ctx, r.pool)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, v := range found {
		r.t.Errorf("seed %d: receivables: %+v", r.seed, v)
	}
	stuck, err := r.disputes.Check(ctx, r.pool)
	if err != nil {
		r.t.Fatal(err)
	}
	for _, v := range stuck {
		r.t.Errorf("seed %d: disputes: %+v", r.seed, v)
	}
	if final {
		r.checkCoverage()
		if !r.settled() {
			r.t.Errorf("seed %d: work still open after draining", r.seed)
		}
		r.checkBreaks()
	}
	if r.t.Failed() {
		r.t.FailNow()
	}
}

// checkBreaks compares the open reconciliation breaks with those the faults injected:
// every injected break found, and no other.
func (r *rails) checkBreaks() {
	got := map[string]int{}
	var after string
	for {
		breaks, more, err := r.recon.Breaks(r.t.Context(), r.pool, false, "", "open", page.Request{Limit: 100, StartingAfter: after})
		if err != nil {
			r.t.Fatal(err)
		}
		for _, b := range breaks {
			got[strings.Join([]string{b.Counterparty, b.Stream, b.Kind, b.Key}, " ")]++
			after = b.ID.String()
		}
		if !more {
			break
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if got[k] != r.injected[k] {
			r.t.Errorf("seed %d: %d open breaks %q, %d injected", r.seed, got[k], k, r.injected[k])
		}
	}
	for _, k := range slices.Sorted(maps.Keys(r.injected)) {
		if got[k] == 0 {
			r.t.Errorf("seed %d: the injected break %q was not found", r.seed, k)
		}
	}
	r.t.Logf("reconciliation: open breaks %v; injected %v", got, r.injected)
	r.record("reconciliation: %d breaks open, all injected", len(got))
}

// coverageAt is the size from which a run must have exercised everything: every kind of
// scenario, faults on the rails, crashes, and breaks for reconciliation to find.
const coverageAt = 200

// checkCoverage fails a run large enough to have exercised the faults that did not,
// so the checks cannot pass for want of anything to check.
func (r *rails) checkCoverage() {
	if len(r.scenarios) < coverageAt {
		return
	}
	for _, kind := range []string{kindCard, kindPix, kindBoleto, kindPayout, kindStray} {
		if r.stats.kinds[kind] == 0 {
			r.t.Errorf("seed %d: no %s scenario ran", r.seed, kind)
		}
	}
	if r.stats.faults == 0 || r.stats.crashes+r.stats.backgroundCrashes == 0 || r.stats.duplicates == 0 || len(r.injected) == 0 {
		r.t.Errorf("seed %d: the run injected too little: %s; %d breaks", r.seed, r.stats, len(r.injected))
	}
}
