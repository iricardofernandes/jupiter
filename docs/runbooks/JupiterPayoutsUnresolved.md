# JupiterPayoutsUnresolved

**Warning.** Metric: `jupiter_payouts_unresolved`: payouts `sending` or `unknown` for more
than an hour.

## What it means

A payout was sent, by Pix through the Pix bank or by bank transfer through Jupiter's bank,
and its outcome is not known. A refusal (400 or 422) fails it at once. Any other error may
hide a transfer that was made, so the payout waits while `payments.resolve_payouts` (every
15 seconds) asks the bank about it (ADR 0035).

## Impact

The merchant or recipient is waiting for money, and the amount stays held on their
balance.

## Diagnose

```sql
SELECT id, merchant_id, recipient_id, method, status, amount, updated_at
FROM payments.payouts WHERE status IN ('sending', 'unknown') ORDER BY updated_at LIMIT 20;
```

- Is `payments.resolve_payouts` running and succeeding?
- Is the bank answering? The worker logs `background task failed` with the bank's error.
- A payout is named by its id at the bank, so the bank's own records can be searched for
  it.

## Fix

Never resend a payout by hand: the bank would see the same transfer, and a new id would
pay twice. Restore the connection and the resolver applies the bank's answer. If the bank
confirms it has no such transfer after a day, escalate to whoever operates the bank
relationship. Do not fail the payout yourself.
