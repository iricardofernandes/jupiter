# 0025. Pix goes through a partner bank's API Pix, trusted only once confirmed

- Status: Accepted
- Date: 2026-09-30

## Context

Jupiter is not a Pix participant and connects to no SPI or DICT (the plan's non-goals).
Like most PSPs that accept Pix, it holds an account at a participant bank. It uses the
interface the Banco Central requires every receiving PSP to offer its users: the
**API Pix**. That interface covers charges (`cob`, `cobv`), payload locations, Pix
received, returns and notifications. It is authenticated with OAuth 2.0 over mutual TLS, with
tokens bound to the client certificate (RFC 8705). The specification is public, versioned
and matched to the Manual de Padrões
([research: Pix and boleto specs](../research/notes/03-pix-and-boleto-specs.md) §1-3).

## Decision

**A connector, `internal/pix`, is Jupiter's client of its bank**, one per mode: live
mode's bank and test mode's, a sandbox, configured separately. It implements the payments
module's `PixRail`.

**Types from the specification.** The request and response types are generated from the
official `openapi.yaml` (version 2.10.0, commit and hash recorded in `api/bacen-pix`), by
oapi-codegen, into `pkg/pixapi`. The generator drops the fields of a `oneOf` inside an
`allOf`, which is how the payer, the receiver and a discount are written. An overlay,
kept beside the untouched specification, spells those out; `make generate-check` fails
when the generated code is stale. The simulator uses the same types, so both sides speak
the specification, not each other's guesses.

**Authentication.** Client credentials with the client id and secret over a connection
made with Jupiter's certificate; the token is cached until shortly before it expires and
asked for again, once, on a 401.

**Idempotency without idempotency keys.** The API Pix has none; the client names things
instead. A charge's txid is its attempt's id without the underscore (28 letters and
digits), a return's id its refund's, a transfer's its payout's. A request repeated
after a lost answer names the same thing: the bank refuses a second charge with that txid,
and the connector reads the first back.

**Outcomes.** A 404 is "the bank has no such record". A 400, 410 or 422 to a write is a
refusal. Anything else is an unknown outcome for the resolver, as with the card network: a
timeout, a 5xx, a read's error, and the 4xx that say nothing of the request (401, 403, 408,
409, 429). A refused repeat of a transfer or a return is read back before it counts as a
refusal: the bank may refuse it because the first was made, and the money may have left.

**Notifications are hints.** The bank posts each Pix with a txid, and each final return,
to `{webhook}/pix`. The listener, on its own address, admits only the bank's client
certificate. Even so, the connector applies nothing a notification says: it reads each
Pix back from the bank (`GET /pix/{e2eid}`) and applies that. A forged notification
can make Jupiter look something up, never credit anyone.

**Reconciliation.** Notifications get lost, and a Pix without a txid is never notified.
A worker job reads every Pix received in the last 72 hours (`GET /pix`, paged, up to a page
limit) and applies those it missed. It reads a fixed window, not from a mark, so a Pix
the bank dates out of order is not skipped. A Pix it cannot read is logged and skipped,
not allowed to stop the rest. A Pix is applied once per endToEndId, however it arrives.

**Modes apart.** Live and test mode must use different keys, clients and notification
URLs; the API refuses to start otherwise, as one account for both would give test
payments real charges.

**Payouts** need Pix sent, which the API Pix does not cover. Each bank has its own
interface; the simulator's (`PUT /transferencias/{idEnvio}`) follows the API's
conventions, and the connector is the only code that knows it.

## Alternatives rejected

- **Being a Pix participant.** Out of scope: a participant needs BCB authorization,
  an account at the BCB and the RSFN.
- **Hand-written types.** They would drift from the specification silently; generated
  ones change only when the pinned version does.
- **Trusting notifications.** mTLS authenticates the bank's certificate, not each
  message's content; reading back from the API costs one request per Pix.
- **Polling charges instead of notifications.** One request per waiting payment per
  interval; the period listing covers what notifications miss at a fraction of that.

## Consequences

- Test mode needs a Pix sandbox as live mode needs a bank; without one, the mode has no
  Pix, as without the card network live mode has no cards.
- The transfer interface is the simulator's; a real bank's needs its own adapter behind
  the same `PixRail` methods.
- The Manual de Segurança do SFN, which sets the JWS rules for payload locations, was not
  available: the simulator's choice (PS256, a `jku` on the location's host) is marked
  unverified in its README.
