# 0015. Correctness under faults is tested by deterministic simulation

- Status: Accepted
- Date: 2026-09-30

## Context

Unit and integration tests exercise the faults someone thought of, one at a time. The
bugs that lose money come from combinations: a response lost while a capture races a
cancellation, a request that dies between phases and is then retried twice. The research
found TigerBeetle's VOPR, which runs a cluster under injected faults "at 1000x speed" and
replays any failing seed, to be the strongest published practice
([research: engineering writeups](../research/notes/06-engineering-writeups.md)).

## Decision

`test/simulation` runs thousands of payments against the whole stack in one goroutine:

- **One source of decisions.** A seeded PCG generator chooses each payment's plan (card,
  amount, capture, cancellation, refunds, authentication), which payment advances next,
  when background work runs, and every fault.
- **Faults.** A rail wrapper loses requests, responses and queries; requests die between
  atomic phases (the phase hook ends their goroutine, leaving what a killed process
  would); finished requests are sent again with the same Idempotency-Key, and the answer
  must be byte-for-byte the same; now and then the clock jumps past the validity of
  every uncaptured authorization, so expiry races the other operations.
- **Background work** (resolver, completer, batched balances) runs when the seed says,
  on a virtual clock.
- **Invariants** run every 500 steps and at the end: the ledger's checks, the payments
  check (intents agree with attempts, merchant balances equal what payments say), no
  operation left unknown, and the rail and Jupiter agreeing in both directions: every
  authorization the rail approved is captured or still held, every one Jupiter voided the
  rail reversed, captures and refunds match to the centavo, and the rail refused nothing
  as breaking its rules.
- **Replay.** Running the same seed twice produces the same trace digest, checked by a
  test; a failure prints `SIM_SEED=… make test-simulation` to reproduce it.

CI runs 10,000 payments per push, about two minutes. The harness was checked by
injecting bugs: a refund posting one centavo less, and lost captures never retried, are
both caught. It also found a real one the first time expiry was added to it: a capture
resolved after the ledger's backstop had released its hold could not be posted.

## Alternatives rejected

- **Concurrent goroutines with random sleeps.** Closer to production timing, but a
  failure could not be replayed, and replay is what makes a found bug fixable.
- **Simulating only the payments module with fakes.** The idempotency layer, the ledger
  and the database constraints are where half the invariants live; faking them would test
  the fakes.
- **Model checking (TLA+).** Valuable for the protocol, but it checks a model rather than
  the code; the research found no payments-specific TLA+ work to build on.

## Consequences

- Determinism has limits: identifiers come from the wall clock, so the trace names them
  by role, and PostgreSQL's own scheduling is not simulated. A seed replays the same
  sequence of decisions and answers, not the same physical interleaving.
- Because it runs in one goroutine, the simulation interleaves operations but never runs
  two at the same instant; true races (capture against cancel, concurrent refunds,
  expiry against capture) are covered by separate concurrent integration tests. Crashes
  are injected between API phases, not yet inside the background loops.
- Every phase extends the harness to its rail (plan, phase 14), and new invariants join
  the checks.
- The phase hook in `api.Deps` and the rail port are the seams the harness needs; they
  are part of the production code so that what is tested is what runs.
