# 0020. Rule tables are versioned files, embedded in the binary, with effective dates

- Status: Accepted
- Date: 2026-09-30

## Context

ADR 0006 made every rule a scheme or regulator can change into data with effective
dates, and left the mechanism to the first phase that needs one. Phase 5 needs how long
an authorization stays valid. That depends on the scheme, whether the card was present,
who initiated the payment and whether the amount was estimated. The sources disagree:
Visa's own framework gives 10 days for a customer-initiated card-not-present payment,
Stripe's documentation 7 ([research: card rails](../research/notes/04-card-rails.md)).

## Decision

A rule table is a CSV file in the repository, embedded into the binary with `go:embed`:
`internal/payments/rules/authorization_validity.csv`. Each row has the columns the rule
depends on (`*` for any value), the date it takes effect, the value, whether it was
`cited` from a source the research read or is `unverified`, and the source itself. A
lookup takes the most specific row in effect on the date that matters. For an
authorization that is the day it was approved, so a payment authorized under the old rule
keeps it.

The first table follows Visa's framework from 13 April 2024, with the 7 days commonly
cited before it, and marks Mastercard's rows and the fallback as unverified. The
authorization's expiry is computed when it is approved and stored on the attempt; the
ledger's hold expires a day later as a backstop.

## Alternatives rejected

- **A database table.** Rules would change without a review, and every environment
  would need the same rows seeded. A file changes through a pull request that shows the
  diff and the source.
- **Configuration read at start-up.** Two instances could run with different rules, and
  a payment's rule would depend on which served it.
- **Constants in code.** The ADR 0006 objection: no dates, and no way to apply the old
  rule to payments made before the change.

## Consequences

- Changing a rule is a data change and a deploy, reviewed with its source.
- The day a rule takes effect is tested on both sides.
- Where the research found a conflict, the table records it: the file's header states
  the Stripe and Visa disagreement, and the rows say which one Jupiter follows.
