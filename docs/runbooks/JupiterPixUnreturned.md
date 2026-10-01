# JupiterPixUnreturned

**Warning.** Metric: `jupiter_pix_unreturned`: Pix received that paid no charge
(`unmatched` or `returning`) for more than an hour.

## What it means

A payer sent a Pix that matches no open charge: it was already paid, it expired, or the
amount was wrong. Jupiter holds the money in `pix_unmatched` and returns it to the payer
(`payments.return_unmatched_pix`, every minute) (ADR 0026). This one has not gone back.

## Impact

A payer's money is held by Jupiter that is not Jupiter's or the merchant's. Returns have
a deadline under the Pix rules.

## Diagnose

```sql
SELECT e2e_id, status, amount, updated_at FROM payments.pix_received
WHERE status IN ('unmatched', 'returning', 'return_failed') ORDER BY updated_at LIMIT 20;
```

- `returning` that stays is waiting for the Pix bank to confirm the return: is
  `pix.reconcile` succeeding?
- `return_failed` is not counted by the alert and needs a person: the Pix bank refused
  the return. Its reason is in the worker's logs.

## Fix

Restore the worker or the Pix bank connection; returns go out on the next run. For a
`return_failed`, contact the Pix bank with the end-to-end id.
