# JupiterPayoutsHeld

**Warning.** Metric: `jupiter_payouts_held`: payouts `held` for an operator for more than
five minutes.

## What it means

A balance's payouts are held until an operator lifts the hold. Jupiter holds them by
itself when a merchant gives its own recipient a new payout destination: a key that was
stolen could otherwise point the merchant's money at an account of the thief's. An
operator may also hold a recipient's payouts.

## Impact

The merchant or recipient is waiting for money. The amount stays set aside on the balance,
so nothing is lost while the payouts wait.

## Diagnose

```sql
SELECT p.id, p.merchant_id, p.recipient_id, p.method, p.pix_key, p.bank_ispb, p.bank_account, p.amount, p.created_at
FROM payments.payouts p WHERE p.status = 'held' ORDER BY p.created_at LIMIT 20;

SELECT id, merchant_id, livemode, is_default, payout_method, pix_key, bank_ispb, bank_account, updated_at
FROM recipients.recipients WHERE payouts_held ORDER BY updated_at;
```

- Who changed the destination, and from where? The API's request log names the key that
  made the `POST /v1/recipients/...` and its address.
- Does the new destination belong to the merchant or recipient? A Pix key's holder is in
  the payout once it is paid; ask the Pix bank's directory (DICT) before that.

## Fix

Confirm the change with the merchant through a channel other than the API, such as its
registered phone. If it is theirs, lift the hold:
`jupiterctl recipient release-payouts <recipient>`, which sends the held payouts. If it is not,
revoke the key that made the change, set the destination back, and leave the hold until
the merchant has rotated its keys.
