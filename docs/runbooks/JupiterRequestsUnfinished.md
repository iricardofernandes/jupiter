# JupiterRequestsUnfinished

**Warning.** Metric: `jupiter_api_requests_unfinished`: requests that stopped between
their atomic phases and that the completer has not finished in 10 minutes.

## What it means

A payment request runs in phases, with a recovery point after each (ADR 0012). If the
process dies between phases, the worker's completer (`api.complete_abandoned`, every
minute) finishes the request from its recovery point, so the merchant's retry finds it
done.

## Impact

The merchant's retry of that request waits, or gets a 409, until it is finished. The
payment itself stays `processing`.

## Diagnose

```sql
SELECT key, merchant_id, operation, path_id, recovery_point, completer_runs, locked_at, last_run_at
FROM api.idempotency_keys WHERE response_status IS NULL ORDER BY last_run_at LIMIT 20;
```

- Is `api.complete_abandoned` running? Look for
  [JupiterTaskFailing](JupiterTaskFailing.md) with that task, and at its error.
- A request that fails again at the same recovery point every time points at a bug, or at
  a dependency the phase needs. The completer waits longer after each run, up to an
  hour, and after 30 runs (`completer_runs`), about a day, it leaves the request, which
  stays here until someone looks.

## Fix

Restore the completer's dependencies; it retries on its own. Once the cause is fixed, a
request it left after 30 runs is taken on again by setting its `completer_runs` back
to 0. Do not delete idempotency
keys by hand: a deleted key lets the merchant's retry run the request a second time.
