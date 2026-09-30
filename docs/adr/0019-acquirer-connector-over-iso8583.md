# 0019. The acquirer connector: every message recorded, requests reversed, advices repeated

- Status: Accepted
- Date: 2026-09-30

## Context

The card rail talks ISO 8583 to a network that can do what the research warns of: not
answer, answer late, answer twice. Its rules for that are old and clear. A request with
no answer in time is reversed with a 0420 advice. An advice is repeated (0421) until
acknowledged. An answer that arrives after the reversal must be ignored, and the issuer
must process the reversal ([research: card rails](../research/notes/04-card-rails.md)).
ISO 8583 has no message that asks what became of an earlier one, but the payments rail
port (ADR 0014) has Query.

## Decision

`internal/acquirer` implements the rail over a persistent TCP connection, with
moov-io/iso8583-connection matching answers to requests by STAN.

**Record before sending.** Each rail call is a row in `acquirer.exchanges`, keyed by the
rail's idempotency key, holding the STAN, RRN and transmission time it was sent with, in
a transaction that commits before the message leaves. A repeated call finds the row and
answers from it; nothing is sent twice.

**Requests are reversed.** An authorization or refund that gets no answer within the
timeout (10 seconds) is marked `reversing` and declined with `issuer_timeout`. A 0420
naming it in DE 90 goes out at once, and is repeated as 0421, with backoff up to five
minutes, until a 0430 arrives. Declining is final, and the reversal makes it so at the
issuer. A payment that is not answered is therefore never left unknown: the customer can
pay again at once.

**Advices are repeated.** A capture (0220) or void (0400) that gets no answer is not
reversed. It must happen: the capture is repeated as 0221, the void as a 0420, until
acknowledged, and the rail answers Unknown meanwhile; the payments resolver asks again.

**An answer must match its message.** Its type, STAN and RRN must answer what was sent,
and an approval must be for the amount asked. Anything else is treated as no answer. A
connection that closes after the message was written may have closed after the network
processed it, so only a message that never found a connection counts as not sent; that
one is declined with `network_unavailable`, and everything else is reversed.

**Late answers are recorded and ignored.** An answer with no request waiting for it is
matched to its exchange by STAN and RRN. It is kept (`late_response_code`) and changes
nothing, except that a late acknowledgement of a repeat ends the repeating.

**Query answers from the record.** Approved, declined, reversed or still repeating. A row
still `sending` after twice the timeout belonged to a process that stopped mid-request:
Query takes it over as a timeout and reverses it.

**Numbers stay in the vault.** The rail detokenizes the card for the 0100 only.
Reversals, completions and refunds name the authorization by DE 90 and the network
transaction id, so the store-and-forward queue holds no card numbers.

The exit criteria are tests: a late 0110 that arrives after the reversal leaves the
payment declined and the issuer holding nothing (`test/cardrail`), and the simulation
drives thousands of live payments through lost requests, lost answers, late and
duplicate answers, then checks that every hold the issuer keeps is an authorization
Jupiter holds, and that every capture is a completion the network accepted for the same
amount.

## Alternatives rejected

- **Leaving a timed-out authorization unknown and querying later,** as the test rail
  does. ISO 8583 has no query, and waiting leaves the cardholder's credit held and the
  customer unable to pay again. Reversing is what acquirers do.
- **Keeping the exchange state in memory.** A process that stops mid-request would lose
  the knowledge that a reversal is owed. The database row survives it.
- **A message queue for store-and-forward.** The exchanges table already is one, claimed
  with `FOR UPDATE SKIP LOCKED` by whichever worker is free.

## Consequences

- A capture the network never acknowledges stays unknown until it does; the resolver and
  the reversal loop keep trying and nothing gives up on it, since the money was taken.
- Partial approvals are never requested. An issuer that grants one anyway is reversed
  and the payment declined, so no hold is left for an amount nobody will capture.
- The network can refuse a reversal, such as one of a refund it already cleared. The
  exchange is then recorded as refused and logged as an error, for reconciliation.
- STANs come from a database sequence, so every process can send without colliding; the
  API and the worker each keep one connection.
