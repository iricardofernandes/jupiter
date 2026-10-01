# JupiterTaskStalled

**Warning.** Metrics: `jupiter_task_last_success` and `jupiter_task_interval`: a task's last
success is more than three intervals, and ten minutes, old.

## What it means

The task has not succeeded for a while; until its first success, it counts from when the
worker started. It may fail every time, which
[JupiterTaskFailing](JupiterTaskFailing.md) also shows. Or it may never finish: one run is
stuck, waiting on a lock, a counterparty that does not answer, or a long query.

## Impact

As for [JupiterTaskFailing](JupiterTaskFailing.md), for the task named.

## Diagnose

- How long do its runs take? `jupiter_task_duration_seconds` for the task.
- Is it waiting in the database?
  ```sql
  SELECT pid, state, wait_event_type, wait_event, now() - query_start AS running, left(query, 120)
  FROM pg_stat_activity WHERE datname = current_database() AND state <> 'idle' ORDER BY query_start;
  ```
- Calls to counterparties have timeouts. A run that hangs anyway is a bug worth a
  goroutine dump (`SIGQUIT` to the worker writes one to its logs).

## Fix

Clear what blocks it. A restart of the worker ends a stuck run, and the next one starts
over from the database.
