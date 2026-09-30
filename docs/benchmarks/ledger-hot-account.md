# Ledger: concurrent postings into one hot account

## What is measured

64 writers post concurrently, 250 postings each (16,000 in total). Each writer moves
R$ 0.10 from its own wallet (credit-normal, non-negative, synchronous balance) into one
shared platform account, the way every payment's fee or settlement lands on the same
platform account. Each posting is one database transaction, with no retry: a deadlock or
serialization failure would fail the run.

The run is repeated with the platform account in both modes (ADR 0008):

- **synchronous**: every posting updates the platform account's cached balance row, so
  every writer queues on that one row lock;
- **batched**: every posting inserts a delta into `ledger.balance_queue` instead, and
  the balance applier folds the queue into the cached balance afterwards.

After each run the test drains the queue and asserts that no update was lost: the
platform account holds exactly 16,000 × R$ 0.10, every wallet is back to zero, and the
invariant checker reports nothing.

Throughput is postings committed per second of wall time; latency is per posting, from
`BEGIN` to `COMMIT` returning.

## How to reproduce

```sh
make bench-ledger
```

It runs `TestConcurrentWritersToAHotAccount` in `internal/ledger` with
`JUPITER_LEDGER_BENCH=1`, against PostgreSQL in a testcontainers container with the
image's default configuration (`fsync` and `synchronous_commit` on).

## 2026-09-30

Environment: AMD Ryzen 7 9800X3D (8 cores, 16 threads), 30 GB RAM, Arch Linux (kernel
7.1.9), Docker 29.7.2 with overlayfs, PostgreSQL 18.6 (`postgres:18.6-alpine`), Go 1.27.1.
The database and the test process shared the machine.

| Run | Mode | Wall time | Postings/s | p50 | p99 | Max |
|---|---|---|---|---|---|---|
| 1 | batched | 994 ms | 16,101 | 3.47 ms | 9.63 ms | 51.74 ms |
| 1 | synchronous | 4.376 s | 3,657 | 10.62 ms | 85.62 ms | 203.28 ms |
| 2 | batched | 1.004 s | 15,936 | 3.65 ms | 9.23 ms | 60.00 ms |
| 2 | synchronous | 4.479 s | 3,572 | 10.53 ms | 93.90 ms | 259.83 ms |
| 3 | batched | 967 ms | 16,548 | 3.48 ms | 8.84 ms | 55.38 ms |
| 3 | synchronous | 4.445 s | 3,600 | 10.54 ms | 93.18 ms | 205.93 ms |

Batched updates carried about **4.5× the throughput** of synchronous ones on the same
hot account (≈16,200 against ≈3,600 postings/s), with p99 latency about **10× lower**
(≈9 ms against ≈90 ms). No run had a deadlock, a serialization failure or a lost update.

### What these numbers do not say

- They are one machine, with the database local and untuned. They compare two designs
  under identical conditions; they are not a capacity figure for a deployment.
- The synchronous figure is bounded by one row lock held for the length of a commit,
  so it will scale with commit latency, not with cores. The batched figure is bounded
  by the wallets' own rows and by commit throughput.
- The time the applier takes to drain the queue is not included; the cached balance of
  a batched account lags its entries by up to one applier interval (200 ms in the
  worker), while `Ledger.Balance` stays exact by adding queued deltas at read time.
