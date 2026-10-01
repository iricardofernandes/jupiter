# JupiterPaymentLatencyHigh

**Warning.** Metric: `jupiter_api_request_duration_seconds_bucket` for `POST
/v1/payment_intents` and `POST /v1/payment_intents/{id}/confirm`: the p99 is above one
second for 10 minutes.

## What it means

Creating or confirming a payment is slow. In the measured setup
([docs/benchmarks/authorization-path.md](../benchmarks/authorization-path.md)) the p99
stays under 20 ms below saturation. Above it, latency grows with the queue while
throughput stays flat.

## Impact

Customers wait at checkout. Merchants' clients may time out and retry, which adds load.

## Diagnose

- Is the database the limit? Look at
  [JupiterDatabasePoolSaturated](JupiterDatabasePoolSaturated.md), and at
  `pg_stat_activity` for what sessions wait on:
  ```sql
  SELECT wait_event_type, wait_event, count(*) FROM pg_stat_activity WHERE state = 'active' GROUP BY 1, 2 ORDER BY 3 DESC;
  ```
  - `Lock:advisory` on risk decisions means many attempts on one card or address.
  - `Lock:tuple` or `transactionid` on `ledger.entries` means a hot account that is not
    batched.
  - `LWLock:WALWrite` means commit throughput: the disk.
- Is a rail slow? Live payments wait for the card network's answer, up to the acquirer's
  timeout. Compare with the network's own latency.
- Is traffic higher than usual? Compare the request rate with the saturation point in
  the benchmark.

## Fix

Relieve the bottleneck found. For load beyond the measured capacity, the benchmark says
what limits it.
