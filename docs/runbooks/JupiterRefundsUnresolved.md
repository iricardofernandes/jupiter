# JupiterRefundsUnresolved

**Warning.** Metric: `jupiter_payments_refunds_unresolved`: refunds `pending` or
`refund_unknown` for more than 15 minutes.

## What it means

A refund was sent to the rail and its outcome is not known. The resolver
(`payments.resolve`) asks the rail about it, and sends it again with the same key if the
rail never received it.

## Impact

The customer has not been told the money is coming back. The merchant's balance already
counts the refund as in flight, so it cannot be paid out.

## Diagnose

```sql
SELECT id, intent_id, status, amount, updated_at, unknown_since, resolutions
FROM payments.refunds WHERE status IN ('pending', 'refund_unknown') ORDER BY updated_at LIMIT 20;
```

- `resolutions` counts how many times the resolver asked. A high count means the rail
  keeps answering "unknown" or not at all.
- For a Pix refund, the return goes through the Pix bank: is `pix.reconcile` succeeding?
- For a live card refund, look at its exchange: `SELECT * FROM acquirer.exchanges WHERE
  key = '<refund id>'`.

## Fix

Restore the worker or the rail; the resolver finishes the rest. A refund the rail
refused ends `failed` and gives the money back to the merchant's balance: tell the
merchant, who can refund again.
