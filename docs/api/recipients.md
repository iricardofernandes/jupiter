# Recipients, splits and anticipation

A marketplace divides its card payments among the sellers whose goods it sold. Each seller
is a recipient, with a balance of its own, and receives its share as the card network
pays: one installment at a time, a month apart, unless it anticipates them.

## Recipients

```sh
curl -X POST http://127.0.0.1:8080/v1/recipients -H "Authorization: Bearer $SK" -H "Idempotency-Key: $(uuidgen)" -d '{
  "name": "Vendedora", "tax_id": "11222333000262",
  "payout_destination": {"type": "pix", "pix_key": "vendedora@example.com"},
  "automatic_anticipation": {"enabled": false}
}'
```

- **`tax_id`.** A recipient's CPF or CNPJ is who its receivables belong to at the registry.
  It is one recipient per mode, and it does not change.
- **`status`.** A recipient starts `pending` verification (KYC or KYB) and ends `verified`
  or `rejected`. In test mode, `POST /v1/test_helpers/recipients/{id}/verify
  {"status": "verified"}` decides.
- **What needs verification.** Only a verified recipient is named in a split, paid out or
  anticipates. Changing a recipient's name or payout destination sends it back to
  `pending`.
- **`me`.** It names the merchant's own recipient wherever a recipient is named. The
  recipient is made from the merchant's CNPJ, and is verified.
- **`transfer_settings`.** Manual, daily, weekly or monthly; stored for the scheduled
  payouts to come.

## Splitting a payment

```json
"split": [
  {"recipient": "rp_…", "percentage": "90.00", "liable": true},
  {"recipient": "me", "percentage": "10.00", "remainder": true, "charge_fee": true}
]
```

**The rules:**
- Every rule is a `percentage`, adding up to `100.00`, or every rule is an `amount`,
  adding up to the payment.
- Exactly one rule is `liable` for chargebacks.
- Exactly one rule takes the `remainder`, the centavos rounding leaves.
- The `charge_fee` rules pay Jupiter's fee, in proportion to their shares, and hold at
  least 10% of the payment.

A split that breaks a rule is refused with `parameter_invalid`. Only card payments are
split.

**At capture**, each recipient's net share becomes its pending balance and its units. The
split is recorded as typed lines:
- `recipient` for a seller's share;
- `commission` for the merchant's own;
- `fee`;
- `remainder`.

**R$ 600.00 in six installments** split as above:
- The seller gets R$ 540.00: six units of R$ 90.00.
- The marketplace gets R$ 60.00 less the R$ 20.94 fee: six units of R$ 6.51.

## Balances

`GET /v1/recipients/{id}/balance` returns:

| Field | What it is |
|---|---|
| `pending` | What its unsettled units hold, by the day each becomes available (`pending_on`: the unit's settlement date) |
| `available` | What can be paid out: `POST /v1/payouts {"amount": …, "currency": "brl", "recipient": "rp_…"}` pays it to the recipient's own Pix key, and to no other |
| `reserved` | What is held back from it |

`GET /v1/receivables/agenda?recipient=rp_…` shows each unit, with what financiers' contracts
hold of it.

A refund takes the payment back from its recipients in proportion to their shares:
- first from what is still pending;
- then, for what was anticipated, from what is available.

## Anticipation

A verified recipient can have its units now. Jupiter buys them for their present value at
its monthly rate (1.99%): `amount / (1 + 0.0199 × days / 30)`.

1. `POST /v1/anticipations/simulate {"recipient": "rp_…", "units": ["ur_…", …]}` quotes the
   units named, or all that can be anticipated. It quotes what of each is still free at
   the registry. The quote holds for ten minutes.
2. `POST /v1/anticipations {"quote": "aq_…"}` buys them. The registry records an ownership
   transfer to Jupiter on each unit, the units settle to Jupiter, and the price is the
   recipient's available balance.

With `automatic_anticipation: {"enabled": true, "delay_days": 1}`, Jupiter buys every unit
of the recipient's that is free, once it is a day old.
