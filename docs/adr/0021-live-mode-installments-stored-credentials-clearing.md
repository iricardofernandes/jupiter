# 0021. Live mode goes over the card network, with installments, stored credentials and clearing

- Status: Accepted
- Date: 2026-09-30

## Context

Until phase 5, test mode paid through the test rail and live mode had no rail. The
network simulator now gives live mode something to talk to. Three card features come with
it. Installments (parcelado), which are how most Brazilian card sales are made. Stored
credentials: a merchant may charge a card without the cardholder only by quoting the
payment in which the cardholder agreed. And clearing, the network's confirmation, the
next day, of what was captured.

## Decision

**Two modes, two rails.** Test mode keeps the test rail: magic amounts, the test payment
methods, no network. Live mode goes through the acquirer connector to the card network
(ADR 0019), and takes only saved cards; the test payment methods are refused there. In
this repository the network is the simulator. The binaries connect to it when
`JUPITER_CARDNET_ADDR` is set, and live payments are refused as `livemode_unsupported`
otherwise.

**Installments are carried, not scheduled.** A payment intent takes `installments` with
a count from 2 to 12 and who finances them. Merchant-financed is parcelado lojista,
without interest to the cardholder; issuer-financed is parcelado emissor. They are BRL
only. The authorization carries them (DE 67 and DE 48), and the attempt records them.
Splitting the receivable into monthly units is phase 9.

**Stored credentials follow the framework.** `setup_future_usage: off_session` marks a
customer-initiated payment that stores the card. When it is approved, the network
transaction id is saved on the payment method. `off_session: true` makes a
merchant-initiated payment, which Jupiter refuses unless the card was stored that way,
and which quotes the stored id. The rule table gives merchant-initiated authorizations
their own, shorter validity.

**Clearing is imported, not trusted blindly.** The worker fetches each closed business
day's clearing file from the network, over https or from this machine, following no
redirects. Each record must agree with what was sent under its RRN: its kind, its
merchant and its network transaction id. Only then is the completion or refund marked
cleared, for its exact amount. All records go in one transaction per file. A day is
imported once: the same file again is a no-op, and a different one is refused. A record
that does not match is kept as an exception rather than failing the file. Exceptions are
tried again on every later import, since a completion can be cleared before Jupiter has
the network's acknowledgement of the capture. What never matches is left to
reconciliation (phase 13).

## Alternatives rejected

- **The simulator as the test-mode rail.** Integrators need test mode to be instant,
  deterministic and reachable without the simulator running. Live mode is where the
  network's realism belongs.
- **Installments as a payment method option, as Stripe models them for Mexico.** The
  count and the financing are properties of the payment in Brazil, not of the card.
- **Letting merchants send the network transaction id themselves.** It would let any
  merchant claim an agreement it never obtained.

## Consequences

- Clearing does not yet touch the ledger: captured money moves to the merchant on
  settlement (phase 11), and clearing only confirms it.
- Live mode has no 3-D Secure step until phase 6; the authentication-required test card
  is a test-mode feature.
- Jupiter records that a card was stored for off-session use, not the cardholder's
  mandate behind it (amounts, frequency). A merchant with a stored card can charge it
  off-session for any amount; recording and enforcing mandates is not in this plan.
