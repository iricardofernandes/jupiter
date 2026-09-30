# 0012. Idempotency keys with atomic phases and recovery points

- Status: Accepted
- Date: 2026-09-30

## Context

A merchant retries a request whenever it cannot tell whether the first one worked:
a timeout, a dropped connection, a crash on either side. For a PSP, acting twice means
charging twice. The research found Brandur Leach's design the reference: a key table,
atomic phases separated by foreign calls, recovery points, a completer and a reaper
([research: engineering writeups](../research/notes/06-engineering-writeups.md)).
Stripe's v2 semantics add that a failed request can be retried without side effects.

Phase 3 will add operations that call a card network between database writes, so the
mechanism has to cover requests that stop halfway, not only duplicates.

## Decision

Every POST accepts an `Idempotency-Key`. Keys are scoped to merchant and mode.

- **Recording.** The request (operation, path id, body, API version, calling key and its
  scopes) is stored with a fingerprint before any work: SHA-256 of the operation, the
  path id and the canonical JSON of the body, so key order and whitespace do not matter.
  The same key with a different fingerprint is refused with 422.
- **Operations are phases.** Each phase may make one foreign call, which must itself be
  idempotent, followed by one database transaction that commits the phase's effects and
  advances the key's recovery point together. A crash between phases leaves the key at a
  recovery point with every earlier effect committed and nothing of later ones.
- **One runner.** A runner holds a random lock token. Every phase re-checks the token
  under a row lock inside its transaction, so a runner whose lock was taken over cannot
  commit, even if it is merely slow.
- **Concurrent duplicates wait.** A request that finds the key held by a live runner
  waits up to ten seconds for its response, so concurrent duplicates all receive the
  same bytes; past that it gets 409.
- **What is stored.** 2xx and 4xx responses are final and stored, sealed with AES-GCM
  because some carry secrets shown once. 5xx and unexpected failures are not stored: the
  lock is released and a retry resumes from the recovery point.
- **The completer** (worker, every minute) finishes requests that committed at least one
  phase and then stopped, once their lock is older than five minutes. Requests still at
  the first recovery point did nothing, so they are left to the client's retry.
- **The reaper** (worker, hourly) deletes keys older than a day, except unfinished ones
  that committed work.

## Alternatives rejected

- **Caching responses only** (store the response after the handler finishes). It cannot
  tell "never ran" from "ran and crashed before storing", which is exactly the case that
  double-charges.
- **Holding one database transaction across the whole request.** Impossible once a
  foreign call sits in the middle, and it would hold locks for the duration of a network
  call.
- **Answering 409 immediately to concurrent duplicates.** Simpler, but a client that
  retries fast gets an error for a request that is about to succeed.
- **Storing 5xx responses** (Stripe v1). It turns a transient failure into a permanent
  one for that key.
- **Temporal for multi-phase operations.** See ADR 0002: the recovery logic is the part
  worth showing, and it fits in the request's own tables.

## Consequences

- An operation's author decides its phases and must make each foreign call idempotent,
  typically by deriving the downstream idempotency key from this one.
- Lock takeover is time-based: a runner genuinely alive after five minutes loses its
  lock, and the token check rolls its next phase back rather than letting two runners
  commit.
- The completer runs an operation with the calling key's scopes as recorded, so revoking
  a key does not stop a request it already started.
