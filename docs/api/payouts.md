# Payouts

A payout sends part of your balance to a Pix key:

```sh
curl -X POST http://127.0.0.1:8080/v1/payouts -H "Authorization: Bearer $SK_LIVE" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"amount": 60000, "currency": "brl", "destination": {"type": "pix", "pix_key": "+5511987654321"}}'
```

The key is a CPF or CNPJ, an e-mail, a phone number in international format
(`+5511…`), or a random key. The amount is taken from your balance at once, so it
cannot be paid out twice.

A recipient's payout comes from its available balance and goes to its own
`payout_destination`, by Pix or, for a bank account, by bank transfer:

```sh
curl -X POST http://127.0.0.1:8080/v1/payouts -H "Authorization: Bearer $SK" \
  -H "Idempotency-Key: $(uuidgen)" -d '{"amount": 9000, "currency": "brl", "recipient": "rp_…"}'
```

A payout is at least R$ 1.00 by Pix and R$ 10.00 by bank transfer.

| Status | Meaning |
|---|---|
| `pending` | Sent to the bank, not yet confirmed. A Pix is usually confirmed at once; a bank transfer, when the bank makes it. A payout whose answer was lost stays here until the bank is asked again |
| `held` | Jupiter is holding the recipient's payouts for review, as it does when your own recipient gets a new payout destination. The amount is set aside; the payout goes once the hold is lifted |
| `paid` | The money reached the destination. For a Pix, `end_to_end_id` identifies it in the Pix system, and `destination.recipient_name` is who the key belongs to |
| `failed` | The bank could not send it, for example to a key or account that does not exist. `failure_message` says why, and the amount is back in the balance |
| `returned` | A bank transfer was paid and then sent back by the receiving bank, for example from a closed account. `failure_message` has the bank's reason and `returned_at` when; the amount is back in the balance |

You can pay out what the balance has available: for your own, what your payments received,
less refunds and earlier payouts, including those still pending. Asking for more answers
`balance_insufficient`.

## Scheduled payouts

A recipient whose `transfer_settings` are `daily`, `weekly` (a day 1 to 5, Monday to
Friday) or `monthly` (a day 1 to 28) is paid out its whole available balance on those days,
or the next business day: once a day, with `scheduled_on` set. A balance below the minimum
waits for the next one.

Events: `payout.created`, `payout.paid`, `payout.failed`, `payout.returned`. Keys need
`payouts:write` to create payouts and `payouts:read` to see them.
