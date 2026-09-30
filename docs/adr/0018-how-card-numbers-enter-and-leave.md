# 0018. How a card number enters Jupiter, and how it reaches the rail

- Status: Accepted
- Date: 2026-09-30

## Context

A card reaches Jupiter in two ways. A merchant's server can send the number to the API,
which puts that server in PCI DSS scope (SAQ D). A checkout page can send it straight to
the vault, which keeps the merchant's server out, if the page is served in a way that
qualifies ([research: card rails](../research/notes/04-card-rails.md)). Either way,
the API must never store the number, and the idempotency layer (ADR 0012) stores every
request body as received.

The security code may be used for one authorization and never stored after it.

## Decision

**A number is swapped for a token before anything is recorded.** `POST /v1/payment_methods`
with a number runs a `prepare` step before the idempotency layer: it sends the card to the
vault and rewrites the body to `{"type":"card","card":{"token":"tok_…"}}`. That rewritten
body is what the Idempotency-Key stores and fingerprints. The vault's tokenization is
idempotent by a request key derived from the Idempotency-Key, so a retry gets the same
token, the same rewritten body, and the first response. The same key with another card
is refused by the vault as a conflict, which the API reports as
`idempotency_key_mismatch`. A repeated request is answered from the first card before
anything is checked again, so a card that expired since cannot turn a retry into an
error. A request key never expires, so a retry always finds its card, however late; the
price is that reusing an Idempotency-Key for another card, even after Jupiter has
forgotten it, is refused. A
`card[token]` that is not a token is refused before it is recorded, and never repeated
back, since it may be a card number sent in the wrong field.

**A token from a web page is claimed with the publishable key it was made with.** The
vault cannot check a publishable key: keys live in Jupiter's database. It stores the key
with the unclaimed token. When a merchant's server saves the token, the API authenticates
that publishable key and requires it to be the caller's, in the caller's mode, before the
vault hands the token over for good. Unclaimed tokens are deleted after an hour.

**The security code lives in the vault's memory only.** There is no column for it. It is
kept for thirty minutes and handed out once, with the first detokenization; the rail
reads a card only when it has not seen the request before, not for a repeat it already
answered. It is
best effort: a vault restart, a lost answer to that detokenization, or a second payment
with the card in those thirty minutes leaves the authorization without it, which fails
safe.

**The rail detokenizes.** A saved card reaches the rail as a token; the rail asks the vault
for the number just before the call that needs it. If the vault does not answer, the
outcome is unknown and the resolver sends the request again (ADR 0014); the simulation
fails vault calls to prove it.

The exit criterion is a test: a canary number unique to each run goes through every
path (saved by number with and without a key, retried, refused as expired, sent in a
malformed request, sent where a token belongs, declined, sent by a web page and claimed) and a paid card is
authorized, captured and refunded. Then every table of every schema in the API database,
and the logs of the API, the rail and the vault, are scanned for each number, as text and
as hex (`TestTheAPIDatabaseNeverHoldsACardNumber`).

## Alternatives rejected

- **Storing the request body encrypted.** The number would still be in the API database,
  and so would the database's scope.
- **Letting the vault check publishable keys.** It would need Jupiter's merchant data,
  through a replica or a call back into the API, which is the dependency the vault exists
  to avoid.
- **Signed client tokens, as Braintree issues.** Sound, but the merchant's server must call
  Jupiter before the page can take a card. Claiming by publishable key keeps Stripe's
  shape, which the API follows elsewhere (ADR 0011).
- **The vault forwarding authorizations itself.** It would take the API and worker out of
  scope, but the vault would then hold the card network connector, which phase 5 builds
  inside Jupiter. It remains the path to a smaller scope.

## Consequences

- The API and worker handle numbers in memory, and are in scope as systems that transmit
  them; [`docs/pci-scope.md`](../pci-scope.md) says so.
- A merchant that sends numbers to the API is SAQ D. One whose page posts them to the
  vault is SAQ A-EP; a Jupiter-hosted iframe, which would make it SAQ A, is a frontend and
  outside this repository.
- The public route limits each address in process, and holds at most 100,000 security
  codes; an edge in front of it would add its own limits.
