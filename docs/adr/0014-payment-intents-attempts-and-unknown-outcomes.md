# 0014. Payment intents, attempts, and unknown outcomes as a state

- Status: Accepted
- Date: 2026-09-30

## Context

A payment talks to a counterparty that can fail in the worst way: it may do what was
asked and never say so. Every engineering writeup the research found makes the same
point: a timeout is neither a success nor a failure; record the intent before the call
and resolve the outcome afterwards ([research: engineering writeups](../research/notes/06-engineering-writeups.md)).
Stripe's PaymentIntent is the API grammar merchants already know
([research: PSP API design](../research/notes/05-psp-api-design.md)).

## Decision

**Intents and attempts.** A payment intent has Stripe's statuses. Each try at paying it is
an attempt; a decline ends the attempt and returns the intent to
`requires_payment_method`, keeping the attempt as history. The allowed transitions are one
table in `internal/payments/states.go`; `setStatus` is the only code that changes a status,
checks the table and publishes the matching event in the same transaction.

**A rail port.** Every payment method implements `Rail`: authorize, capture, void, refund
and query. Each call carries an idempotency key derived from the attempt (`pa_…`,
`pa_…:capture`, `pa_…:void`) or the refund (`re_…`), so repeating a call is always safe.
Query asks what became of the request sent with a key.

**Record, call, apply.** Each rail operation is three steps: an atomic `Start…` records
what is about to happen (the attempt in `authorizing`, the capture amount, the refund in
`pending`); the rail is called outside any transaction; an atomic `Finish…` applies the
answer only if the attempt is still waiting for it. The API runs these as the phases of
an idempotent request (ADR 0012), so a crash between them is finished by the completer.

**Unknown is a state.** A lost answer leaves the attempt `authorization_unknown`,
`capture_unknown` or `void_unknown` and the intent in `processing`. The resolver
(worker, every 15 seconds) asks the rail about each operation in flight for more than a
minute: an answer is applied; a request the rail never received is sent again with the
same key; `pending` waits. An authorization still unknown after 15 minutes is reversed.
The void names the authorization's key, so the rail reverses it whether it approved it,
is still processing it, or has not received it yet, and a late approval can never
happen; only once the rail confirms the reversal is the attempt failed, so the customer
can pay again without being charged twice. Captures, voids and refunds are never
abandoned, because repeating them is safe and the money must land.

Captures, voids and refunds name the authorization they act on, and the rail checks them
against it: a capture of an authorization that was reversed, or larger than it, and
refunds beyond what was captured, are refused. A capture whose answer arrives after the
ledger's backstop released its hold is posted on its own, and never twice: the hold's
resolution is checked first.

**The ledger mirrors the rail.** Authorization places a hold from the network
receivable to the merchant's balance; capture posts it, partially if asked, releasing the
rest; void and expiry void it; a refund is a posted transaction back from the merchant to
the network receivable. Uncaptured authorizations expire after seven days (a constant
until the rule table of phase 5, ADR 0006); the ledger's own hold expiry is a day later,
as a backstop.

**Consistency is checked.** `payments.Check` verifies, in one snapshot, that every
intent agrees with its latest attempt and its refunds, and that each merchant's ledger
balance holds exactly what the payments say: posted, received less refunded; pending,
what open authorizations hold.

## Alternatives rejected

- **Temporal (or Cadence) for the payment workflow.** Durable execution would handle
  retries and timeouts, and Uber and Coinbase use it. But the state that matters, the
  attempt and its unknown outcome, would live in Temporal's history rather than in the
  tables the ledger reconciles against; every transition would need a second system to
  be up; and the recovery logic this project exists to show would be hidden in a
  framework. River jobs and explicit state machines keep the state in PostgreSQL next to
  the money.
- **Treating a timeout as a decline.** The customer would retry, and the first
  authorization, which may have succeeded, would sit on their card until it expired.
- **A status column updated freely.** Without one transition table and one function
  that applies it, forbidden transitions creep in through each new code path.
- **Refunds as ledger reversals.** The plan mapped refunds to reversal transactions, but
  a reversal cancels a whole transaction exactly (ADR 0009) and refunds are often
  partial. A refund is a posted transaction of its own, linked to the payment through the
  refund record.
- **Multi-leg holds for automatic capture with split.** See ADR 0009: splits will post
  from a clearing account after capture.

## Consequences

- An intent can sit in `processing` for up to about 15 minutes after a lost answer; the
  webhook tells the merchant when it settles.
- The rail's own memory of each key (the test rail keeps it in a table) is what makes
  recovery safe; a real connector must provide the same, as phase 5's ISO 8583 connector
  will with STAN and RRN matching.
- Payment methods are test tokens (`pm_card_visa`, …) until the vault (phase 4) accepts
  test card numbers.
