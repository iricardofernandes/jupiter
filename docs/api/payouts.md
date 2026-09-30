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

| Status | Meaning |
|---|---|
| `pending` | Sent to the bank, not yet confirmed. Usually brief; a payout whose answer was lost stays here until the bank is asked again |
| `paid` | The Pix reached the key. `end_to_end_id` identifies it in the Pix system, and `destination.recipient_name` is who the key belongs to |
| `failed` | The bank could not send it, for example to a key that does not exist. `failure_message` says why, and the amount is back in your balance |

You can pay out what your balance has available: what your payments received, less
refunds and earlier payouts, including those still pending. Asking for more answers
`balance_insufficient`.

Events: `payout.created`, `payout.paid`, `payout.failed`. Keys need `payouts:write` to
create payouts and `payouts:read` to see them.
