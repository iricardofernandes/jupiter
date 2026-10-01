# Benchmarks

Each file records one measurement: what was measured, the command that reproduces it,
the hardware and software it ran on, and the raw results. Numbers are never edited after
the fact; a new measurement is a new section with its date.

| File | Measures |
|---|---|
| [`ledger-hot-account.md`](ledger-hot-account.md) | Concurrent postings into one hot account, synchronous versus batched balance updates |
| [`ledger-saturation.md`](ledger-saturation.md) | Postings at rising concurrency until the ledger saturates, into accounts of their own and into one hot account |
| [`authorization-path.md`](authorization-path.md) | A payment's authorization end to end, over HTTP and ISO 8583, to saturation; the profiling that found its bottlenecks, and each fix's before and after |
