# 0022. A rules-based risk engine, with velocity in PostgreSQL and a decision log

- Status: Accepted. Amended by [ADR 0040](0040-measured-capacity-of-the-authorization-path.md): decisions lock per card, and per merchant only under watch.
- Date: 2026-09-30

## Context

A PSP decides on every payment before it reaches a rail: let it through, look at it
later, ask the cardholder to authenticate, or refuse it. The production pattern the
research found is a machine-learning score with merchant-editable rules on top
([research: card rails, risk](../research/notes/04-card-rails.md)). Jupiter has no fraud
labels to train on, and the plan rules a model out: trained on synthetic data it would
prove nothing. Card testing, fraudsters trying stolen numbers with small amounts, is now
a scheme-monitored metric (Visa's enumeration ratio under VAMP). Detecting it is a
compliance matter as well as a loss one.

## Decision

`internal/risk` decides on each attempt inside the transaction that records it, before
any rail call.

**Features.** Amount, currency, brand, BIN, installments, whether it is merchant-initiated,
the customer's IP when the merchant sends it (`customer_ip`), and velocity counters
over sliding windows:
- attempts per card in the last hour and day, across merchants, keyed by the vault's
  fingerprint;
- declines per card;
- attempts and distinct cards per address, within the merchant, since the address is the
  merchant's word;
- the merchant's attempts in the last minute and ten minutes, and its decline ratio.

The counters are queries over a `risk.attempts` table with an index per key. At
Jupiter's scale PostgreSQL answers them in the same transaction, which a separate store
such as Redis could not.

**Lists and rules.** A merchant's allow and block lists (card fingerprint, IP, BIN) are
checked first; an allow entry settles the decision. Then the platform's rules
(`internal/risk/platform_rules.json`) and the merchant's own run, written in expr-lang's
expression language, e.g. `amount >= 500000 && brand == "elo"`. A rule is compiled
against the features when it is created, so one that names a missing feature or is not
a boolean is refused. Rules run in the payment's transaction, so their cost must follow
from their length: functions, closures and ranges are refused, an expression has at most
200 terms, and it cannot have side effects. A rule that fails when it runs (a division by
zero) is skipped and logged, so one bad rule cannot stop a merchant's payments. The decision is the most
severe action of the rules that fired: `block` > `request_3ds` > `review` > `allow`.

**Card testing.** When at least 20 of a merchant's attempts in ten minutes show a
decline ratio of 20% or more, the merchant is throttled for 30 minutes. Decisions for one
merchant take a transaction-scoped advisory lock, so concurrent attempts all see each
other in the counts. While throttled,
attempts beyond three a minute are blocked by a rule whose explanation carries the
numbers.

**The decision log.** Every decision is stored with the rules that fired, their
explanations and the features they saw (`GET /v1/risk/decisions`). How the attempt ended
is recorded against it, so the log is also the labelled data a model could later be
trained on.

The exit criterion is a test: the load generator (`pkg/loadgen`) runs a card-testing
burst of 200 attempts at one merchant while another pays normally. The burst is
throttled: 180 blocked, none succeeding. The other merchant is untouched. Every block
in the log names a rule and explains it (`test/risk`).

## Alternatives rejected

- **A model trained on synthetic data.** It would learn the generator.
- **Redis for velocity.** Another moving part, and counters that cannot be read in the
  transaction that records the attempt; PostgreSQL is fast enough here.
- **CEL (cel-go) for rules.** Equally safe; expr-lang's syntax is closer to what a merchant
  would write, and its type-checking against a Go struct keeps rules and features in step.
- **Blocking by merchant rate alone.** Busy merchants would be blocked for being busy; the
  decline ratio is what separates testing from trade.

## Consequences

- Blocked attempts never reach a rail; the simulation checks it.
- `request_3ds` needs 3-D Secure (ADR 0023). In test mode it asks for the test helper.
- The thresholds are code defaults; per-merchant tuning, and device fingerprints, wait for
  a phase that needs them.
