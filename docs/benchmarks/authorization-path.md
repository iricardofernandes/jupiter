# The authorization path, to saturation

## What is measured

A merchant authorizes a card payment: `POST /v1/payment_intents` with a saved card,
`confirm: true` and `capture_method: manual`, over HTTP. The request goes through the
whole path:
- the API's idempotency phases;
- the risk engine's decision;
- the vault, for the card number;
- ISO 8583 over TCP to the card network simulator, whose issuer approves;
- a pending transfer (the hold) in the ledger;
- the payment's events.

A request counts when it answers `200` with the intent in `requires_capture`. Latency is
measured at the client, from sending the request to reading the whole answer.

The load is live mode, with each payment on a card of its own, so the risk engine's
velocity rules see ordinary traffic:
- each level saves enough cards that none is used more often than the platform's rules
  allow in an hour;
- the clock moves two hours between levels.

Each level makes 200 payments to warm up, then 2,000 measured ones, at a fixed number of
concurrent clients from 1 to 256. Two configurations:
- **one merchant**: every payment at the same merchant, the hardest case for anything
  locked per merchant;
- **eight merchants**: payments spread over eight, as a platform's traffic is.

The API, the vault, the card network simulator and the clients run in one Go process;
PostgreSQL runs in a container (testcontainers), with the image's default configuration
(`fsync` and `synchronous_commit` on). The API's pool has pgx's default size: 16
connections on this machine.

## How to reproduce

```sh
make bench-authorization
```

It runs `TestAuthorizationPath` in `test/load`. These variables change it:

| Variable | Effect |
|---|---|
| `LOAD_LEVELS=8,32` | Only those concurrency levels |
| `LOAD_POOL_CONNS=64` | The API's pool size |
| `LOAD_DIAGNOSE=1` | Per level: commits and statements per payment, and what the database's sessions wait on |
| `LOAD_PROFILE_DIR=dir` | CPU, block and mutex profiles of the run |

## Environment

- AMD Ryzen 7 9800X3D (8 cores, 16 threads), 30 GB RAM, NVMe SSD (Kingston SNV3S2000G).
- Arch Linux, kernel 7.1.9, Docker 29.7.2 with overlayfs.
- PostgreSQL 18.6 (`postgres:18.6-alpine`), Go 1.27.1.
- The database and the test process shared the machine. Other, idle containers ran
  beside them.

## 2026-10-01: profiling, and what it changed

The first sweep found two ceilings:
- **eight merchants** stopped at about 2,300 payments a second;
- **one merchant** stopped at about 1,340, and its latency grew with every client
  added.

The CPU profile showed the Go process using about 1.5 of 16 cores. The block profile
showed that a quarter of the time spent waiting was spent waiting for a database
connection, which put the limit in the database. A pool of 32 or 64 connections did not
change the throughput: each payment just held its connections longer.

Sampling `pg_stat_activity` every 5 ms during a level showed what the busy sessions
waited on. A pgx tracer counted the statements each payment sent: 51 statements, in 4
transactions, with about 10 commits counted by PostgreSQL. Three changes followed, each
measured alone with the full sweep ([ADR 0040](../adr/0040-measured-capacity-of-the-authorization-path.md)).

**1. One merchant's payments queued on the risk engine's lock.** At one merchant and 32
clients, 12 sessions were waiting on `Lock:advisory` in `LockMerchant`: every decision for
a merchant was serialized until its transaction committed. Decisions now lock their card
and their address, and lock the merchant only when it is near or under the card-testing
throttle.

**2. Then on the merchant's balance row.** With the risk lock gone, 9 sessions waited on
`Lock:tuple` and `transactionid` while inserting ledger entries: every hold updated the
merchant's cached balance row synchronously. The merchant balance is now a batched
account (ADR 0008).

**3. Round trips.** Each statement is a round trip to the database, and the counts showed
three that repeated themselves:
- the payment method was read 4 times;
- the subscribers of each of 3 events were looked up apart from the event;
- the two ledger accounts were read in two queries.

A payment now sends 44 statements.

Throughput (payments a second) and p99, before and after each change:

| Clients | Before | After 1 | After 2 | After 3 |
|---|---|---|---|---|
| **One merchant** | | | | |
| 1 | 411 · 3.08 ms | 405 · 3.28 ms | 394 · 3.85 ms | 417 · 3.62 ms |
| 8 | 1,343 · 7.16 ms | 1,568 · 8.96 ms | 1,978 · 4.85 ms | 2,087 · 4.81 ms |
| 16 | 1,329 · 14.61 ms | 1,514 · 108.4 ms | 2,327 · 10.43 ms | 2,230 · 10.00 ms |
| 32 | 1,285 · 36.76 ms | 1,466 · 115.93 ms | 2,124 · 20.6 ms | 2,334 · 16.88 ms |
| 64 | 1,263 · 66.2 ms | 1,327 · 113.23 ms | 2,244 · 34.43 ms | 2,249 · 34.18 ms |
| 128 | 1,099 · 172.31 ms | 1,346 · 156.15 ms | 2,207 · 67.46 ms | 2,168 · 70.05 ms |
| 256 | 853 · 403.85 ms | 1,289 · 240.14 ms | 2,218 · 129.65 ms | 2,119 · 138.64 ms |
| **Eight merchants** | | | | |
| 1 | 424 · 2.95 ms | 407 · 3.96 ms | 429 · 3.06 ms | 392 · 3.89 ms |
| 8 | 1,988 · 5.4 ms | 1,979 · 5.06 ms | 2,066 · 4.72 ms | 2,077 · 5.02 ms |
| 16 | 2,267 · 9.29 ms | 2,320 · 9.37 ms | 2,362 · 9.2 ms | 2,359 · 9.23 ms |
| 32 | 2,311 · 17.52 ms | 2,265 · 17.26 ms | 2,322 · 16.84 ms | 2,335 · 20.72 ms |
| 64 | 2,306 · 32.81 ms | 2,224 · 33.34 ms | 2,349 · 30.83 ms | 1,894 · 74.42 ms |
| 128 | 2,293 · 63.11 ms | 2,207 · 69.16 ms | 2,252 · 65.38 ms | 2,270 · 68.36 ms |
| 256 | 2,231 · 128.91 ms | 2,024 · 162.71 ms | 2,289 · 124.41 ms | 2,403 · 119.64 ms |

