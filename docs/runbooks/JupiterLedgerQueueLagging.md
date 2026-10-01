# JupiterLedgerQueueLagging

**Warning.** Metrics: `jupiter_ledger_queue_oldest` (seconds) and
`jupiter_ledger_queue_depth`.

## What it means

Hot accounts, such as the network receivable and merchants' balances, update their
cached balance in batches (ADR 0008): each posting queues a delta, and the worker's
`ledger.apply_queued` task folds the queue into the balance every 200 ms. The oldest delta
has now waited more than a minute.

## Impact

Balances stay exact: `Ledger.Balance` adds queued deltas when it reads. But each read
sums a longer queue, and the queue grows without bound if nothing drains it.

## Diagnose

- Is the worker running, and is `ledger.apply_queued` succeeding? Look for
  `JupiterTaskFailing` or `JupiterTaskStalled` with that task, and for `background task
  failed` with `task=ledger.apply_queued` in the worker's logs.
- How deep is the queue, and on which accounts?
  ```sql
  SELECT account_id, count(*) FROM ledger.balance_queue GROUP BY account_id ORDER BY 2 DESC LIMIT 10;
  ```
- A drifted account refuses reads and postings until it is repaired: see
  [JupiterLedgerBalanceDrift](JupiterLedgerBalanceDrift.md).

## Fix

- If the worker is down, start it; the queue drains on its own.
- If the applier fails on a database error, fix that first (connections, disk, locks).
- If the load is beyond what it drains, the queue grows steadily while the applier runs:
  see `docs/benchmarks/ledger-saturation.md` for the ledger's measured capacity.
