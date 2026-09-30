# 0004. PostgreSQL as the ledger store, not a dedicated ledger database

- Status: Accepted
- Date: 2026-09-30

## Context

The ledger is the source of truth for every money movement in Jupiter. Purpose-built
ledger databases exist and are good: TigerBeetle runs two-phase transfers with pending
and posted balances at very high throughput, and Formance and Blnk offer ledgers as
services ([research: open source](../research/notes/07-open-source-landscape.md)). The
published designs agree on what a ledger must guarantee: entries are append-only, a
transaction's entries sum to zero, pending and posted balances are separate, and
corrections are new transactions
([research: engineering writeups](../research/notes/06-engineering-writeups.md)).

A payment's ledger postings also have to commit together with the payment's own state
change (ADR 0001).

## Decision

The ledger lives in Jupiter's PostgreSQL database, as tables Jupiter owns: accounts,
transactions and entries. It adopts TigerBeetle's semantics (two-phase transfers that are
posted, voided or expire, and four balance fields per account) and enforces its
invariants **in the database**: entries cannot be updated or deleted, and a transaction
whose entries do not sum to zero per currency cannot commit. Go code checks them too, but
the database is the last line.

Phase 1 designs the schema, the hot-account strategy and the invariant checker.

## Alternatives rejected

- **TigerBeetle as the ledger.** The strongest option on throughput and on correctness
  testing. But a payment's intent, split and receivables live in PostgreSQL, and TigerBeetle
  cannot join a PostgreSQL transaction, so every payment would need a two-system
  consistency protocol. And the design of a ledger on a relational database is itself what
  this project sets out to show.
- **Formance or Blnk as a service.** The same split-transaction problem, plus a network
  hop on every posting.
- **Invariants in application code only.** One bug, one manual `psql` session or one
  future service that writes directly would be enough to break them silently.

## Consequences

- Payment state, ledger postings and enqueued jobs commit atomically.
- Throughput is bounded by PostgreSQL row locks on hot accounts. Phase 1 measures it and
  documents the strategy (synchronous locks on user-side accounts, batched balance updates
  on platform accounts) with numbers in `docs/benchmarks/`.
- A future move to TigerBeetle stays possible: the ledger module's public interface speaks
  in transfers, not tables.
