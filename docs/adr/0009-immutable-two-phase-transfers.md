# 0009. Two-phase transfers as immutable transactions, with invariants in the database

- Status: Accepted
- Date: 2026-09-30

## Context

ADR 0004 put the ledger in PostgreSQL with TigerBeetle's semantics: a transfer can be
created pending, then posted in full or in part, voided, or left to expire. Those are the
operations that card authorization, capture and void will map onto in phase 3.

TigerBeetle keeps pending amounts in mutable balance fields and marks a pending transfer
resolved in place. Jupiter's entries must be append-only, every balance must be
recomputable from them, and the database, not only Go code, must refuse a transaction
that breaks an invariant.

## Decision

**Everything is a transaction, and every transaction is immutable.** A transaction has a
kind: `posted` and `reversal` carry any number of legs; `pending` carries exactly two
(one debit, one credit); `post_pending`, `void_pending` and `expire_pending` resolve a
pending transaction by naming it.

**Entries live in two layers.** A pending transaction writes `pending`-layer entries (the
hold). Resolving it writes new `pending`-layer entries that release the whole hold and,
for a post, `posted`-layer entries for the amount posted, which may be less than the
hold. Nothing is updated, so each account's four balance fields (posted debits and
credits, pending debits and credits) are a function of its entries. One SQL function,
`ledger.entry_deltas`, defines that function for both the write path and the checker.

**Once is a unique index.** A pending transaction is resolved at most once and a
transaction reversed at most once, enforced by partial unique indexes on `resolves_id`
and `reverses_id`. A capture and a void racing for one hold cannot both commit.

**Invariants the database enforces:**

| Invariant | Mechanism |
|---|---|
| Entries, transactions and accounts never change | Triggers refuse `UPDATE`, `DELETE` and `TRUNCATE` |
| Each transaction sums to zero per book, currency and layer | Deferred constraint trigger, checked at commit |
| No entry is added to a committed transaction | The same trigger checks the entry count the transaction declared |
| An entry matches its account's book and currency | Composite foreign key to the account |
| A non-negative account never goes below zero, counting holds against it | `CHECK` on its balance row, updated in the same transaction |
| A drifted synchronous account accepts no entries | The balance trigger raises until the account is repaired; reads of any drifted account are refused |

Go checks the same rules first, to return typed errors such as `ErrInsufficientBalance`
and `ErrUnbalanced` rather than SQL errors.

**Writes take locks in one order.** Entries are inserted sorted by account, so concurrent
ledger writes lock balance rows in the same order and cannot deadlock one another. Within
an account, releases come before postings and increases before decreases, so the
row-level `CHECK` never sees a state the whole transaction would not end in.

**Two-phase transfers have two legs.** A multi-leg split is a separate posted transaction
through a clearing account in the same database transaction: capture posts the hold into
clearing, and a posted transaction splits clearing among the recipients. The clearing
account returns to zero, which the checker verifies.

## Alternatives rejected

- **Mutable pending state (TigerBeetle's model, or a status column on the transaction).**
  Simpler writes, but a balance would no longer be recomputable from entries alone, and
  "resolved once" would rely on a status update under a lock rather than on a constraint.
- **Multi-leg pending transfers.** A partial post of a hold split among several accounts
  has no single correct meaning; which legs shrink? Clearing accounts make the
  split explicit instead.
- **A per-row check of zero-sum** (a trigger on each entry insert, not deferred). It
  would fail on the first entry of every transaction; the check has to see the whole
  transaction, which only commit time guarantees.
- **Invariants in Go only.** See ADR 0004: one bug, one manual session, one future
  writer that bypasses the package, and they are gone.

## Consequences

- Reading a transaction's effect needs its kind as well as its entries; entries carry
  the kind, checked by a foreign key, so no join is needed.
- A reversal reverses only the posted layer, and a reversal cannot itself be reversed;
  a correction of a correction is a new posted transaction.
- Deadlock-freedom holds between ledger calls. A caller that makes several ledger calls
  in one database transaction can still lock accounts in two different orders, so
  `postgres.InTx` retries transactions PostgreSQL aborts to break a deadlock.
