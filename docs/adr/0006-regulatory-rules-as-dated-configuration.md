# 0006. Rules regulation can change are configuration with effective dates

- Status: Accepted
- Date: 2026-09-30

## Context

The research found several rules Jupiter depends on to be changing, in conflict or
unverified ([research: synthesis, "Conflicting and unverified claims"](../research/payments-market.md#conflicting-and-unverified-claims)):

- Visa's framework gives 10 days of authorization validity for a customer-initiated
  card-not-present payment; Stripe's documentation gives 7.
- Res. BCB 562/2026 revoked and replaced the lien-release and anticipation-cancellation
  deadlines of Res. 514/2025, and the new wording was not available to the research.
- The participant's chargeback liability cap is 180 days in the draft read, after an
  earlier proposal of 120.
- MED contestation windows went from 30 to 80 days on 1 September 2026.

Brazilian payment regulation changes quarter by quarter, and a payment authorized under
one rule must still be settled, disputed or reconciled under that rule after it changes.

## Decision

Every rule that a regulator, a scheme or a counterparty can change is **data**, not code:
authorization validity windows, settlement terms, dispute deadlines per scheme and stage,
liability caps, registry and MED deadlines, and interchange caps.

Each rule is stored with the date it takes effect. Jupiter evaluates a rule as of the
date that matters for the object in question (usually when the payment was authorized or
the dispute opened), not as of today. Each value records its source, and values the
research could not verify are marked as such.

The mechanism (tables versus versioned files) is decided in the first phase that needs a
rule, phase 5 for authorization validity. This record fixes the principle.

## Alternatives rejected

- **Constants in code.** A deploy per regulation change, and no way to apply the old rule
  to payments made before the change.
- **Configuration without effective dates.** Updating a value would silently change the
  rules for payments already in flight.
- **Waiting for certainty before modelling.** Several primary texts could not be read. The
  domain still has to be built; the uncertainty is carried as data that can be corrected.

## Consequences

- Rule lookups take a date. Tests can pin a date and check both sides of a change.
- A wrong value becomes a data fix with an audit trail, not a code change.
- Every rule needs a documented source, which keeps unverified claims visible.
