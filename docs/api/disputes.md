# Disputes

A dispute is a payment's customer getting their money back through their bank, and you
answering it:
- a **chargeback** (`kind: chargeback`): the card's issuer disputes a card payment through
  the card network;
- a **MED claim** (`kind: med`): the payer contests a Pix as fraud in their bank's app,
  under the Banco Central's Mecanismo Especial de Devolução.

Both are the same object, with the same events:

```sh
curl http://127.0.0.1:8080/v1/disputes?payment_intent=pi_… -H "Authorization: Bearer $SK"
```

| Status | Meaning |
|---|---|
| `needs_response` | You can answer until `due_by`: submit evidence, or accept |
| `under_review` | Waiting on the network, the payer's bank, or Jupiter's analysis |
| `won` | The money is yours: what was taken comes back (`funds: reinstated`), or what was held is released (`funds: released`) |
| `lost` | The money went back to the customer |

`stage` says where the dispute is:
- for a chargeback: `chargeback`, then `pre_arbitration` if the issuer rejects your
  evidence, then `arbitration` if you escalate;
- for a MED claim: `med_analysis`, then `med_contestation` if you contest the return.

`history` lists every change.

## Answering

Save evidence, and submit it when it is complete:

```sh
curl -X POST http://127.0.0.1:8080/v1/disputes/dp_… -H "Authorization: Bearer $SK" \
  -H "Idempotency-Key: $(uuidgen)" \
  -d '{"evidence": {"shipping_tracking_number": "BR123456789", "product_description": "…"}, "submit": true}'
```

The evidence is text only: `product_description`, `customer_name`, `customer_email`,
`customer_communication`, `shipping_tracking_number`, `refund_policy` and
`uncategorized_text`. Each field holds up to 5,000 characters, and all of them together
up to 32 KB. Jupiter sends what you write; it does not make evidence of its own.

What a submission does depends on the stage:
- at `chargeback`, Jupiter represents to the network;
- at `pre_arbitration`, it escalates to arbitration;
- for a MED claim, a Jupiter analyst weighs your answer before the payer's bank's block
  ends.

To give up, accept:

```sh
curl -X POST http://127.0.0.1:8080/v1/disputes/dp_…/close -H "Authorization: Bearer $SK" -H "Idempotency-Key: $(uuidgen)"
```

**Deadlines.** `due_by` is two days before the network's deadline, so Jupiter has time to
send your answer. When it passes:
- a chargeback is represented with the evidence you saved, if you saved any, and accepted
  otherwise;
- a pre-arbitration is accepted;
- a MED claim you did not answer is accepted, which returns the money to the payer.

Deadlines per network and stage are in
[`internal/disputes/rules/deadlines.csv`](../../internal/disputes/rules/deadlines.csv),
with their sources.

## Chargebacks

When a chargeback opens, its amount is taken from your balance at once (`funds:
withdrawn`). The network has taken it from Jupiter. If the payment was split, it comes out
of the available balance of the recipient the split made `liable`.

A recipient that does not have it owes it. Jupiter then recovers it from the recipient's
future receivable units, in the order the registries apply:
- what is free on each unit first;
- then other financiers' contracts, from the latest back.

A balance a chargeback leaves below zero is what you owe: nothing can be paid out of it
until what you receive next has brought it back above zero. Refunds and MED holds, which
Jupiter can refuse, never take it below zero.

If the dispute is won, the amount comes back to the recipient.

A chargeback opened more than 180 days after the payment was authorized is the card
scheme's (Res. BCB 522/2025): `liability: scheme`. Jupiter takes nothing from you and
answers it for you.

A fraud chargeback (`reason: fraudulent`) of a payment authenticated by 3-D Secure is the
issuer's, and the network refuses it.

## MED claims

When a MED claim arrives, Jupiter holds its amount on your balance, as far as the balance
has it (`funds: held`, `held`). Payouts cannot spend held money. What already left by
payout is traced instead: `trace` lists those payouts, which the payer's bank can follow.
Then:
- if you accept, or Jupiter agrees after weighing your answer, the bank returns the
  amount held to the payer, and the claim is lost. Should the bank return more than was
  held, what it returned beyond the hold is taken from your balance too, below zero if it
  has not got it, as a chargeback's would be;
- if Jupiter disagrees, or no decision is reached within the 11-day block, the hold ends
  and the claim is won.

If a claim was lost, you can contest the return once, within 80 days (30 before 1
September 2026), by submitting evidence. If the payer's bank upholds it, the money comes
back.

## Fraud reports and the dispute ratio

Issuers also report payments as fraud without disputing them (TC40, SAFE). These reports
move no money, and are listed at `GET /v1/fraud_reports`.

Like Visa's acquirer monitoring program, disputes and fraud reports are measured against
your card payments, each month:

```sh
curl "http://127.0.0.1:8080/v1/dispute_monitoring?month=2026-10" -H "Authorization: Bearer $SK"
```

`ratio_bps` is card disputes plus fraud reports, per 10,000 card payments. You are
`excessive` at 150 bps with at least 1,500 of them; Jupiter's operators are told.

## Events and scopes

Events:
- `dispute.created` and `dispute.updated`;
- `dispute.closed`, when a dispute is won or lost;
- `dispute.funds_withdrawn`, when money is taken or held;
- `dispute.funds_reinstated`, when it comes back or is released;
- `fraud_report.created`.

Keys need `disputes:read` to see disputes and `disputes:write` to answer them.

In test mode, `POST /v1/test_helpers/disputes` opens a chargeback, and
`POST /v1/test_helpers/fraud_reports` reports a payment as fraud. See
[Testing payments](testing.md#disputes).
