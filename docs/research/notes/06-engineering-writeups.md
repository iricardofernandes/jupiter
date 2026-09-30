# Public engineering writeups on building payment systems (as of Sept 2026): patterns and lessons for "Jupiter" (Go payments backend)

Scope note: about 19 search and fetch calls. Primary sources (company engineering blogs and official docs) were fetched where possible. The Airbnb Medium post returned HTTP 403, so its details come from search-result metadata and snippets and are marked as such. Dates are publication dates as given on the pages.

## Ledgers: how Stripe, Square, Uber, Modern Treasury, Nubank and TigerBeetle model money

### Takeaway
Every mature payments ledger converges on the same core: an **append-only, immutable log of balanced (zero-sum) double-entry transactions**. Balances are a *derived and cached* view (split into pending and posted), corrections are made with new entries rather than updates, and the ledger doubles as the **correctness oracle** for upstream systems (for example, "clearing accounts must return to zero").

### Cited Findings
**Stripe Ledger (Feb 16, 2024, Ilya Ganelin)**
- Ledger is an immutable, auditable log and Stripe's system of record for financial data. "Transactions previously published into Ledger cannot be deleted or modified." — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- It models producer systems as state machines, where balance movements (events) happen between accounts (states), and money movement is modelled as multi-system "fund flows". — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- **Clearing invariant:** in steady state, intermediate clearing accounts should be empty. "A single missing, late, or incorrect transaction immediately creates a detectable accuracy issue." — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- The Data Quality platform uses three metrics: **Clearing** ("did the fund flow complete correctly?"), **Timeliness** and **Completeness**. These roll up into a single DQ score. — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- Scale: 5 billion events per day, 300M transactions at peak, 99.99% of dollar volume ingested and verified within 4 days, and more than 99.9999% "explainability of money movement". — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- Lesson: "If we instrument Ledger, we indirectly instrument the data-producing pipelines." The goal is to keep imperfections "manageable and bounded", not to eliminate them. — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)

