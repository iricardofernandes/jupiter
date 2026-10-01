# The ledger, to saturation

## What is measured

Writers post concurrently, each posting R$ 0.10 from its own wallet (credit-normal,
non-negative, synchronous balance), in a transaction of its own, with no retry. Three
cases:
- **independent**: into an account of the writer's own, so no two postings share an
  account;
- **hot, batched**: all into one shared account whose balance is batched (ADR 0008), as
  the network receivable and merchant balances are;
- **hot, synchronous**: all into one shared account whose balance row every posting
  updates.

Each level makes about 8,000 postings: 8,000 divided among the writers, and at least 20
each. After each level the test applies the queue and checks that no update was lost:
every balance is exactly what was posted, and the invariant checker reports nothing.

Throughput is postings committed per second of wall time. Latency is per posting, from
`BEGIN` to `COMMIT` returning.

The pool has one connection per writer plus four, up to 64: PostgreSQL's default
`max_connections` is 100. Writers beyond 64 queue for a connection, as a service's
requests would.

## How to reproduce

```sh
make bench-ledger-sweep
```

It runs `TestLedgerSaturation` in `internal/ledger` with `JUPITER_LEDGER_SWEEP=1`.

## Environment

As in [authorization-path.md](authorization-path.md):
- AMD Ryzen 7 9800X3D, 30 GB RAM, NVMe;
- PostgreSQL 18.6 in a container, with default configuration (`fsync` and
  `synchronous_commit` on);
- Go 1.27.1, on the same machine.

## 2026-10-01

**Independent accounts**

| Writers | Postings/s | p50 | p95 | p99 | Max |
|---|---|---|---|---|---|
| 1 | 2,525 | 380 µs | 500 µs | 600 µs | 1.19 ms |
| 2 | 5,227 | 370 µs | 480 µs | 550 µs | 5.07 ms |
| 4 | 8,027 | 440 µs | 630 µs | 780 µs | 71.73 ms |
| 8 | 14,704 | 520 µs | 640 µs | 830 µs | 8.51 ms |
| 16 | 15,370 | 960 µs | 1.54 ms | 2.03 ms | 11.66 ms |
| 32 | 17,474 | 1.67 ms | 2.75 ms | 4.18 ms | 21 ms |
| 64 | 17,471 | 3.08 ms | 5.88 ms | 9.39 ms | 54.49 ms |
| 128 | 17,836 | 6.4 ms | 10.34 ms | 31.37 ms | 42.6 ms |
| 256 | 17,398 | 13.29 ms | 19.11 ms | 48.07 ms | 57.06 ms |

**One hot account, batched**

| Writers | Postings/s | p50 | p95 | p99 | Max |
|---|---|---|---|---|---|
| 1 | 2,553 | 380 µs | 510 µs | 600 µs | 3.39 ms |
| 2 | 5,152 | 370 µs | 500 µs | 560 µs | 5.64 ms |
| 4 | 9,178 | 410 µs | 550 µs | 680 µs | 6.53 ms |
| 8 | 12,838 | 580 µs | 870 µs | 1.07 ms | 7.82 ms |
| 16 | 17,305 | 860 µs | 1.22 ms | 1.86 ms | 13.14 ms |
| 32 | 17,996 | 1.63 ms | 2.56 ms | 3.52 ms | 21.85 ms |
| 64 | 16,974 | 3.28 ms | 6.04 ms | 10.29 ms | 50.89 ms |
| 128 | 14,694 | 7.67 ms | 13.7 ms | 33.09 ms | 64.1 ms |
| 256 | 16,343 | 14.3 ms | 19.99 ms | 45.66 ms | 53.38 ms |

**One hot account, synchronous**

| Writers | Postings/s | p50 | p95 | p99 | Max |
|---|---|---|---|---|---|
| 1 | 2,561 | 380 µs | 470 µs | 560 µs | 3.25 ms |
| 2 | 4,652 | 420 µs | 550 µs | 700 µs | 5.22 ms |
| 4 | 4,495 | 870 µs | 1.32 ms | 1.61 ms | 8.18 ms |
| 8 | 4,419 | 1.16 ms | 4.38 ms | 5.74 ms | 19.14 ms |
| 16 | 4,117 | 1.45 ms | 14.39 ms | 25.06 ms | 70.53 ms |
| 32 | 3,656 | 5.41 ms | 26.95 ms | 43.88 ms | 119.33 ms |
| 64 | 3,115 | 11.28 ms | 65.46 ms | 115.1 ms | 293.52 ms |
| 128 | 3,128 | 31.47 ms | 87.11 ms | 131.33 ms | 249.11 ms |
| 256 | 3,190 | 69.8 ms | 133.85 ms | 185.17 ms | 284.71 ms |

### The saturation points

| Case | Saturates at | Throughput there | p99 there |
|---|---|---|---|
| Independent | 32 writers | about 17,500 /s | 4.2 ms |
| Hot, batched | 16–32 writers | about 17,300–18,000 /s | 1.9–3.5 ms |
| Hot, synchronous | 2 writers | about 4,650 /s | 0.7 ms, then over 100 ms by 64 writers |

- **A batched hot account costs nothing:** the shared account is as fast as no shared
  account at all.
- **A synchronous one saturates at two writers.** Every posting queues on its one row
  lock for the length of a commit, and throughput then falls as writers are added. That
  is why every account all payments touch is batched, merchants' balances included
  since [ADR 0040](../adr/0040-measured-capacity-of-the-authorization-path.md).
- **Past saturation**, from 64 writers, the pool's queue adds latency, and runs vary by
  about 15% from one level to the next. The p99 there is the queue's, not the ledger's.

### What these numbers do not say

As for the authorization path: one machine, an untuned database in a container. They
compare designs under the same conditions. A posting here is two entries. A payment's
postings are larger, and the authorization path makes several per payment, which is why
it saturates far below the ledger alone.
