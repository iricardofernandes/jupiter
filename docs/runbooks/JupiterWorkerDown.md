# JupiterWorkerDown

**Critical.** Metric: `jupiter_task_interval{service_name="worker"}` absent for 5 minutes.

## What it means

The worker reports its tasks every 15 seconds while it runs. Nothing has arrived for five
minutes: it is down, cannot reach the Collector, or was started without
`OTEL_EXPORTER_OTLP_ENDPOINT`.

## Impact

Nothing in the background runs:
- no unknown outcome is resolved;
- no hold expires;
- no webhook is delivered;
- no clearing file is imported;
- no Pix is returned;
- no payout is sent;
- no reconciliation runs.

Payments still go through the API, but everything after them waits.

## Diagnose

- Is the process up? Is it restarting? Look at its logs.
  - A task that returns an error is logged and retried.
  - A failure to start, such as a missing setting, the database or the vault being down,
    or a migration not applied, stops the process.
- Is the Collector up? Are the other services' metrics arriving?

## Fix

Start the worker. Whatever waited is picked up on its first runs: every task works from
the database, so nothing is lost while it is down.
