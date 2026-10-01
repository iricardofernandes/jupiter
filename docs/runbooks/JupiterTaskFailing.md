# JupiterTaskFailing

**Warning.** Metric: `jupiter_task_runs_total{outcome="error"}`, by task: every run of a
background task in the last 15 minutes failed.

## What it means

One of the worker's loops fails each time it runs. The alert names the task. A failure is
logged and retried on the next tick, so a passing problem clears by itself. One that
lasts does not.

## Impact

Depends on the task. The worst cases:

| Task | What stops |
|---|---|
| `payments.resolve`, `payments.resolve_payouts` | Unknown outcomes stay unknown |
| `ledger.apply_queued`, `ledger.expire_due` | Batched balances lag; expired holds stay |
| `ledger.check` | Invariants go unchecked |
| `api.complete_abandoned` | Interrupted requests stay unfinished |
| `acquirer.retry_forwards`, `acquirer.import_clearing` | Reversals and clearing stop |
| `pix.reconcile`, `payments.return_unmatched_pix` | Pix received are not applied or returned |
| `bank.collection` | Boleto remittances and returns stop |
| `receivables.advance` | Receivables are not registered or settled |
| `disputes.*` | Disputes do not move |
| `reconciliation.run` | Breaks are not found |

## Diagnose

The worker logs every failure as `background task failed` with `task` and `error`. Most
failures are a counterparty that is down (the network, a bank, the registry, the SLC),
the database, or the vault.

## Fix

Restore the dependency. The task needs nothing else: it works from the database, and
the next run picks up everything that waited.