**Square Books (Oct 16, 2019, Łukasz Strzałkowski)**
- Three tables: **Books** (accounts with running balances), **Journal Entries** (one per transaction) and **Book Entries** (lines holding debits as positive and credits as negative amounts, with **monotonic version counters**). — [Square: Books](https://developer.squareup.com/blog/books-an-immutable-double-entry-accounting-database-service/)
- Journal and book entries are append-only. Only the books' balances are mutable. Every transaction must sum to zero. — [Square: Books](https://developer.squareup.com/blog/books-an-immutable-double-entry-accounting-database-service/)
- Other details: Google Cloud Spanner (no app-level sharding), "shelves" for multitenancy, UUIDs instead of monotonic IDs, and cursor shards to avoid timestamp hotspots. About 20 TB of data, run by a 3-person team. Used for payments, refunds, pending balances and payouts. — [Square: Books](https://developer.squareup.com/blog/books-an-immutable-double-entry-accounting-database-service/)

**Uber Gulfstream / LedgerStore ("Zero-Sum by Design: 10 Years of Uber's Payments Platform", Aug 6, 2026)**
- Pre-commit validation enforces that "all entries in any money order sum to zero". Money orders are immutable, and adjustments create new orders. — [Uber: 10 years of payments platform](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- Three core models: **Money Order**, **Ledger** (an entity with several sub-accounts) and an **Entity changelog** (every balance update, used for audit and reconstruction). — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- Architecture: stateless microservices, each owning one lifecycle phase (creation, processing, collection, disbursement), with Kafka as the backbone. **Cadence** is used for synchronous user-session payments on top of the async backbone. Generic order types (Collection, Disbursement, Refund) are shared across all lines of business. — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- Scale: balances for more than 1.2B entities, and total money movement close to 2x gross bookings ($217B annualized GB). — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- LedgerStore (2018) is an immutable, ledger-style database with signing and sealing of data, strongly consistent indexes and automatic tiering. Uber migrated more than a trillion entries (petabytes) from DynamoDB, mainly for cost and to consolidate three storage systems into one. — [Uber: Migrating from DynamoDB to LedgerStore](https://www.uber.com/in/en/blog/migrating-from-dynamodb-to-ledgerstore/); [Uber: LedgerStore indexes](https://www.uber.com/us/en/blog/how-ledgerstore-supports-trillions-of-indexes/)

**Modern Treasury (series "How to Scale a Ledger", Part VI Jan 10, 2023, updated Aug 27, 2025, Matt McNierney)**
- Balances are cached as pending and posted debits and credits. **Current balances** are updated synchronously, because balance locks need them. **Effective-time balances** are updated asynchronously, either by *anchoring* (cached end-of-day balance plus intraday entries summed at read time) or by *resulting balances* (balance stored after every entry: O(1) reads, expensive writes). — [MT: Scale a Ledger Part VI](https://www.moderntreasury.com/journal/how-to-scale-a-ledger-part-vi)
- **Cache drift monitoring:** cached balances are checked regularly against the entries (the source of truth), and reads are automatically disabled for accounts that have drifted. — [MT Part VI](https://www.moderntreasury.com/journal/how-to-scale-a-ledger-part-vi)
- Idempotency keys are stored for 24h. A duplicate key returns the prior response. — [MT Part VI](https://www.moderntreasury.com/journal/how-to-scale-a-ledger-part-vi)
- The series covers: I (why ledger databases), II (mapping events to double-entry primitives), III (transaction models), IV (recording and authorizing), V (immutability guarantees). — [MT Part VI](https://www.moderntreasury.com/journal/how-to-scale-a-ledger-part-vi)

**Nubank**
- Real-time double-entry accounting on **Datomic** is the system of record. Datomic's built-in history lets them replay transactions in order to check balance integrity, and to split databases chronologically when services are split. — [Datomic: Nubank's story](https://www.datomic.com/nubanks-story.html)
- Talks: QCon NY 2017 "Building and scaling a financial system of record" and QCon SF 2017 "Architecting a Modern Financial Institution" (authorization, Kafka, Datomic double-entry). — [QCon NY 2017](https://qconnewyork.com/ny2017/ny2017/presentation/building-and-scaling-financial-system-record.html); [QCon SF 2017](https://qconsf.com/sf2017/sf2017/presentation/architecting-modern-financial-institution.html)

**TigerBeetle (docs, current)**
- Each account has four fields: `debits_pending`, `debits_posted`, `credits_pending` and `credits_posted`. The balance-constraint flags `debits_must_not_exceed_credits` and `credits_must_not_exceed_debits` enforce no-overdraft at the database level. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)
- **Two-phase transfers:** a pending transfer reserves amounts in the `*_pending` fields and is then **posted** (fully or partially), **voided**, or **expires** on a timeout. Posting and voiding create new transfer records, so nothing is mutated. — [TigerBeetle: Two-Phase Transfers](https://docs.tigerbeetle.com/coding/two-phase-transfers/)
- **Linked transfers** (`flags.linked`) make a chain atomic. Multi-debit and multi-credit, currency exchange, balance bounds, rate limiting and correcting transfers are all built from single-debit/single-credit transfers. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)
- The client generates the transfer ID and persists it before submitting, which gives idempotency. A retry returns `exists`. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)

### Inferences
- For Jupiter, the defensible baseline is: `accounts` / `transactions` / `entries` tables, append-only entries, a DB-enforced sum-to-zero check per transaction, cached balances split into pending and posted (available = posted minus pending debits), and reversals made as new transactions. The Square, Uber, TigerBeetle and Modern Treasury writeups all agree on this.
- Copying Stripe's "clearing accounts must reach zero" check as a scheduled job gives Jupiter a cheap, demonstrable correctness signal, and it works well in a portfolio.
- Modern Treasury's "verify cache against entries, disable reads on drift" is a concrete, implementable safety net.

### Gaps
- Coinbase, Monzo, Wise and PayPal ledger internals were not fetched in this pass. No primary ledger-design writeup from them was verified here.

## Hot accounts: techniques and trade-offs

### Takeaway
The known options are: (1) avoid locking the hot account and lock only the low-volume counterparty; (2) batch or buffer postings to the hot entity; (3) serialize everything on one core with large batches (TigerBeetle). Optimistic version locking **starves** on hot accounts.

### Cited Findings
- Optimistic locking uses `lock_version`, which increments on every posting. Clients pass the expected version per account, and a mismatch rolls the whole transaction back (StaleObjectError, then retry). Without `lock_version`, the version is updated asynchronously. Optimistic locking was chosen because reads are not blocked on read-heavy traffic. — [MT: Designing Ledgers with Optimistic Locking (Aug 15, 2021, Andy Qin)](https://www.moderntreasury.com/journal/designing-ledgers-with-optimistic-locking)
- "Version locking is susceptible to hot accounts … if the account_version is incrementing at a fast enough pace, some Transactions will never be able to commit." — [MT: How to Scale a Ledger Part IV (search snippet)](https://www.moderntreasury.com/journal/how-to-scale-a-ledger-part-iv)
- Hot accounts are typically an omnibus cash account or a shared settlement account. The recommended design is to lock only the user-side accounts and let hot-account entries be written asynchronously. An entry is processed synchronously or asynchronously depending on whether locking parameters are passed. — [MT docs: Design a Ledger for Concurrency (search snippet)](https://docs.moderntreasury.com/ledgers/docs/handle-concurrency)
- Uber: expansion into B2B created serialized write bottlenecks on "hot entity" rows. **Specialized batch-write mechanisms** gave a 10x throughput increase. — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- TigerBeetle has no row locks and uses a single-core design because "business transactions often have accounts involved in a high percentage of all transactions". Up to 8,189 events per batch amortize replication cost. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)
- Square uses cursor shards to avoid timestamp hotspots on Spanner. — [Square: Books](https://developer.squareup.com/blog/books-an-immutable-double-entry-accounting-database-service/)

### Inferences
- Jupiter in Go on Postgres could lock user and merchant accounts with `SELECT … FOR UPDATE` or a version check. Platform, fee and settlement accounts can be updated through an async batched "balance applier", such as a per-account goroutine or queue consumer that folds N entries into one UPDATE. Entries are still written synchronously, so the double-entry invariant holds at transaction level while the cached balance of the hot account is eventually consistent.
- Sharded sub-balances (N rows per hot account, summed on read) are a common alternative. **No primary source verified in this pass describes it**, so treat it as folklore.

### Gaps
- No published contention numbers (for example, TPS per hot row on Postgres) were found.

## Idempotency: Stripe, Brandur, Airbnb, Shopify

### Takeaway
Clients generate the idempotency keys. The server persists them together with request params, a lock and **recovery points** that separate atomic local phases from foreign calls. Responses are cached. The retention window is about 24 to 72h.

### Cited Findings
- Stripe (Feb 22, 2017): idempotent endpoints let the client retry on any error until it verifiably succeeds, which resolves ambiguous failures. Clients retry with backoff. — [Stripe: Designing robust and predictable APIs with idempotency](https://stripe.com/blog/idempotency)
- Brandur (Oct 27, 2017): the `idempotency_keys` table has `locked_at`, `request_params`, `recovery_point`, `response_code` and `response_body`. **Atomic phase** is defined as "a set of local state mutations that occur in transactions between foreign state mutations". Recovery points are, for example, `started`, `ride_created`, `charge_created` and `finished`. The design uses SERIALIZABLE isolation, a mismatched-params check and staged background jobs, plus an **enqueuer**, a **completer** (pushes abandoned requests forward) and a **reaper** (deletes keys after about 72h). — [brandur.org: Implementing Stripe-like Idempotency Keys in Postgres](https://brandur.org/idempotency-keys)
- Airbnb (Apr 17, 2019, Jon Chew): "Orpheus" is a general idempotency library used across payments services. Replica lag can cause duplicate execution, so reads and writes go to the primary. The DB is sharded by idempotency key. Airbnb reports five nines of consistency while payment volume doubled. The request has pre-RPC, RPC and post-RPC phases, with retryable and non-retryable error classification. — [Airbnb Tech Blog](https://medium.com/airbnb-engineering/avoiding-double-payments-in-a-distributed-payments-system-2981f6b070bb) (403 on fetch, so details come from search snippets and well-known secondary summaries such as [GeeksforGeeks](https://www.geeksforgeeks.org/system-design/airbnb-idempotency-avoiding-double-payments-in-a-distributed-payments-system/))
- Shopify (Jul 28, 2022, Bart de Water): the idempotency key looks up the steps an attempt has completed and ensures only one request goes to financial partners. Use **ULID over UUIDv4** (50% faster INSERTs). Keep the window at 24h or less. — [Shopify: 10 Tips for Building Resilient Payment Systems](https://shopify.engineering/building-resilient-payment-systems); [Shopify: Resilient GraphQL APIs using idempotency](https://shopify.engineering/building-resilient-graphql-apis-using-idempotency)
- TigerBeetle: the client generates and persists the transfer ID before sending it, and a replay returns `exists`. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)

### Inferences
- Jupiter should use the Brandur design close to verbatim: an `idempotency_keys` table with recovery points, one Postgres transaction per atomic phase, and the PSP call between phases. Pass Jupiter's own deterministic key downstream to the PSP (idempotency propagates end to end).
- For the ID type, use ULID or UUIDv7 (time-ordered) for keys and IDs, following Shopify's measured INSERT gain.

### Gaps
- The exact phase names in Airbnb Orpheus could not be verified against the original post because of the 403.

## Unknown outcomes, reconciliation, orchestration (sagas and workflows, Temporal and Cadence)

### Takeaway
Treat any timeout or 5xx from a PSP as **UNKNOWN**, never as failed. Resolve it by retrying idempotently, querying status, or using a completer or reconciliation job. Durable workflow engines (Cadence at Uber, Temporal at Coinbase) are an accepted way to orchestrate multi-step payment state machines. Reconciliation against partner records is a first-class system.

### Cited Findings
- Brandur: foreign mutations (a Stripe charge, an email) are irreversible. Record intent before the call and accept whatever result comes back. A **completer** process drives abandoned requests to a terminal state. — [brandur.org](https://brandur.org/idempotency-keys)
- Shopify: the timeouts are 1s open and 5s read/write (Go has no default timeouts). Circuit breakers (Semian) can be scoped per merchant country. **Distinguish payment failures (expected) from errors (unexpected)** in monitoring. Reconciliation records anomalies when local records diverge from partners' records and remediates automatically where possible. Queues start growing at about 70 to 80% saturation (Little's Law). — [Shopify: 10 Tips](https://shopify.engineering/building-resilient-payment-systems)
- Uber: services own lifecycle phases over Kafka, and **Cadence** is used for synchronous payment flows. Immutable orders plus the entity changelog support self-audit. — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- Coinbase moved its transactional application component by component into Temporal/Cadence workflows. Each crypto transaction resolves to "succeeded" or "failed". — [Temporal: Coinbase case study](https://temporal.io/resources/case-studies/coinbase). In May 2026 Coinbase described a read-only Temporal MCP server for debugging failed workflows in the "payments namespace". — [Temporal blog: How Coinbase debugs Workflows with MCP](https://temporal.io/blog/bringing-temporal-into-your-ai-editor-how-coinbase-debugs-workflows-with-mcp)
- Stripe frames reconciliation as fund flows spanning banks, third-party reporting and regulatory reporting, checked through clearing, timeliness and completeness metrics. — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- TigerBeetle pending transfers with timeouts fit the authorize-then-capture flow natively (reserve, then post or void, or auto-expire). — [TigerBeetle: Two-Phase Transfers](https://docs.tigerbeetle.com/coding/two-phase-transfers/)

### Inferences
- Jupiter's payment state machine should include an explicit `UNKNOWN` or `PENDING_CONFIRMATION` state, a status-poll or webhook resolver and a sweeper (completer). Authorization should map to ledger *pending* entries, capture to *post*, and void or expiry to *void*.
- Temporal's Go SDK would suit a Go portfolio project, with Uber (Cadence) and Coinbase as precedent. A hand-rolled Postgres state machine with an outbox is the lighter option in the Brandur style. Both are defensible. Recommend documenting the trade-off.
- Three-way reconciliation (internal ledger, PSP report, bank statement) is standard practice, but **no primary source fetched here spells out a 3-way algorithm**.

### Gaps
- No Stripe primary writeup on Temporal usage was verified. Airbnb's orchestration posts were not retrieved. No settlement-file matching algorithm was found in the sources fetched.

## Correctness methods: invariants, simulation, TLA+, property-based testing

### Takeaway
The strongest published practice is **continuous invariant checking on the ledger** (Stripe clearing, Uber zero-sum pre-commit, Modern Treasury cache drift) and **deterministic simulation testing** (TigerBeetle VOPR, validated by Jepsen). Public payment-specific TLA+ writeups are scarce.

### Cited Findings
- Stripe: double-entry "gives us a mathematical proof of correctness". Stripe reports a DQ score of 99.99%. — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- Uber enforces zero-sum at pre-commit validation. — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- The TigerBeetle VOPR runs a whole cluster of real code on a single thread and injects network, storage and process faults "at 1000x speed", 24/7 on 1024 cores. It is combined with hash-chaining for corruption detection. — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)
- Jepsen published an independent analysis of TigerBeetle 0.16.11. — [Jepsen: TigerBeetle 0.16.11](https://jepsen.io/analyses/tigerbeetle-0.16.11)
- Datomic history lets Nubank replay transactions to verify balance integrity. — [Datomic: Nubank](https://www.datomic.com/nubanks-story.html)
- Shopify runs load tests with benchmark gateways that mimic production latency. — [Shopify: 10 Tips](https://shopify.engineering/building-resilient-payment-systems)

### Inferences
- A good fit for Jupiter in Go: property-based tests (`testing/quick` or rapid) asserting that sum(entries)=0 per transaction, that balances equal the fold of entries, and that no overdraft occurs on flagged accounts. A deterministic simulated PSP (with injected timeouts, duplicates and late webhooks) and a seeded scheduler give a "mini-VOPR". A nightly invariant job covers zero-sum across all transactions, cached balances against recomputed balances, and clearing accounts at zero.

### Gaps
- No primary payments-company writeup on TLA+ was found in this pass. The AWS formal-methods paper and TigerBeetle's own spec work were not verified here and should not be cited without checking.

## Go in payments companies

### Takeaway
Monzo (the whole bank backend) and Mercado Libre / Mercado Pago (about half of traffic) are the best-documented Go payments shops. The Go-specific public material is about infrastructure, not money-handling patterns.

### Cited Findings
- Monzo's backend is Go microservices on Cassandra (chosen for horizontal scale) and Kubernetes, with a shared core library in every service. — [Monzo: Building a Modern Bank Backend (Sep 19, 2016)](https://monzo.com/blog/2016/09/19/building-a-modern-bank-backend). The service count is about 1,600 as of 2020. — [The Register (Mar 9, 2020)](https://www.theregister.com/2020/03/09/monzo_microservices/). See also [InfoQ talk](https://www.infoq.com/presentations/monzo-microservices).
- Mercado Libre (go.dev case study, Nov 10, 2019): Go powers MercadoPago and core APIs. Core APIs average 8 to 10M requests per minute, with some above 20M. Response times are under 10ms. Servers went from 32 to 4. Test suites got 24x faster. About 50% of traffic runs on Go. — [go.dev: MercadoLibre](https://go.dev/solutions/mercadolibre); [ML Tech: Go toolkit](https://medium.com/mercadolibre-tech/for-devs-by-devs-67a395a03c3f)
- Shopify points out that Go HTTP clients have no default timeouts, so they must be set explicitly. — [Shopify: 10 Tips](https://shopify.engineering/building-resilient-payment-systems)

### Gaps
- Stone, Cash App, Nubank (mostly Clojure) and Wise Go usage were not verified. No GopherCon talk specifically about money handling was located in this pass.

## Performance numbers

### Takeaway
Published numbers are mostly volume figures, not latency SLOs. TigerBeetle states a 1M TPS design target. Mercado Libre reports under 10ms API latency. Stripe publishes ingestion and verification SLAs measured in days, not ms.

### Cited Findings
- TigerBeetle is "designed to handle 1 million transactions per second". — [TigerBeetle docs](https://docs.tigerbeetle.com/single-page/)
- Stripe Ledger: 5B events per day, 300M transactions at peak, and 99.99% of volume verified within 4 days. — [Stripe: Ledger](https://stripe.dev/blog/ledger-stripe-system-for-tracking-and-validating-money-movement)
- Uber: 1.2B+ ledger entities and a 10x hot-entity throughput gain from batching. — [Uber: 10 years](https://www.uber.com/us/en/blog/ubers-payments-platform/)
- Mercado Libre: 8 to 10M requests per minute and under 10ms per request. — [go.dev: MercadoLibre](https://go.dev/solutions/mercadolibre)
- Shopify: suggested timeouts of 1s open and 5s read/write, and queueing starts around 70 to 80% utilization. — [Shopify: 10 Tips](https://shopify.engineering/building-resilient-payment-systems)

### Inferences
- Reasonable Jupiter targets, to state as design goals rather than industry facts: authorization path p99 under 200 to 300ms excluding the PSP, ledger post p99 under 20ms on Postgres, and a PSP call budget of 1s connect and 5s read.

### Gaps
- No primary source found publishing card-authorization p99 SLOs (for example from Stripe or Adyen), or hot-account TPS on relational DBs.
