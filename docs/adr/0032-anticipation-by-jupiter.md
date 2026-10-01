# 0032. Anticipation: Jupiter buys units at a dated rate, registered as an ownership transfer

- Status: Accepted
- Date: 2026-10-01

## Context

A recipient waiting months for its installments can have them earlier: the PSP buys the
units (antecipação, the post-contracted kind) and is paid in their place when they settle
([research §3](../research/notes/02-brazil-regulation-and-receivables.md)). Two things are
required:
- **Free amounts only.** The purchase may only take what the registry holds free on a
  unit, after other financiers' contracts and blocks (Convenção 5.4, Res. BCB 264 art. 8º
  §3º).
- **Reported.** The registry must record it so that settlement pays the buyer.

The plan makes Jupiter the only financier and asks for `simulate → create` and automatic
anticipation.

## Decision

**Simulate** (`POST /v1/anticipations/simulate`) prices a verified recipient's units,
those named or all that can be:
- **Eligible units.** Registered as they are, unsettled, settling after today.
- **Amount.** For each unit, what the recipient still has pending on it, up to what the
  registry holds free.
- **Price.** The amount discounted at Jupiter's monthly rate over the days until the unit
  settles: `amount / (1 + rate × days / 30)`, rounded down. The rate comes from a dated
  rule table, 1.99% a month to start.

The research found simple and compound discounts both in use. Simple discount keeps the
arithmetic in exact rationals, with no floating point. The quote holds for ten minutes.

**Create** (`POST /v1/anticipations {quote}`) carries a quote out in one transaction:
1. It locks the quote and its units.
2. It checks each unit still has what the quote counted on, here and at the registry.
3. It places one contract per unit at the registry, with Jupiter as financier: an
   ownership transfer of a fixed amount, reaching only that unit. A contract is named by
   the quote and the unit, and the registry accepts the same contract again. A repeat
   after a lost answer therefore places nothing twice.
4. It posts the purchase to the ledger:
   - the amount out of the recipient's pending balance;
   - the price into its available balance;
   - the difference into Jupiter's anticipation fees.

A quote is used once; carrying it out again answers the same anticipation.

**Contracts the registry took for a purchase Jupiter undid.** The registry calls run inside
the transaction that records the purchase, with the units locked. If the transaction is
then undone, the registry keeps the contracts.
- **A manual purchase carried out again.** It counts its own contracts, named by its
  quote, as free, and places them again, the same.
- **Every other case.** The daily reconciliation makes the registry commit to Jupiter, on
  each unit, what Jupiter holds of it. Jupiter's contracts there end, and one for what it
  holds takes their place. This also mends a fixed contract left larger than what Jupiter
  holds after a refund, which the registry would otherwise fill again from later sales.
  The new contract comes after any accepted since, and the registry pays those first.

**After a purchase:**
- **Settlement.** The registry pays Jupiter its part of the unit, and the settlement notice
  checks that the registry's split gives Jupiter exactly what it bought.
- **Reconciliation.** The daily reconciliation compares what the registry commits to
  Jupiter on each unit with what Jupiter bought.
- **Refunds.** A refund that reaches what Jupiter bought takes it back from the recipient's
  available balance.

**Automatic anticipation** is a recipient setting: enabled, with a delay of 1 to 30 days.
The worker buys every free unit at least that old, priced as a simulation would be, a page
of recipients at a time. A simulation reads without locking anything.

## Alternatives rejected

- **A lien (ônus) instead of an ownership transfer.** Jupiter is buying, not lending
  against the units: a troca de titularidade.
- **One contract for all of a recipient's units.** A fixed contract spreads over every unit
  it reaches, earliest first, and would not stay on the units priced.
- **Pricing with compound interest.** It needs fractional powers, so floating point or a
  series approximation. Simple discount is common in the market and exact here.
- **Paying the price from Jupiter's own funds in the ledger now.** No cash moves in the
  ledger until settlement arrives (phase 11). The client-funds book records what the
  recipient may withdraw, and phase 11 adds the funding.

## Consequences

- An anticipated recipient's available balance can be paid out at once, before the network
  pays. Funding that is Jupiter's, and modelled with phase 11's settlement.
- The registry is the authority on effects: a unit reduced after a purchase may reduce
  Jupiter's contract differently from Jupiter's records (it takes free amounts first and
  then the latest contract). The daily reconciliation reports the difference and mends
  it.
- A refund that reaches what Jupiter bought, after the recipient was paid out, takes its
  available balance below zero: a debt to Jupiter, for phase 12's recovery from later
  units.
- Startup refuses a registry without Jupiter's CNPJ (`JUPITER_TAX_ID`) and a financier
  token of its own.
