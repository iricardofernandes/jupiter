# 0040. Risk decisions lock per card, and merchant balances are batched

- Status: Accepted. Amends [ADR 0022](0022-risk-engine-rules-velocity-card-testing.md) on how
  risk decisions are serialized, and [ADR 0008](0008-hot-accounts-with-batched-balances.md)
  on which accounts are batched.
- Date: 2026-10-01

## Context

Phase 14 load-tested the authorization path end to end
([benchmark](../benchmarks/authorization-path.md)): a merchant's `POST
/v1/payment_intents`, through the risk engine, the vault and ISO 8583 to the card network
simulator, to a hold in the ledger. It measured two configurations:
- every payment at one merchant;
- payments spread over eight merchants.

Eight merchants reached about 2,300 authorizations a second. One merchant stopped at about
1,300, and its p99 grew with every client added. Sampling `pg_stat_activity` showed why.
Twelve of the busy sessions waited on one advisory lock, the risk engine's lock per
merchant (ADR 0022). Each decision held it until its transaction committed, so a
merchant's payments were decided strictly one at a time.

Removing that lock moved the queue to the next row every payment of a merchant touches:
its balance in the ledger. A hold credits `merchant_balance` as pending, and a
synchronous account updates its cached balance row in the posting's transaction (ADR
0008). Sessions now waited on `Lock:tuple` while inserting the entries.

## Decision

**Risk decisions lock what they count.** A decision takes a transaction-scoped advisory
lock on its card, and on its address at the merchant when there is one, so velocity per
card and per address still sees every attempt before it. The merchant's lock is taken
too, but only when the merchant is under watch:
- its declines in the last ten minutes run at the card-testing ratio; or
- it is throttled.

Under watch the throttle starts on the attempt that crosses it, and its rate counts every
attempt. Ordinary traffic decides in parallel, so its merchant-wide counts can be off by
the attempts in flight. Locks are always taken in the same order (card, address,
merchant), so they cannot deadlock.

**Merchant balances are batched.** `merchant_balance` is created batched, like the network
receivable: its entries are still written in the posting's transaction, but its cached
balance is updated by the applier. Nothing that guards money relied on that row lock:
- payouts and refunds check funds under their own advisory locks;
- `Ledger.Balance` stays exact by adding the queued deltas when it reads.

Accounts are append-only, so the flag applies to accounts created from now on.

**Fewer round trips.** Three changes cut an authorization from 51 statements to 44:
- a request reads each payment method once;
- an event's subscribers come back from the statement that records the event;
- a card payment's two ledger accounts are read in one query.

## Measured

Each change was measured alone, at 1 to 256 concurrent clients:

| Change | One merchant, peak | p99 at its peak concurrency | Eight merchants, peak |
|---|---|---|---|
| Before | 1,343 /s (8 clients) | 7.16 ms; 404 ms at 256 clients | 2,311 /s |
| Risk locks per card | 1,568 /s (8) | 8.96 ms; 108 ms at 16 clients | 2,320 /s |
| Merchant balance batched | 2,327 /s (16) | 10.43 ms; 130 ms at 256 clients | 2,362 /s |
| Fewer round trips | 2,334 /s (32) | 16.88 ms | 2,403 /s |

At one client, the round trips took the p50 from 2.40 to 2.32 ms. That is about 3%,
consistent over six runs, and within the noise of any single run.

What limits the platform now is PostgreSQL's write path:
- sessions wait on `LWLock:WALWrite` and `WALInsert` at commit;
- inserts wait on `LWLock:BufferContent` on the `ledger.entries` index.

A larger connection pool does not raise it (16, 32 and 64 connections measured the
same).

## Alternatives rejected

- **No lock: velocity counted without serialization.** It would be faster still, but a
  burst of parallel attempts on one stolen card would all see a count of zero. Per card
  is exactly what card velocity needs.
- **Keep the merchant lock and shorten what it covers** by deciding in a transaction of
  its own. It still spans a commit, so it would halve the wait at best.
- **Tuning PostgreSQL** (`synchronous_commit`, `commit_delay`, a larger WAL). That is a
  deployment's choice. The benchmark uses the image's defaults so the numbers compare
  designs, not settings.

## Consequences

- One merchant can use the platform's whole capacity. The p99 at saturation fell from
  404 ms to 130 ms with 256 clients.
- Merchant-wide velocity features are approximate for merchants not under watch, as risk
  scores always are. Card and address velocity stay exact.
- A card-testing burst on many cards, at a merchant not yet under watch, decides in
  parallel until its declines reach the ratio. The throttle can start late by as many
  attempts as are in flight, up to the API's concurrency. Declines were already counted
  only once the rail answered, so the old lock never closed that window. Once throttled,
  the rate is exact again.
- `customer_ip` is the merchant's to send, as before. A request without one is not
  locked or counted by address.
- A merchant's cached balance lags its entries by up to the applier's interval, as the
  network receivable's already did. Reads through `Ledger.Balance` do not lag.
