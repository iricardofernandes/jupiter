# 0039. Three-way reconciliation by streams, matched exactly or broken

- Status: Accepted
- Date: 2026-10-01

## Context

Every money movement is recorded three times: in the ledger, in a rail's record and in a
bank's statement. Each recording can be lost, repeated or late. Until now each connector
handled only its own mismatches:
- the clearing import kept the records it could not match as exceptions;
- the CNAB import kept the returns it could not apply;
- the registry reconciliation kept its divergences.

Nothing compared the three. Nothing looked at the statements, and nothing aged what did
not match. The research found no published three-way reconciliation algorithm for a PSP
([research](../research/payments-market.md#gaps-and-open-questions)).

## Decision

Reconciliation is a module of its own (`internal/reconciliation`), and it only reports: it
never moves money. The algorithm is in [docs/reconciliation.md](../reconciliation.md).

**Streams.** Each counterparty's record is a stream with two sides:
- **Jupiter's side**, provided by the module that owns the movements: payments,
  receivables, the acquirer, the bank. It is read from what the ledger booked.
- **The counterparty's side**, provided by its connector: a stored file, a grade, a
  statement.

Both sides are reduced to the same record: identity, key, direction, amount and value
date. The engine knows nothing of rails. A counterparty added later is a new stream.

**Matching** is exact by key, amount and direction. Only that resolves anything without a
person. Everything else becomes a break of a named kind with a score and its reasons:
- an amount mismatch;
- a duplicate;
- a probable match within two days;
- a record missing on one side.

Dates decide only when an unmatched record becomes a break: once the day reconciled
reaches it. A late record is therefore a break the day it is missing, resolved the day it
arrives.

**The queue.** A break is open until:
- its record matches;
- it is superseded by another break;
- its divergence is fixed at the counterparty;
- an operator resolves it, naming themself and why.

Reports age the breaks per counterparty, and per merchant through the API.

**Records are kept.** Every record read, with what it matched, is stored. A clearing file
or a return file's records are kept at import, line by line, so a duplicate line is a
record of its own. Reading a day again changes nothing.

**Simulators can lie.** Each one's records can be lost, duplicated or delayed by a fault,
which is how the tests prove that every injected break is found and no false one is:
- the bank's statement lines and return records;
- the Pix bank's statement lines;
- the card network's clearing records.

## Alternatives rejected

- **Reconciling balances instead of movements.** A balance that differs says that
  something is wrong, but not what. Movements say which one, and balances can be added on
  top.
- **Fuzzy matching that resolves on its own.** Two Pix of the same amount on the same day
  are common; resolving them on a score would hide the break it guesses wrong. Probable
  matches wait for a person.
- **One connector-specific reconciliation per rail**, as the clearing and CNAB imports had.
  Each would invent its own queue and ageing; the merchant would have no single report.

## Consequences

- The golden path reconciles every counterparty and ends with no break.
- A stream's Jupiter side must be keyed as the counterparty keys it. The Pix and bank
  connectors already named transfers and returns by Jupiter's ids, which made that so.
- What no counterparty reports is not reconciled: the networks netting refunds and
  chargebacks out of later settlements, and the balances of the accounts.
- The SLC now credits Jupiter's account at its bank (sim-slc's credit hook), so a
  settlement can be matched the three ways.
