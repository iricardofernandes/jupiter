# Architecture decision records

Each record states one decision, the context that forced it, the alternatives that were
rejected and why, and what the decision costs. A record is never edited to change its
decision: a later record supersedes it and says so, and the old one's status changes to
point at its replacement.

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-modular-monolith-with-isolated-vault.md) | A modular monolith, with the card vault as its one separate service | Accepted |
| [0002](0002-go-and-the-library-set.md) | Go, and the libraries Jupiter builds on | Accepted |
| [0003](0003-money-as-integer-minor-units.md) | Money as `int64` minor units with an explicit currency | Accepted |
| [0004](0004-postgresql-as-the-ledger-store.md) | PostgreSQL as the ledger store, not a dedicated ledger database | Accepted |
| [0005](0005-simulators-as-separate-binaries.md) | Every counterparty is a simulator in its own binary | Accepted |
| [0006](0006-regulatory-rules-as-dated-configuration.md) | Rules regulation can change are configuration with effective dates | Accepted |
| [0007](0007-prefixed-time-ordered-identifiers.md) | Identifiers are a type prefix and a UUIDv7 | Accepted |
| [0008](0008-hot-accounts-with-batched-balances.md) | Hot accounts keep synchronous entries and batched balances | Accepted |
| [0009](0009-immutable-two-phase-transfers.md) | Two-phase transfers as immutable transactions, with invariants in the database | Accepted |
| [0010](0010-per-module-migrations.md) | Each module migrates its own schema with goose | Accepted |
| [0011](0011-api-conventions.md) | API conventions: keys and modes, versions, errors, lists | Accepted |
| [0012](0012-idempotency-keys-with-recovery-points.md) | Idempotency keys with atomic phases and recovery points | Accepted |
| [0013](0013-thin-events-and-webhook-delivery.md) | Thin events written with the change, delivered at least once by River | Accepted |
| [0014](0014-payment-intents-attempts-and-unknown-outcomes.md) | Payment intents, attempts, and unknown outcomes as a state | Accepted |
| [0015](0015-deterministic-simulation.md) | Correctness under faults is tested by deterministic simulation | Accepted |

## Template

```markdown
# NNNN. Title in the imperative or as a noun phrase

- Status: Proposed | Accepted | Superseded by NNNN
- Date: YYYY-MM-DD

## Context
What forces the decision, with links to the research where a finding drives it.

## Decision
What Jupiter does.

## Alternatives rejected
Each option that was seriously considered, and the reason it lost.

## Consequences
What becomes easier, what becomes harder, and what must now be kept true.
```
