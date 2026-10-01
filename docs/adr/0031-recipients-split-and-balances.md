# 0031. Recipients hold balances, and a split is typed ledger lines at capture

- Status: Accepted
- Date: 2026-10-01

## Context

A marketplace's payment belongs to several parties: the sellers whose goods were sold,
and the marketplace's commission. Each has receivables of its own. Under Res. BCB
264/2022, a unit is a merchant's (usuário final recebedor), by its CPF or CNPJ, so a
seller's share of a payment constitutes the seller's units, not the marketplace's. The
plan asks for three things:
- recipients with KYC status, a payout destination and transfer settings;
- a split whose lines are typed and whose liability is explicit;
- balances per recipient: pending, available and reserved.

## Decision

**Recipients** (`internal/recipients`, `/v1/recipients`) belong to a merchant.
- **Identity.** Each has a CPF or CNPJ, one recipient per tax id in each mode, since the
  registry keys units by it. A seller selling in two marketplaces on Jupiter is a
  limitation of this.
- **Verification.** A recipient starts `pending` and is `verified` or `rejected`: by an
  operator (`jupiterctl recipient verify`) in live mode, by a test helper in test mode.
  Changing its name or payout destination sends it back to `pending`, since that is what
  was verified.
- **What verification gates.** Only a verified recipient can be named in a split, so no
  receivables are registered under a CPF or CNPJ nobody checked. Only a verified one is
  paid out, and only to its own Pix key.
- **The merchant's own.** Every merchant has a default recipient, made from its CNPJ the
  first time it is needed. The merchant was verified at sign-up, so this recipient is
  verified too. The API calls it `me`.

**A split** is a card payment intent's `split`: rules of a fixed amount or a percentage,
all of the same kind, adding up to the whole payment.
- **Validation.** Exactly one rule is liable for chargebacks and exactly one takes the
  remainder. No amount is more than the payment. The rules that pay Jupiter's fee hold at
  least 10% of the payment between them. Recipients must be the merchant's, in its mode,
  and verified. All of this is checked when the intent is created, updated and confirmed.
- **Pix.** Pix payments are not split.

**At capture**, the captured amount and the fee are divided:
- each rule gets its weight's share, rounded down;
- the remainder rule's recipient gets the centavos left over;
- the fee is shared by the paying rules in proportion to their shares;
- should their shares not cover the fee, as only a payment of a few centavos can make
  happen, everyone shares it in proportion.

The division therefore never fails after the card was charged, and a property test checks
it for any split:
- the lines add up to the payment, and the fee lines to the fee;
- each share is within a centavo of exact;
- no one nets less than nothing.

The recipients' units are locked first, by recipient and date, the order refunds,
anticipations and settlements lock them in. Then the lines are recorded by type, and one
ledger transaction moves the net shares from the merchant's balance to the recipients'
pending balances:
- `recipient`, a seller's share;
- `commission`, the merchant's own share;
- `fee`;
- `remainder`.

Each recipient's share then constitutes its units, installment by installment. A payment
without a split is all the merchant's own recipient's.

**Balances** are three ledger accounts per recipient, in the client-funds book:
- **Pending** is what the recipient's unsettled units hold that Jupiter did not buy. Each
  pending amount carries the date it becomes available: its unit's settlement date.
- **Available** can be paid out (`/v1/payouts` with `recipient`). An anticipation or a
  unit's settlement puts money there.
- **Reserved** is held back. Nothing reserves yet; disputes (phase 12) will.

Every posting to a recipient's account is also recorded as a movement. The receivables
check proves three things:
- each account holds what was posted to it;
- each pending balance equals what the recipient's units hold;
- the merchant's own balance still equals its payments, less what the splits moved out
  and plus what refunds moved back.

**Refunds** take the payment's net amount back from its recipients, in proportion to what
each installment still holds:
- first from pending;
- then, for what Jupiter had bought, from the recipient's available balance;
- what no installment holds any more comes from the liable recipient's available balance,
  which may go negative: a debt for phase 12 to recover.

## Alternatives rejected

- **Units keyed by merchant, with the split only in the ledger.** The registry would show
  the seller's receivables as the marketplace's: wrong under Res. 264 and useless to a
  seller's financier.
- **One account per recipient with flags for pending and available.** The ledger could not
  show what is pending without reading the units, and the invariant would be circular.
- **Failing a capture whose split cannot be computed.** The card was charged by then. The
  division is total instead, and validation keeps the fallback rare.

## Consequences

- Phase 9's units were the merchant's, keyed by it; the migration to recipients refuses to
  run over any. Phase 9 never ran beyond development databases.

- Card money no longer reaches the merchant's own balance: it is pending for its
  recipients. Payouts of card money name a recipient. Pix money stays in the merchant's
  balance, which payouts without a recipient still pay from.
- Transfer settings are stored and validated; the payouts that follow them come with
  phase 11's schedules.
