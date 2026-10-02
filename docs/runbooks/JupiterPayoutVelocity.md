# JupiterPayoutVelocity

**Warning.** Metric: `jupiter_payouts_balances_near_daily_limit`: balances whose payouts
asked for through the API today reached 80% of `JUPITER_PAYOUT_DAILY_LIMIT`.

## What it means

A merchant's balance, or a recipient's, is being paid out faster than its usual pace:
close to the limit of a day (Brasília), after which payouts are refused with
`payout_limit_reached`. Scheduled payouts are not counted. Payouts go only to the
balance's verified destination, so the money reaches its owner; what is unusual is the
pace, which can mean a key in the wrong hands draining the balance before a dispute or
refund can be paid from it.

## Impact

None yet. Past the limit, the merchant's payouts of the day are refused, and the rest
waits for the next day.

## Diagnose

```sql
SELECT merchant_id, livemode, recipient_id, count(*), sum(amount)
FROM payments.payouts
WHERE created_at >= date_trunc('day', now() AT TIME ZONE 'America/Sao_Paulo') AT TIME ZONE 'America/Sao_Paulo'
  AND scheduled_on IS NULL AND status NOT IN ('failed', 'returned')
GROUP BY 1, 2, 3 ORDER BY 5 DESC LIMIT 20;
```

- Which keys asked for them? The API's request log has each `POST /v1/payouts`.
- Is it the merchant's usual volume, such as a large sale day? Compare with past days.

## Fix

If the merchant confirms, nothing: the limit resets at midnight, and a merchant whose
ordinary volume is above it needs a higher `JUPITER_PAYOUT_DAILY_LIMIT`. If it does not,
revoke the keys that asked for the payouts, and hold the merchant's own recipient
(`jupiterctl recipient hold-payouts <recipient>`) until it has rotated them.
