# Accepting Pix

A Pix is pushed by the customer from their own bank: you show them what to pay, and the
payment succeeds when their Pix arrives. Jupiter makes the charge at its bank, gives you
the BR Code to show, and tells you when it is paid.

## A payment

Create and confirm a payment intent with the payment method `pix`:

```sh
curl -X POST http://127.0.0.1:8080/v1/payment_intents -H "Authorization: Bearer $SK_LIVE" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"amount": 12345, "currency": "brl", "payment_method": "pix", "confirm": true, "description": "Pedido 42"}'
```

It answers `requires_action` with the code:

```json
"next_action": {
  "type": "pix_display_qr_code",
  "pix_display_qr_code": {"data": "00020101021226810014br.gov.bcb.pix2559...6304ABCD", "expires_at": 1790886400}
}
```

Show `data` as a QR code and as text the customer can copy and paste into their bank's
app (Pix Copia e Cola). When their Pix arrives, the intent is `succeeded` and you get
`payment_intent.succeeded`. The description, up to 140 characters, is shown to the
payer.

Pix is for BRL only, with automatic capture, no installments and no `setup_future_usage`.
The risk engine and 3-D Secure do not apply: the payer's bank authenticates them.

## How long a code can be paid

An immediate charge can be paid for a day unless you set `pix[expires_after_seconds]`
(from 60 seconds to 14 days). When it expires unpaid, the intent goes back to
`requires_payment_method` with `payment_intent_payment_attempt_expired`. Confirm it again
for a new code; the old one can no longer be paid.

Canceling a payment intent that waits on a Pix removes the charge first: from then on the
code cannot be paid. If the customer paid it at that very moment, the payment wins and the
intent succeeds instead.

## A charge with a due date

For a bill, a boleto-like charge, give `pix[due_date]` and the payer:

```json
"pix": {
  "due_date": "2026-10-15",
  "days_after_due": 30,
  "payer": {"name": "Maria da Silva", "tax_id": "12345678909"},
  "fine": {"percent": "2.00"},
  "interest": {"monthly_percent": "1.00"},
  "discount": {"amount": 500, "until": "2026-10-10"}
}
```

It can be paid until `days_after_due` days after the due date (30 by default). Paid
early, the discount comes off. Paid late, the fine (an amount or a percent, once) and the
interest (a percent a month, per day late) are added. The customer's bank computes the
amount on the day they pay, so `amount_received` is what they paid, which may be more or
less than `amount`. `tax_id` is a CPF or a CNPJ with valid check digits.

## Refunds

Refund a Pix payment as any other (`POST /v1/refunds`), all or part of what was received.
Jupiter asks the bank to return that much of the customer's Pix. The refund is `pending`
until the bank confirms the return, usually within seconds, then `succeeded`, or `failed`
with `pix_return_failed` when the bank could not return it, for example because the
customer's account was closed.

## Pix nobody asked for

A Pix that pays nothing you are waiting for is returned to whoever sent it, and never
reaches your balance. That covers a Pix sent to Jupiter's key without a charge, and one
that paid a charge already expired or canceled.
