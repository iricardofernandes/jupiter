# JupiterReconciliationBehind

**Warning.** Metric: `jupiter_reconciliation_days_behind`: days between yesterday and the
last day live mode was reconciled through.

## What it means

The worker's `reconciliation.run` task reconciles through yesterday every hour. A run is
recorded only when every stream was read, so a counterparty that cannot be read holds
the run back, and the next run reads those days again. A mode far behind catches up a
month at a time.

## Impact

Breaks are not being found, so missing or extra money goes unseen.

## Diagnose

- The worker logs which stream failed: `background task failed` with
  `task=reconciliation.run`, naming the counterparty, the stream and the day.
- Which day did the last run reach?
  ```sql
  SELECT livemode, max(day), max(ran_at) FROM reconciliation.runs GROUP BY livemode;
  ```

## Fix

- Restore the counterparty: the bank's statement endpoint, the Pix bank's, the SLC, or
  the network's clearing import.
- A record that cannot be matched, such as one with no key or no amount, does not hold
  the run back: it is reported in the logs and skipped.
- Once the counterparty answers, the next run catches up.
