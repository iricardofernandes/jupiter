# 0034. Card receivables settle through the SLC, gross, in a grade built once a day

- Status: Accepted
- Date: 2026-10-01

## Context

Res. BCB 522/2025 makes every sub-acquirer take part in centralized settlement (the SLC,
run by Núclea) as a payer to its merchants, from 11 May 2026
([research §1](../research/notes/02-brazil-regulation-and-receivables.md)). On each
settlement date:
- the network's money for a unit goes through the SLC to whoever the registry says it
  belongs to: the holder, at its domicile, or the financiers of its contracts;
- a sub-acquirer that pre-funds merchants reports it, the same day or the next.

Núclea's layouts were not available. Until now, settling a unit told the registry and
moved the recipient's pending balance; no cash came in, and what the networks owed stayed
on `network_receivable`.

## Decision

Once a business day, per mode, the worker:
1. **Settles the units due** with the registry, which fixes their split. A unit that cannot
   be settled (not registered as it is, the registry's split off) waits for the next
   business day.
2. **Builds the day's grade** from the units settled that day:
   - the registry's payments, the block back to Jupiter included;
   - Jupiter's fee on each unit, what the network still owes for it.

   Together they are the gross outstanding. The grade is stored as built and sent again
   the same; the SLC takes the same grade again and refuses another for the day.
3. **Submits** it. A refusal is marked for an operator, not retried.
4. **Posts the cash** once the SLC reports it settled, in one entry:
   - `network_receivable` is credited with the total;
   - Jupiter's account at its bank is debited with what was paid into it (the holders'
     share, Jupiter's contracts, blocks and fees);
   - `financiers` is debited with what went straight to other institutions, which
     discharges what Jupiter owed them since the registry's split.

The SLC's totals must match the grade's, or nothing is posted.

**Refunds.** The fee a refund gives back is taken off the open installments in proportion
to the fee each still has (`fee_reduced`), so the gross outstanding follows refunds. The
network nets what settled installments gave back out of later settlements; it stays on
`network_receivable` until then.

**Anticipations** are reported to the SLC, each unit once, with the day Jupiter bought it.
`sim-slc` lists a report after the business day following as late.

`sim-slc` takes grades for today, a business day, with entries due by today. At its
settlement window it credits the participant with the entries at its own ISPB and pays the
rest to other institutions.

## Alternatives rejected

- **Settling unit by unit with the SLC.** The SLC works in daily grades; one message per
  unit would have no counterpart.
- **Paying the net only, with the fee swept later.** The network pays the gross; the fee
  is Jupiter's share of what it pays. Sweeping it out of `card_fees` into Jupiter's own
  funds is a treasury move left for later.
- **Telling the registry after the SLC settles.** The split must be fixed before the grade
  is built from it. The registry is told the day's settlement before the window, and the
  units cannot change after.

## Consequences

- The golden path reaches cash: settlement pays the seller's units into her available
  balance, and she is paid out by Pix.
- Units of a day without a grade, because the worker missed it, settle the next business
  day in that day's grade; the registry lists the notice as late.
- A refused grade holds its units out of every later grade until an operator acts.
