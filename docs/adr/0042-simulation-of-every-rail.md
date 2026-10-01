# 0042. Every rail simulated, on a fixed seed budget per push and random seeds nightly

- Status: Accepted. Extends [ADR 0015](0015-deterministic-simulation.md).
- Date: 2026-10-01

## Context

The deterministic simulation (ADR 0015) covered cards, test and live over ISO 8583. Since
then, phases 7 to 13 added more rails, each tested alone by integration tests:
- Pix, with MED;
- boleto;
- receivables, the registry and the SLC;
- payouts;
- disputes;
- reconciliation.

The plan asks for the harness on every rail, with four kinds of fault:
- network faults;
- process crashes;
- clock skew;
- duplicated messages.

It asks for it in CI on every push with a fixed seed budget, and nightly with random
seeds.

## Decision

**A second simulation, `TestRailsSimulation`**, runs scenarios on every rail in test mode,
where each rail is a simulator. A scenario is one of:
- a card payment in installments, its receivables registered and settled through the SLC
  into Jupiter's bank account, sometimes refunded or charged back;
- a Pix charge, paid or left to expire, sometimes refunded or claimed under MED;
- a boleto, paid at the bank or left unpaid;
- a payout by Pix;
- a Pix that pays nothing and must go back.

Each one is advanced a request at a time, as the card scripts are. Between steps the
worker's background loops run, then the banks' and the SLC's days.

**Faults:**
- **Network faults**:
  - the card rail loses requests and answers;
  - the Pix bank loses notifications, leaves transfers and returns pending, and answers
    some only after acting.
- **Clock skew**: every simulator runs on its own clock, up to two minutes from Jupiter's.
- **Duplicated messages**:
  - statement lines at the bank and the Pix bank are written twice, delayed or lost;
  - the bank's return records are written twice or delayed;
  - requests are sent again.
- **Process crashes**:
  - requests die between phases;
  - a background run is cancelled at its k-th database statement, after whatever it had
    already told a rail.

**Faults are drawn by order, not by identifier.** Simulators make identifiers at random
(end-to-end ids, payout ids), so a fault keyed by one could not be replayed. Instead the
n-th event of each kind at each rail is decided by the seed and n. The Pix simulator now
settles returns one at a time, in the order asked, so that order is the run's own. The
same seed replays the same trace, and a test checks it.

**Every run ends drained and reconciled.** The faults stop, and the background runs day
after day until nothing is open:
- no unknown outcome;
- no unfinished request;
- no unsettled unit;
- no undecided dispute.

Then every module's invariants must hold. Reconciliation's open breaks must be exactly
those the faults injected: each lost or duplicated statement line, and each duplicated
payment record. No break may be missing, and none may be extra.

**Seeds:**
- `SIM_SEEDS` takes a list, a range or `random:N`.
- CI runs fixed ranges on every push: cards seeds 1–2 with 10,000 payments each, every
  rail seeds 1–4 with 500 scenarios each.
- The nightly workflow runs 16 random seeds of each, larger, and keeps the logs.
- A seed that finds a bug goes into `test/simulation/testdata/regressions` with the fix,
  and runs on every push from then on.

## Alternatives rejected

- **Extending the card simulation with every rail.** Its one-thread schedule and its
  invariants are about authorization races. Rails run on days and files, and drain over
  months of settlement. Two simulations sharing helpers stay readable.
- **Seeded identifiers.** Making Jupiter's and the simulators' identifiers deterministic
  would touch production code for a test's sake. Ordering faults gives the same replay.
- **Random seeds on every push.** A push that fails on a seed nobody chose cannot be
  told apart from a flaky test. Fixed seeds keep CI reproducible; nightly random seeds
  explore.

## Consequences

- The `make` targets now fail when a simulation fails. Before, `go test … | grep` hid
  the test's exit status; the Makefile now runs recipes with `pipefail`.
- The background runs the worker's loops once per step of 1 to 30 minutes, but polls
  the banks for MED claims every 5, as the worker does: a claim must be held within 30
  minutes of the bank's notice, even when the notice is lost.
- The first random seeds found a bug: the test rail answered a query about an approved
  authorization without its network transaction id, so a card resolved after a lost
  answer had none. A chargeback then looked up the empty id and found another payment,
  a boleto. Both are fixed: the query answers with the id, and an empty id finds
  nothing. The seeds are regressions now.
- The exit criterion, a week of nightly runs without a violation, needs the workflow to
  run for a week once pushed. Until then, local runs over random seeds stand in for it.
