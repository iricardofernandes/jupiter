# Boleto

A boleto is a bill your customer pays at any bank, in their bank's app, or at a lottery
outlet. Jupiter's boletos are hybrid: once the bank registers them they also carry a Pix
QR code, so the customer can pay at once by Pix.

```sh
curl -X POST http://127.0.0.1:8080/v1/payment_intents -H "Authorization: Bearer $SK" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"amount": 15000, "currency": "brl", "payment_method": "boleto", "confirm": true,
       "boleto": {"due_date": "2026-10-15", "days_after_due": 5,
                  "payer": {"name": "Maria da Silva", "tax_id": "12345678909"}}}'
```

| Field | |
|---|---|
| `boleto.due_date` | From today to a year ahead |
| `boleto.days_after_due` | How many days after the due date it can still be paid, 0 to 60. After that the bank writes it off |
| `boleto.payer` | Who pays: a name without accents and a CPF or CNPJ, as the bank registers them |
| `boleto.pix` | `false` for a boleto without a Pix QR code |

The intent waits in `requires_action`, with what to show the customer:

```json
"next_action": {
  "type": "boleto_display_details",
  "boleto_display_details": {
    "line": "99990.00103 00000.000133 20012.345615 6 16000000015000",
    "barcode": "99996160000000150000001000000000132001234561",
    "due_date": "2026-10-15",
    "pix_code": null
  }
}
```

`pix_code` fills in once the bank has registered the boleto, usually the next time Jupiter
exchanges files with it.

| What happens | The intent |
|---|---|
| The customer pays it, by the line, the barcode or Pix | `succeeded` when the bank reports it, usually the next business day. The amount is in your balance |
| You cancel it | `processing` until the bank confirms the write-off, then `canceled` |
| It is not paid by the end of its term | `requires_payment_method`, with `last_payment_error.decline_code` `boleto_expired` |
| The bank refuses to register it | `requires_payment_method`, with `decline_code` `boleto_rejected` and the bank's reason |

Boletos cannot be refunded: the bank never learns the payer's account. They have no fee.
They are captured when paid, in one go, and cannot save the payer for later.

In test mode, the bank simulator's admin pays a boleto: `POST /admin/boletos/pay` with its
`line` or `pix_code`.
