# 0030. Receivable units kept beside the ledger, registered and reconciled with a registry

- Status: Accepted
- Date: 2026-10-01

## Context

A credit card sale in six installments is six payments from the card network, a month
apart. Res. BCB 264/2022 makes the accreditor (Jupiter, as a sub-acquirer) do three
things:
- register them at a registry as receivable units: one per merchant, arrangement,
  accreditor and settlement date;
- keep each unit's constituted value current by the business day after the sale;
- report settlements, and reconcile daily, weekly and fortnightly.

Financiers lend against units or buy them through any registry. The registries'
Convenção sets the order in which contracts take a unit:
- **settlement** pays the earliest accepted contract first, then the free amount;
- **reductions** take the free amount first, then the latest contract;
- **blocks** may not touch what is committed.

The registries' file layouts were not available
([research §2](../research/notes/02-brazil-regulation-and-receivables.md)).

## Decision

**Units are their own module (`internal/receivables`), written in the capture's
transaction.**
- Payments tells receivables of every card capture and refund through an interface, in
  the transaction that posts them to the ledger.
- A capture becomes installments: one per settlement date, each an equal share of the
  amount and of the fee, the earliest taking the leftover centavos.
- Installments financed by the issuer are paid in one go.
- The dates come from a dated rule table: D+30, then every 30 days, moved to the next
  business day.
- Each installment adds its net amount to the merchant's unit for that arrangement and
  date.
- A refund takes its net amount from the payment's unsettled installments, in proportion
  to what each still holds. What they cannot cover is recorded as uncovered, for phase 12
  to recover.
- Every change to a unit is an event, so the agenda can be read as of any moment.
- The registry's view of contracts is kept as snapshots, so the agenda's commitments can
  be read as of any moment too.

**The ledger is not split by unit.** The network still owes Jupiter one receivable per
mode. The units are a finer view of the same money, and the receivables check proves it:
every unit is worth what its installments constituted less what refunds took.

**The registry is a simulator (`sim-registry`) with a format of its own (`pkg/registryapi`),**
whose fields follow the Convenção's minimum payload.
- Its waterfall is a small engine checked by a property test against a reference model:
  for any sequence of units, contracts, reductions, blocks and settlements, each
  beneficiary is paid what the reference pays, and no unit is paid twice or below zero.
- It holds accreditors to the next-business-day deadlines for updates and settlement
  notices, and lists those missed.
- Financiers see an agenda only after the merchant opts in.

**Jupiter's adapter (`internal/registry`)** sends units whole: their value and block,
absolute. Sending one again is therefore harmless, and a unit whose version the registry
has not taken is simply sent on the next pass. A unit the registry refuses waits an hour
before it is sent again, so it never holds up another merchant's, and so does an opt-in
that cannot be passed on. A unit stays locked while its settlement is reported, so no
refund can change it in between.
- **Daily reconciliation** compares values and blocks, sends a unit the registry
  disagrees on again, and records what is committed to contracts.
- **Weekly reconciliation** compares settlements and how they were split.
- **Fortnightly reconciliation** compares the merchants with a live contract.
- **Divergences** stay open until a reconciliation no longer finds them. The check flags
  one open past two business days, and a unit unregistered past the business day after
  its sale.

**Deadlines are a dated rule table.** The one the research could not confirm, for lifting
effects after a merchant ends a trava, is marked unverified: Res. BCB 562/2026 rewrote
it. The business-day calendar (`pkg/bizday`) knows the national holidays, Carnival, Good
Friday and Corpus Christi; it does not know local holidays.

**A merchant's CPF or CNPJ** identifies its units at the registry. It belongs to one
merchant only, and once set it does not change. Units of a merchant without one are kept
and not registered, and the check flags them once they are late. A merchant without one
cannot opt in.

## Alternatives rejected

- **One ledger account per unit.** Thousands of accounts per merchant, and the
  settlement of each would be a ledger transaction before any money moves. Splitting the
  network's receivable this way waits for settlement (phase 11) to say how money arrives.
- **Sending the registry deltas.** A lost or repeated delta corrupts a unit. Absolute
  values make every send idempotent.
- **Computing the waterfall in Jupiter.** The registry owns contract priority across
  every accreditor; Jupiter only reads the result.
- **Settling units on their date automatically.** No money arrives until phase 11's
  settlement. The notice is implemented and tested, and settlement will call it.

## Consequences

- The golden path registers six units and reconciles them with the registry with zero
  divergences.
- The waterfall property test checks the engine against a model that shares its reading
  of the Convenção. A hand-worked example is the independent check of that reading.
- Test mode keeps units too, and registers them only when a test registry is configured.
  The two modes need separate registries, since a merchant's units in both share their
  keys.
- What the simulator decides on its own, and what is unverified, is listed in its README.
