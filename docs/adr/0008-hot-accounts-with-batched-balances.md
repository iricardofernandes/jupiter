# 0008. Hot accounts keep synchronous entries and batched balances

- Status: Accepted
- Date: 2026-09-30

## Context

Every payment touches a few accounts that every other payment touches too: the platform's
fee revenue, card clearing, settlement. If each posting updates the cached balance row
of such an account, every writer in the system queues on one row lock, and throughput is
capped by how fast PostgreSQL can commit one transaction after another.

The published writeups name the trap and three ways out
([research: engineering writeups](../research/notes/06-engineering-writeups.md)):
optimistic version checks starve ("some Transactions will never be able to commit",
Modern Treasury); Modern Treasury locks only the user-side accounts and posts to the hot
one asynchronously; Uber batches writes to the hot entity; TigerBeetle serializes
everything on one core in large batches. Sharded sub-balances appear in folklore but in
no source.

What cannot be given up: entries are the source of truth and must be written in the
same transaction as the business change, and a non-negative account must be checked
against its exact balance when it is written.

## Decision

Each account is created either synchronous or **batched**.

- Entries to every account are written synchronously, in the posting's transaction, and
  the zero-sum check covers them all. Nothing about the ledger's correctness is deferred.
- A synchronous account's cached balance is updated by a trigger in the same transaction,
  under its row lock, and its non-negative constraint is a `CHECK` on that row.
- A batched account's trigger inserts the entry's balance deltas into
  `ledger.balance_queue` instead of touching the balance row. The worker's applier
  deletes queued rows and adds their sums to the cached balances in one statement, in
  entry order, under an advisory lock so that only one applier runs.
- `Ledger.Balance` of a batched account adds the queued deltas at read time, so reads are
  exact; only the stored cache lags.
- A batched account cannot be non-negative (the database refuses the combination),
  because its balance is not known when an entry is written. Hot accounts are platform
  accounts, which have no such limit; customer and merchant balances stay synchronous.

The invariant checker compares cached plus queued deltas against a recomputation from
entries, so the batched path is proven the same way as the synchronous one.

## Alternatives, as measured

Measured on 16,000 concurrent postings from 64 writers into one hot account
([benchmark](../benchmarks/ledger-hot-account.md)):

| Hot account | Postings/s | p99 latency |
|---|---|---|
| Synchronous (row lock per posting) | ≈3,600 | ≈90 ms |
| Batched (queue + applier) | ≈16,200 | ≈9 ms |

- **Synchronous everywhere.** Simplest and exact, but measured at a quarter of the
  throughput and ten times the tail latency on the same hardware, and the gap grows with
  commit latency, which a real deployment's replicated storage would raise.
- **Optimistic version checks with retry.** Not measured: the literature reports
  starvation under exactly this contention, and it would make a posting's latency
  unbounded rather than merely high.
- **Sharded sub-balances** (N rows per hot account, summed on read). Removes the single
  lock but multiplies read cost and complicates the non-negative check; no source
  reports it working, and batching already removes the contention.
- **An asynchronous entry writer** (write entries later, not just balances). Breaks the
  rule that entries commit with the business change.

## Consequences

- A batched account's stored cache lags by up to one applier interval; anything reading
  the table directly must add the queue.
- The applier must run: if it stops, the queue grows and reads get slower, and the
  checker still passes because it includes queued deltas. The queue's length is therefore
  a metric worth alerting on once metrics exist.
- Deltas are applied in entry order by one applier at a time, because a hold's release
  applied before the hold itself would take a cached pending balance below zero.
