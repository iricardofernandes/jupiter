# 0029. Card fees are a dated price table, taken from the merchant at capture

- Status: Accepted
- Date: 2026-10-01

## Context

A receivable unit is registered net of the accreditor's fee. Res. BCB 264 art. 3º §3º
lets the constituted value be reduced only by:
- the fee;
- reversals;
- settlements;
- blocks.

That is the [research §2](../research/notes/02-brazil-regulation-and-receivables.md).
Until phase 9, Jupiter credited a merchant the whole amount of a card payment and charged
nothing. Receivables need the fee to exist, and the ledger must show it, because no
balance may be stored that the ledger cannot recompute.

## Decision

**The price** is a rule table like the authorization validity of ADR 0020:
`internal/payments/rules/card_fees.csv`. It has one rate per scheme, per who finances the
installments, and per range of installment counts, each with the date it takes effect.
The first prices are Jupiter's own:
- 2.99% in one payment, or in installments financed by the issuer;
- 3.49% for 2 to 6 installments financed by the merchant;
- 3.99% for 7 to 12.

**Charging it.** Capturing a card payment posts the captured amount to the merchant, as
before. In the same transaction, the fee at the price in effect that day moves from the
merchant's balance to a `card_fees` account. The fee is rounded half up, and it is
recorded on the attempt. The account sits in the client-funds book: the fee stays there
until settlement moves it to Jupiter's own funds (phase 11), so each book still balances
on its own.

**Refunds.** A refund gives back the refunded share of the fee. The share is cumulative
and rounded down, so refunding a payment in parts returns the whole fee, never a centavo
more. The refunded amount, net of the fee returned, is what the receivables lose.

**Invariants.** The payments check now expects each merchant's balance to hold:
- what was received;
- less what was refunded;
- less fees net of what refunds returned;
- less what was paid out.

It also expects the fee account to hold the fees charged less those returned.

## Alternatives rejected

- **Keeping the fee out of the ledger and in the receivables only.** The ledger would
  credit the merchant money that the registry says is not theirs.
- **Keeping the whole fee on refunds, as Stripe does.** The receivables would have to lose
  more than the merchant got, and a full refund would leave the merchant short of a fee
  on a sale that did not happen.
- **A price per merchant.** Prices are negotiated per merchant in practice, but nothing
  in the plan needs that yet. The table can gain a merchant column when something does.
- **Pix fees.** Pix receivables are not registered, and no phase needs a Pix price yet.

## Consequences

- A merchant's balance after a capture is net. The golden path pays out R$ 579.06 of the
  R$ 600.00.
- Changing a price is a data change with its date, and it applies only to payments
  captured from that date on.
