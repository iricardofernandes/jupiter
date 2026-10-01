# Receivables

A card payment is not paid to Jupiter at once. A credit payment in six installments
financed by the merchant is paid by the card network in six parts, about a month apart.
Jupiter keeps what each merchant will receive, and when, as receivable units:
- one per arrangement (VCC for Visa credit, MCC for Mastercard credit, and so on) and
  settlement date;
- net of Jupiter's fee and of refunds.

As a sub-acquirer, Jupiter registers the units with a receivables registry, where banks
and other financiers can lend against them or buy them.

## Fees

Each capture pays Jupiter's fee, at the price in effect that day:

| Payment | Fee |
|---|---|
| In one payment, or in installments financed by the issuer | 2.99% |
| 2 to 6 installments financed by the merchant | 3.49% |
| 7 to 12 installments financed by the merchant | 3.99% |

The fee comes out of the balance when the payment is captured. A refund gives back its
share of the fee.

## The agenda

```sh
curl "http://127.0.0.1:8080/v1/receivables/agenda?from=2026-10-01&to=2027-06-30" -H "Authorization: Bearer $SK"
```

Each unit says:
- `amount`: what it will settle for;
- `blocked`: what Jupiter holds back;
- `committed`: what contracts at the registry hold, and for whom;
- `free`: what nobody holds;
- `registered`: whether the registry has it yet;
- once settled, `settled_amount` and `settled_on`.

`as_of`, a Unix timestamp, shows the agenda as it stood then. `recipient` shows a
recipient's agenda instead of the merchant's own ([recipients](recipients.md)).

R$ 600.00 in six installments captured on 1 October 2026 becomes six units of R$ 96.51:
R$ 100.00 each, less 3.49%. The first settles 30 days later. That day is a Saturday and
the Monday after it is a holiday, so it settles on Tuesday 3 November. Each of the others
settles 30 days after the one before, on a business day.

## Financiers

A financier sees the merchant's agenda at the registry only with the merchant's
authorization:
- `POST /v1/receivables/opt_ins {"financier": "<CNPJ>"}` grants it;
- `POST /v1/receivables/opt_ins/<CNPJ>/revoke` withdraws it;
- `GET /v1/receivables/opt_ins` lists them, with whether the registry has been told.

Contracts a financier places on the units appear in `committed` once Jupiter's daily
reconciliation with the registry sees them.

The merchant's CPF or CNPJ is who the units belong to at the registry. Until it is set
(`jupiterctl merchant set-tax-id`), units are kept but not registered.

## Settlement

On a unit's date, the card network pays it through centralized settlement (the SLC):
- what a financier holds goes straight to the financier;
- the rest is paid into Jupiter's account, and becomes your available balance (or the
  recipient's), which a payout can send out.

A unit settles once, on its date or, if Jupiter could not settle it then, the next business
day. Its `settled_amount` and `settled_on` show it in the agenda.
