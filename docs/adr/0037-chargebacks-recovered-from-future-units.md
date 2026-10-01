# 0037. A chargeback is the liable recipient's, recovered from its future units in the Convenção's order

- Status: Accepted
- Date: 2026-10-01

## Context

A card chargeback takes money Jupiter may already have paid out. With installments and
anticipation, it may also hit receivables that were sold. Each split names one recipient
liable for chargebacks ([ADR 0031](0031-recipients-split-and-balances.md)), as Pagar.me
does.

Res. BCB 264 nets reversals from disputes out of a unit's constituted value. The
Convenção entre Entidades Registradoras (item 3.14) fixes how a unit is reduced: its
free amount first, then its contracts from the latest accepted back
([research §2](../research/notes/02-brazil-regulation-and-receivables.md)). A
sub-acquirer's merchants are paid through their units, so a debt that their balances
cannot cover can only be recovered from units that have not settled yet.

## Decision

**At once.** When a chargeback opens, its amount:
- leaves the merchant's balance for `network_receivable`, since the network nets it from
  what it owes;
- comes back to that balance from the liable recipient's available balance. That balance
  goes below zero when it does not have the amount, and the shortfall is what the
  recipient owes.

A payment without receivables, from a merchant without a CPF or CNPJ, is taken from the
merchant's balance alone. What was refunded already is not withdrawn again.

**Recovery.** Every pass of the receivables worker takes each recipient whose available
balance is below zero and reduces its unsettled units, nearest settlement date first. For
each unit, Jupiter reads the registry's position and reduces it as the registry will:
- what is free first;
- then the contracts, from the latest accepted back;
- stopping at the first of Jupiter's own. Reducing a unit Jupiter bought would only move
  the loss to Jupiter.

What is taken from the unit:
- reduces its installments, in proportion to what each holds;
- moves from the recipient's pending balance to its available balance.

When the unit is next registered, the registry applies the same order. Only units the
registry holds as they are, settling after today, are reduced. Each reduction is
recorded, with what it took from what was free and from each contract.

**Won.** When the dispute is won, the amount goes back the way it came: from the network
to the merchant's balance, and on to the liable recipient. Units reduced to recover it
stay reduced. The recipient has that amount available at once, instead of on the units'
dates.

## Alternatives rejected

- **Blocks instead of reductions.** A block (Res. BCB 264 art. 8º) takes only what is free
  and settles to the accreditor; it offsets chargebacks already settled. But a sale
  reversed by a dispute is a reduction in the registry's own terms. A reduction also
  reaches other financiers' contracts in the order the Convenção sets, which a block
  never does.
- **Sharing the chargeback among the split's recipients.** That is what refunds do. A
  split names one liable recipient precisely so that sellers who did not cause a dispute
  do not pay for it.
- **Waiting for the debt to settle out of later payouts.** It would work for recipients who
  keep selling. It would also leave their future units free to be pledged or anticipated
  elsewhere first, and it would ignore the registry, which must reflect reversals.

## Consequences

- The exit test: a seller sells everything to Jupiter and is paid out, then a bank takes a
  lien on her agenda. A chargeback is recovered from her next sale's units: the free part
  first, then the bank's lien, never Jupiter's contracts. The ledger, payments,
  receivables and disputes checks agree at every step.
- The recovery is Jupiter's plan, read from the registry's position at that moment. If
  another financier's contract is accepted before the unit is next registered, the
  registry may apply its order differently, and the daily reconciliation compares only
  values.
- What a chargeback takes from `network_receivable` beyond what units recover stays there
  until the network nets it, as refunds of settled installments already do; phase 13's
  reconciliation matches it.
- A recipient with no unsettled units keeps owing until it sells again; nothing else
  recovers it.
