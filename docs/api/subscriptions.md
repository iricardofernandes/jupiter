# Subscriptions

A subscription charges a customer every week, month, quarter, half-year or year by Pix
Automático. The customer authorizes it once. From then on, their bank debits each charge
on its due date, and they do nothing else.

```sh
curl -X POST http://127.0.0.1:8080/v1/subscriptions -H "Authorization: Bearer $SK_LIVE" \
  -H "Idempotency-Key: $(uuidgen)" -d '{
    "amount": 4990, "currency": "brl", "interval": "month", "start_date": "2026-11-05",
    "description": "Plano mensal",
    "customer": {"name": "Maria da Silva", "tax_id": "12345678909"},
    "authorization": {"method": "qr_code"}
  }'
```

- `start_date` is the first charge's due date. The others follow by the interval: a
  day the month does not have becomes its last, so a subscription from January 31 is
  due on February 28.
- `end_date`, optional, is the last day a charge may be due. `start_date` is at most a
  year away.
- `description` is what the customer's bank shows them, up to 35 characters.
- `retries`, `three_in_seven` unless set: when a debit fails, for lack of funds for
  example, the customer's bank tries again up to three times, on different days, within
  seven days of the due date. `none` fails the charge at once.

## Authorization

The subscription is `incomplete` until the customer authorizes it.

| `authorization.method` | How | `next_action` |
|---|---|---|
| `qr_code` | The customer reads a QR code in their bank's app | `pix_display_qr_code`, with the code in `data`: show it as a QR code and as text |
| `payer_request` | Jupiter sends the request to the customer's bank, which shows it to them | `awaiting_payer_authorization` |

A payer request needs the customer's account: `payer_bank: {"ispb": "…", "branch": "…",
"account": "…"}`. The customer can refuse it, and the subscription is then `rejected`, as
it is when their bank cannot receive the request (`end_code` `bank_refused`). Only one
subscription at a time waits for a customer's authorization.

## Statuses

| Status | Meaning |
|---|---|
| `incomplete` | Waiting for the customer's authorization |
| `active` | Charging |
| `past_due` | The latest charge failed; the next one goes on as usual |
| `rejected` | The customer refused the request, or their bank could not receive it |
| `canceled` | By you (`POST /v1/subscriptions/{id}/cancel`) or by the customer in their bank's app; `canceled_by` says which |
| `ended` | Past its `end_date` |

Every change sends `subscription.updated`.

## Charges

Each cycle is a payment intent with `payment_method: "pix_automatico"` and
`subscription` set, listed in the subscription's `cycles`. It is made eight days before
its due date and waits in `processing` while the customer's bank has it scheduled. Then:
- **paid:** `succeeded`, with `payment_intent.succeeded`;
- **not paid:** `requires_payment_method` with `payment_intent_payment_attempt_failed`,
  once its retries are used up. You may collect it another way;
- **canceled:** `canceled`, when you or the customer cancel the subscription before the
  day before it is due. A charge due today or later in its retries goes on, and is
  followed until it ends.

A failed payment may be updated to another method, a plain `pix` charge included, and
confirmed like any other; its cycle stays `failed`. Refunds work as for any Pix payment.