- **Change 1** raised one merchant's peak from 1,343 to 1,568, but its tail got worse:
  more decisions in flight met at the balance row.
- **Change 2** brought one merchant to the platform's ceiling, and cut its p99 at 256
  clients from 404 ms to 130 ms.
- **Change 3** does not show in single runs: one run differs from the next by about 5%,
  as the eight-merchant row at 64 clients shows. Six runs of each at one client measured
  it:

| One client, eight merchants, 6 runs each | p50 | Payments/s |
|---|---|---|
| After 2 | 2.40 ms | 404 |
| After 3 | 2.32 ms (−3%) | 420 (+4%) |

## 2026-10-01: the path as it stands, to saturation

The full sweep with all three changes, and with the API's metrics on, on the machine
above with nothing else running.

**One merchant**

| Clients | Payments/s | p50 | p95 | p99 | Max | Errors |
|---|---|---|---|---|---|---|
| 1 | 407 | 2.38 ms | 3.16 ms | 3.84 ms | 7.64 ms | 0 |
| 2 | 755 | 2.6 ms | 3.13 ms | 3.43 ms | 6.44 ms | 0 |
| 4 | 1,364 | 2.88 ms | 3.47 ms | 4.06 ms | 7.35 ms | 0 |
| 8 | 2,123 | 3.69 ms | 4.31 ms | 4.66 ms | 7.62 ms | 0 |
| 16 | 2,447 | 6.41 ms | 7.71 ms | 9.26 ms | 11.91 ms | 0 |
| 32 | 2,404 | 13.21 ms | 15.18 ms | 16.29 ms | 18.13 ms | 0 |
| 64 | 2,241 | 27.07 ms | 40.06 ms | 50.77 ms | 59.3 ms | 0 |
| 128 | 2,323 | 54.08 ms | 59.45 ms | 63.85 ms | 68.67 ms | 0 |
| 256 | 2,072 | 115.95 ms | 139.47 ms | 153.89 ms | 159.12 ms | 0 |

**Eight merchants**

| Clients | Payments/s | p50 | p95 | p99 | Max | Errors |
|---|---|---|---|---|---|---|
| 1 | 402 | 2.43 ms | 3.11 ms | 3.71 ms | 7.84 ms | 0 |
| 2 | 768 | 2.53 ms | 3.33 ms | 4.15 ms | 5.44 ms | 0 |
| 4 | 1,394 | 2.84 ms | 3.31 ms | 3.79 ms | 4.66 ms | 0 |
| 8 | 2,013 | 3.81 ms | 4.92 ms | 8.23 ms | 11.17 ms | 0 |
| 16 | 2,187 | 6.89 ms | 10.55 ms | 15.22 ms | 20.36 ms | 0 |
| 32 | 2,224 | 14.06 ms | 17.14 ms | 19.23 ms | 21.07 ms | 0 |
| 64 | 2,304 | 27.59 ms | 30.16 ms | 32.05 ms | 36.14 ms | 0 |
| 128 | 2,206 | 57.2 ms | 63.51 ms | 65.52 ms | 71.01 ms | 0 |
| 256 | 2,334 | 107.81 ms | 120.21 ms | 125.31 ms | 140.5 ms | 0 |

### The saturation point

Throughput stops growing at **16 concurrent clients, about 2,200 to 2,450 authorizations a
second**, whether the payments are at one merchant or spread over eight. From there,
each client added waits in line: latency grows in proportion and throughput stays flat.
At 256 clients the p99 is about 150 ms and nothing fails.

What saturates is PostgreSQL's write path, not Jupiter's process. The Go process used
about 1.5 of its 16 cores. A larger connection pool did not help. The busy sessions
wait on:
- the WAL at commit (`LWLock:WALWrite`, `WALInsert`);
- the `ledger.entries` index (`LWLock:BufferContent`).

### What these numbers do not say

- They are one machine, with the database local, in a container, and untuned. They are
  not a deployment's capacity: a dedicated database, faster disks for the WAL, or
  `commit_delay` would move them.
- The card network is a simulator answering in microseconds. A real network answers in
  tens or hundreds of milliseconds. Each authorization would then hold its client
  longer, so the same throughput would need more concurrent requests, which a real
  deployment has.
- Captures, refunds and Pix are other paths with other costs. The ledger's own capacity
  is in [ledger-saturation.md](ledger-saturation.md).
