# JupiterClearingExceptions

**Warning.** Metric: `jupiter_acquirer_clearing_exceptions`: records in the network's
clearing files that have matched no capture or refund for a day.

## What it means

Each day `acquirer.import_clearing` reads the network's clearing file, and marks what it
lists as cleared. A record that matches nothing is kept as an exception and tried again.
A record whose capture was still waiting for the network's acknowledgement matches once
that arrives. One still open after a day did not.

## Impact

The network says it moved money that Jupiter cannot place: a capture Jupiter does not
know, a wrong amount, or a refund it never sent. Reconciliation shows the same record as
a break.

## Diagnose

```sql
SELECT business_date, kind, rrn, amount, merchant_code, reason FROM acquirer.clearing_exceptions
WHERE resolved_at IS NULL ORDER BY business_date;
```

The `reason` says what did not match. Find the RRN in `acquirer.exchanges`, then follow it
to the attempt or refund. The reconciliation report for that day shows the break too:
```sh
jupiterctl reconcile report <business date> live
```

## Fix

- A capture or refund that Jupiter is missing, or one with a different amount, is a
  dispute with the network. Raise it with the network.
- A bug in matching needs a fix and a test. Once fixed, the exception resolves on the
  next import.
