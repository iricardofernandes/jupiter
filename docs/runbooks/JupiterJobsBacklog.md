# JupiterJobsBacklog

**Warning.** Metric: `jupiter_jobs_waiting_oldest`: the oldest job due to run has waited
more than five minutes.

## What it means

Jobs are queued in PostgreSQL (River) and run by the worker. Today they are webhook
deliveries: one per event per subscribed endpoint (ADR 0013). They are not being picked up
fast enough, or not at all.

## Impact

Merchants hear about payments, refunds and disputes late. Their systems may act on
stale state.

## Diagnose

```sql
SELECT kind, state, count(*), min(scheduled_at) FROM river.river_job
WHERE state IN ('available', 'running', 'retryable') GROUP BY 1, 2 ORDER BY 1, 2;
```

- Is the worker up ([JupiterWorkerDown](JupiterWorkerDown.md))?
- Are deliveries slow because an endpoint is slow? Each delivery waits for the merchant's
  endpoint up to its timeout. Many slow endpoints can occupy every worker slot.

## Fix

Start the worker. If endpoints are slow and the database has room, raise the worker's
job concurrency (20 at a time, `jobs.WorkerConfig.Concurrency`). A slow or dead merchant endpoint is the merchant's to fix: Jupiter retries with
backoff and then gives up ([JupiterJobsDiscarded](JupiterJobsDiscarded.md)).
