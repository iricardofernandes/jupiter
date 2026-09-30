# Jupiter — phase plan

Jupiter is a payment service provider backend written in Go: the system behind an API like
Stripe's or Pagar.me's. It accepts payments by card, Pix and boleto, splits them between
recipients, carries them through settlement on a double-entry ledger, and pays merchants out.

Its universal core — payment intents, the ledger, idempotency, split, payouts, disputes and
reconciliation — is what any PSP in any country has. Brazilian rails sit behind adapters:
Pix, boleto and CNAB, card installments and the receivables registry. Every external
counterparty (card network, issuer, 3DS directory, Pix settlement system, banks, the
receivables registry, the centralized settlement system) is a **simulator shipped in this
repository**, speaking the real protocol where the protocol is public.

This repository is the backend only.

The plan is built on [the research](research/payments-market.md). Each phase lists
**deliverables**, **exit criteria** (what must be demonstrably true to move on) and
**non-goals** (what is deliberately left out, so its absence reads as sequencing rather than
omission).

---

## What Jupiter is for

The research reached one conclusion that shapes everything below: **the hard part of a
Brazilian PSP is not accepting a payment, it is the months between capture and payout.**
A credit sale in six installments becomes six receivable units, settling at D+30 through
D+180, each registered at a registry where banks, courts and chargebacks compete for it
under FIFO and LIFO rules with legal deadlines. Since May 2026 every sub-acquirer must also
pay its merchants through centralized settlement.

Global PSP designs stop at the balance ledger. Stripe's Brazilian accounts cannot use Pix
Automático or refund a boleto, and have no receivables or anticipation. Open source covers
ledgers and ISO 8583 well and Brazilian rails barely at all: there is no maintained Go CNAB
library, no maintained Pix settlement simulator, no open 3DS2 access-control server.

So Jupiter demonstrates three things, in this order of importance:

1. **Money correctness under failure.** An append-only, zero-sum ledger whose invariants are
   checked continuously, and a payment core where a timeout is never mistaken for a
   decline.
2. **Protocol fidelity.** Rails implemented against their real specifications — ISO 8583
   over TCP, the BCB Pix API with mTLS and certificate-bound tokens, FEBRABAN CNAB 240 — and
   verified against the specifications' own examples.
3. **A domain global PSPs do not model.** Installment receivables, registry contract
   effects, anticipation and centralized settlement, as first-class parts of the ledger.

## Principles

- **The ledger is the source of truth.** No balance is stored that cannot be recomputed
  from entries, and every money movement in every phase is a ledger transaction.
- **Unknown is a state.** Every call to a counterparty records intent before it is made. A
  timeout leads to a status query, an idempotent retry or a reversal — never to "failed".
- **Every rule that regulation can change is data.** Authorization validity windows,
  settlement terms, dispute deadlines, liability caps and registry deadlines live in
  versioned configuration with an effective date, because the research found several of
  them in conflict or about to change.
- **Simulators are honest about being simulators.** Nothing in Jupiter claims to be licensed,
  certified or connected to a real network. Where a simulator's behaviour rests on an
  unverified claim, its documentation says so.
- **Depth over breadth.** A phase is done when its exit criteria are demonstrable in CI, not
  when its feature list is typed out.

---

## Architecture in one page

Jupiter is a **modular monolith** plus a separately deployed card vault and a set of
simulators. The reasoning is recorded as an ADR in phase 0; in short:

- A payment touches the ledger, the intent, the split and the receivables in one
  transaction. Splitting those across services would buy distributed transactions for no
  benefit a PSP of this size needs.
- Module boundaries are still enforced mechanically: Go `internal/` packages, one public
  interface per module, and an architecture test in CI that fails on a forbidden import.
- **The vault is the one justified exception.** Card data defines PCI DSS scope. Isolating
  the vault as its own process, database and network segment is what keeps the rest of
  Jupiter out of scope, and that is a real architectural reason rather than a stylistic one.
- **Simulators are separate binaries** because they stand for other companies. They share
  no code with Jupiter's domain and talk to it only over the wire protocol a real
  counterparty would use.

```
                 merchants (REST + webhooks)
                          │
                 ┌────────▼─────────┐        ┌──────────────┐
                 │  jupiter api     │───────►│  vault       │  PCI scope
                 │  jupiter worker  │  mTLS  │  (own DB,    │
                 │                  │        │   KMS port)  │
                 │  intents · ledger│        └──────────────┘
                 │  split · payouts │
                 │  receivables     │
                 │  disputes · risk │
                 │  reconciliation  │
                 └──┬───┬───┬───┬───┘
        ISO 8583/TCP│   │   │   │ HTTPS + mTLS, files
      ┌─────────────┘   │   │   └──────────────────────┐
┌─────▼──────┐  ┌───────▼──┐ ┌▼──────────────┐ ┌───────▼──────────┐
│ card       │  │ Pix      │ │ bank          │ │ receivables      │
│ network +  │  │ SPI/DICT │ │ (CNAB 240,    │ │ registry + SLC   │
│ issuer +   │  │ simulator│ │  statements)  │ │ simulator        │
│ 3DS DS/ACS │  └──────────┘ └───────────────┘ └──────────────────┘
└────────────┘                  simulators — separate binaries
```

**Stack.** Go (latest stable) · PostgreSQL · pgx + sqlc · River for jobs enqueued in the
same transaction as the state change · oapi-codegen · moov-io/iso8583 and
iso8583-connection · OpenTelemetry → Collector → Jaeger / Prometheus / Grafana ·
testcontainers-go · Docker Compose locally · GitHub Actions.

**Money.** `int64` minor units with an explicit currency, never floating point. Rates and
percentages use a decimal type and are converted with a named rounding mode. Splitting an
amount uses largest-remainder allocation, so a minor unit is never lost or invented.

---

## The golden path

The deliverable that matters most is one flow that works end to end, runs in CI on every
push, and grows with each phase:

**a customer pays R$ 600.00 by credit card in 6 installments → the issuer simulator
authorizes over ISO 8583 → Jupiter captures → the payment is split between a marketplace
and a seller → six receivable units are registered with the registry simulator → the
seller anticipates two of them → settlement arrives through the SLC simulator → the seller
is paid out by Pix → three-way reconciliation closes with zero breaks.**

`make demo` runs it; the invariant checker must report zero violations at the end; a trace
covers every hop. Phases add their segment and extend the CI job — a phase whose segment is
not in the golden path is not complete.

---

## Milestones

| Milestone | After phase | What can be shown |
|---|---|---|
| **M1 — core** | 3 | A correct ledger, a Stripe-grade API with idempotency and signed webhooks, and a payment state machine that survives timeouts |
| **M2 — portfolio-ready** | 7 | Card payments over ISO 8583 with a vault and 3DS, plus Pix per the BCB spec. The point at which Jupiter is ready to be shown in interviews |
| **M3 — the Brazilian differentiator** | 11 | Receivables, anticipation, split, payouts, centralized settlement and boleto |
| **M4 — production-shaped** | 15 | Disputes, reconciliation, measured performance, a public deployment and a demo |

---

## Phase 0 — Foundation

Repository structure, tooling and the decisions everything else depends on. No domain
behaviour.

**Deliverables**

- Go module layout: `cmd/` (api, worker, vault, one binary per simulator), `internal/` per
  domain module, `pkg/` only for code meant to be imported by others (the CNAB library,
  the BR Code codec).
- `Makefile` with `check`, `test`, `test-e2e`, `up`, `down`, `demo`.
- Lint (golangci-lint with a strict config), `go vet`, `govulncheck`, formatting, and an
  architecture test that fails on a cross-module import outside a module's public
  interface.
- Docker Compose: PostgreSQL, the OpenTelemetry Collector, Jaeger, Prometheus, Grafana.
- `internal/money` (amount, currency, allocation, rounding) and identifier generation
  (prefixed, time-ordered IDs such as `pi_…`, `txn_…`).
- CI on GitHub Actions: lint, unit tests, integration tests with testcontainers-go.
- First ADRs, each with the alternatives rejected: modular monolith with an isolated vault;
  Go and the library set; money as integer minor units; PostgreSQL as the ledger store
  rather than an external ledger database; simulators as separate binaries; configuration
  with effective dates for regulatory rules.

**Exit criteria**

- `make check` passes on a clean clone; `make up` reaches all-healthy from cold.
- The architecture test fails when a deliberate forbidden import is introduced.
- `internal/money` has property-based tests: allocation always sums to the input, and
  mixing currencies is an error, never a coercion.

**Non-goals**

- No ledger, no API, no simulators yet.
- No Kubernetes, no Terraform.

---

## Phase 1 — Ledger

The double-entry ledger every later phase writes to.

**Deliverables**

- Accounts, transactions and entries. Entries are append-only; a transaction's entries sum
  to zero per currency, enforced by the database, not only by Go code.
- Pending and posted balances per account, following TigerBeetle's model: a two-phase
  transfer is created pending, then posted (fully or partially), voided or expired. This is
  the primitive that authorization, capture and void map onto later.
- Corrections by reversal only: a reversal is a new transaction naming the one it undoes.
- Account normal balance and constraints (an account that may not go negative refuses the
  transaction that would make it so).
- Separate books for client funds and Jupiter's own funds, because Brazilian law treats
  payment-account balances as a segregated estate.
- Cached balances with a **drift checker** that recomputes them from entries and refuses
  reads on an account whose cache disagrees.
- A hot-account strategy: user-side accounts are locked synchronously; platform, fee and
  clearing accounts receive entries synchronously but have their cached balance applied by
  a batched updater. Documented in an ADR with the alternatives measured.
- An invariant checker runnable on demand and as a scheduled job: zero-sum, cache versus
  entries, and clearing accounts that must return to zero.

**Exit criteria**

- Property-based tests generate random sequences of transfers, pending posts, voids and
  expiries, and the invariants hold after every one.
- A concurrency test posts to one hot account from many goroutines without deadlock,
  starvation or a lost update, with throughput recorded in `docs/benchmarks/`.
- An attempted `UPDATE` or `DELETE` on an entry fails at the database.

**Non-goals**

- No HTTP API for the ledger; it is internal.
- No multi-currency conversion. Accounts have one currency; FX is out of scope for the
  whole plan.

---

## Phase 2 — API foundation

The public surface every merchant integration will use, before any payment exists.

**Deliverables**

- Merchants and their API keys: secret and publishable keys, separate **test and live
  modes** (`livemode` on every object), keys shown once and stored hashed, scoped
  permissions.
- **Idempotency keys** after Brandur Leach's design: a key table with the request
  fingerprint, a lock, the stored response and recovery points between atomic phases; a
  completer that finishes abandoned requests and a reaper that expires keys. A replay with
  different parameters is refused.
- **Date-named API versions** pinned per merchant, with response transformers so an old
  version keeps its old shape.
- Typed errors with a stable `type` and `code`, a `request_id` and a documentation link.
- Cursor pagination and `include[]` expansion.
- **Events and webhooks:** an event is written in the same transaction as the change that
  caused it; delivery is at least once, signed as `t=…,v1=HMAC-SHA256(t.body)` with a
  5-minute tolerance, retried with backoff, and replayable. Secrets rotate with an overlap
  window. Events are thin: the receiver fetches current state.
- The OpenAPI document as a source file, validated in CI, with the server generated from
  it.

**Exit criteria**

- An end-to-end test sends the same idempotent request concurrently and gets exactly one
  effect and identical responses.
- A test kills the process between atomic phases and the completer finishes the request
  correctly.
- A webhook receiver in the test suite verifies signatures, rejects a replay outside the
  tolerance, and deduplicates by event id.
- A response under an older pinned version keeps its old shape.

**Non-goals**

- No payments yet. No dashboard or UI of any kind.
- No OAuth for third-party platforms; merchants use API keys.

---

## Phase 3 — Payment intents

The universal payment state machine, rail-agnostic, proven with a fake rail before any real
protocol exists. Ends milestone **M1**.

**Deliverables**

- `PaymentIntent` with Stripe's states — `requires_payment_method`,
  `requires_confirmation`, `requires_action`, `processing`, `requires_capture`,
  `succeeded`, `canceled` — and **attempts**: a decline returns the intent to
  `requires_payment_method` and keeps the failed attempt as history.
- An explicit **unknown outcome** on an attempt, resolved by status query, idempotent retry
  or reversal, and a resolver job that drives every unknown attempt to a final state
  within a bounded time.
- A `Rail` port that every payment method implements: authorize, capture (full or
  partial), void, refund, query status. Deterministic idempotency keys are propagated
  downstream.
- Refunds, full and partial, as their own objects with their own lifecycle.
- Ledger mapping: authorization → pending transfer; capture → post; void or expiry → void;
  refund → reversal transaction.
- A **test rail** driven by magic amounts and card numbers (approve, decline, timeout,
  late response, duplicate response) so integrators and tests can reach every state.
- The first cut of the **deterministic simulation harness**: a seeded scheduler that runs
  many concurrent payments against the test rail with injected timeouts, duplicate
  deliveries and process restarts, checking ledger invariants throughout.

**Exit criteria**

- Every state transition in the machine has a test; forbidden transitions are rejected.
- The simulation harness runs 10,000 seeded payments with faults and ends with zero
  invariant violations, and any failing seed replays deterministically.
- The golden path's first segment runs in CI: intent → authorize → capture → ledger.

**Non-goals**

- No real card, Pix or boleto protocol.
- No workflow engine. State machines are explicit Go code driven by River jobs; the ADR
  states why Temporal was not adopted.

---

## Phase 4 — Card vault

Card data enters Jupiter only through an isolated service, so the rest of the system stays
outside PCI DSS scope.

**Deliverables**

- A separate `vault` binary with its own database, reached only over mTLS from the API.
- Tokenization: a card number becomes a random token. Each record is encrypted with
  AES-256-GCM under its own data key, wrapped by a key-encryption key behind a `KMS` port
  (a local implementation for development; the port is what a cloud KMS would implement).
- A keyed fingerprint that identifies the same card across tokens without being
  correlatable with the stored truncated form; display as first-6 or first-8 and last-4.
- Security code held in memory only for the one authorization that uses it, never stored.
- Key rotation: re-wrapping data keys under a new key-encryption key without downtime.
- `docs/pci-scope.md`: what is in scope, what is not and why, and which merchant
  questionnaire (SAQ A) a hosted-fields integration would let a merchant use.

**Exit criteria**

- A test proves the API database never contains a full card number: a canary PAN is
  tokenized and the API database is scanned for it.
- Key rotation completes while tokenization and detokenization continue under load.
- The vault refuses any caller without the API's client certificate.

**Non-goals**

- No real HSM; its operations sit behind an interface with a software implementation.
- No hosted-fields frontend. The vault exposes the endpoint such a frontend would call.

---

## Phase 5 — Card network and issuer simulator

The card rail over real ISO 8583, against a simulated network and issuer.

**Deliverables**

- `sim-card-network`: a network and issuer simulator built on moov-io/iso8583 and
  iso8583-connection. It handles 0100/0110 authorization, 0200/0210 financial, 0400/0420
  reversal and 0800/0810 network management, with test card ranges that decline, time
  out, respond late, respond twice, or approve a partial amount. Issuer stand-in when the
  "issuer" is unavailable.
- Jupiter's acquirer connector: request/response matching by STAN and RRN, timeouts,
  automatic 0420 reversal on timeout, and correct handling of a **late 0110 that arrives
  after the reversal was sent**.
- Installments carried on the authorization: count and who finances them
  (`merchant`/`issuer`).
- Customer- and merchant-initiated transactions: the first customer-initiated payment's
  network transaction id is stored and sent on every merchant-initiated one.
- Authorization expiry computed from a versioned rule table (scheme, card present or not,
  customer or merchant initiated, estimated or final). The research found Visa's own
  framework and Stripe's documentation disagreeing, so the table is data.
- A clearing file produced by the simulator each simulated day, consumed by Jupiter to
  move captured payments to cleared.

**Exit criteria**

- The golden path runs over TCP with ISO 8583: authorize with 6 installments, capture,
  clearing.
- The simulation harness injects network timeouts and late responses; no payment ends
  captured without an approved authorization, and no reversal leaves a pending hold behind.
- The ISO 8583 message specification Jupiter uses is documented field by field, with each
  field marked as sourced or unverified.

**Non-goals**

- No real scheme certification and no scheme-proprietary fields.
- No card-present (terminal, EMV chip) flows; the simulator treats every payment as card
  not present.

---

## Phase 6 — 3DS2, network tokens and risk rules

Authentication, tokenization and the first risk decisions around the card rail.

**Deliverables**

- A 3DS2 simulator: a directory server and access control server exchanging AReq/ARes,
  CReq/CRes and RReq, with frictionless and challenge outcomes, and an authentication
  value that Jupiter forwards on the authorization. The protocol details the research could
  not source from EMVCo are marked unverified in the simulator's README.
- `requires_action` driven by a 3DS challenge, with `next_action` returned to the merchant.
- A network token service simulator: provisioning a token for a vaulted card, per-payment
  cryptograms, lifecycle updates when a card is replaced.
- A risk engine, version one: a rules language over payment attributes, velocity counters
  over sliding windows (per card fingerprint, per IP, per merchant), allow and block lists,
  and a decision (`allow`, `review`, `block`, `request_3ds`) recorded on the attempt with
  the rules that fired.
- Card-testing detection: an enumeration ratio per merchant, with automatic throttling.

**Exit criteria**

- The golden path includes a frictionless 3DS authentication; a separate CI scenario
  completes a challenge.
- A card-testing burst from the load generator is detected and throttled, and the
  decision log explains every block.

**Non-goals**

- No machine-learning model. The rules engine records features and outcomes so that a
  model could be trained later; training one on synthetic data would prove nothing.

---

## Phase 7 — Pix

Pix per the Banco Central specification, against a simulated settlement system. Ends
milestone **M2**.

**Deliverables**

- Types generated from the official `bacen/pix-api` OpenAPI specification (version pinned
  and recorded).
- `sim-pix`: an SPI and DICT simulator. It enforces mTLS with certificate-bound OAuth tokens
  (RFC 8705), resolves keys, settles instantly, and issues `endToEndId`s in the specified
  format.
- Charges: `cob` (immediate) and `cobv` (with due date, fine, interest and discount), with
  `txid` never reused per receiver, statuses exactly as the specification's enum, and
  expiry derived rather than stored as a status.
- A **BR Code** encoder and parser (EMV TLV, static and dynamic, CRC16), published as an
  importable package and tested against the manual's example checksum.
- Dynamic payload locations serving a signed payload.
- Received Pix and devoluções (returns), including partial returns, mapped to refunds.
- Pix as a `Rail`: an intent in `requires_action` with the QR code in `next_action`, moved
  to `succeeded` by the simulator's webhook — with an unknown-outcome path when the webhook
  never arrives, resolved by querying `/pix/{e2eid}`.
- Outbound Pix transfers, used later for payouts.

**Exit criteria**

- The golden path's payout segment runs over Pix.
- The simulator rejects a token presented with a certificate other than the one it was
  bound to.
- Every BR Code example in the Manual de Padrões parses and round-trips.

**Non-goals**

- No real SPI or DICT connection; Jupiter is not a Pix participant.
- No key portability or claim flows in DICT.

---

## Phase 8 — Pix Automático

Recurring Pix, the product Stripe's Brazilian accounts do not offer.

**Deliverables**

- Recurrences (`rec`), recurrence requests (`solicrec`) and recurring charges (`cobr`),
  following the specification's resources and statuses.
- The authorization journeys the simulator supports, with journey 1 (push request) able to
  be rejected by the payer.
- Charge scheduling within the specified window before the due date, and the retry policy
  (none, or up to three retries on different days within seven days), each retry with its
  own `endToEndId`.
- A subscription abstraction in Jupiter's API that uses Pix Automático underneath.

**Exit criteria**

- A simulated year of monthly charges runs in accelerated time, including a payer without
  funds, retries, a cancellation by the payer and one by the receiver, with the ledger and
  statuses matching the specification at every step.

**Non-goals**

- No card-based subscriptions or billing logic (proration, plans, invoices); Jupiter is a
  PSP, not a billing system.

---

## Phase 9 — Receivables and the registry

Installment receivables as first-class ledger objects. The central differentiator.

**Deliverables**

- Receivable units generated at capture: one per installment per settlement date, net of
  the fee for that installment count, keyed by merchant document, arrangement, Jupiter's
  identifier as sub-acquirer and settlement date.
- The merchant's **agenda** as a queryable, time-travelling view: what is expected on each
  date, what is committed to contracts, what is free.
- `sim-registry`: a receivables registry simulator implementing the registries'
  Convenção. Contract effects are ownership transfer or lien, by fixed amount or
  percentage. Settlement pays effects FIFO by acceptance, then the free portion. Reductions
  (a chargeback, a cancellation) consume the free portion first, then effects LIFO. Blocks
  cannot touch committed amounts.
- Jupiter's registry adapter with the regulatory timing: updates by the next business day,
  settlement notices, daily, weekly and fortnightly reconciliation with the registry, and a
  business-day calendar with Brazilian holidays.
- Merchant opt-in before a financier can see the agenda.
- Deadlines the research could not confirm (the revised Res. BCB 562/2026 wording) are
  configuration, flagged in the adapter's README.

**Exit criteria**

- Property-based tests of the waterfall: for any sequence of contracts, settlements,
  reductions and blocks, the amounts paid to each beneficiary match a reference model, and
  no unit is ever paid twice or below zero.
- The golden path registers six units and reconciles with the registry with zero
  divergences.

**Non-goals**

- No real registry connection or registry file layouts; the published layouts were not
  available to the research.
- No credit origination against receivables beyond anticipation.

---

## Phase 10 — Anticipation, split and recipients

How money is divided and how a merchant gets it sooner.

**Deliverables**

- Recipients (sellers in a marketplace) with KYC/KYB status, bank or Pix payout
  destination, and transfer settings.
- Split rules on a payment as **typed ledger lines** — recipient, commission, fee,
  remainder — with explicit liability: exactly one recipient is liable for chargebacks and
  exactly one absorbs the remainder. Split validation refuses rules that exceed the net
  amount and never fails silently.
- Balances per recipient: pending, available (with an `available_on` date from settlement
  terms) and reserved.
- **Anticipation** as `simulate → create`: the price computed as the present value of the
  chosen units at a configured rate, checked against the free portion at the registry, and
  registered as an ownership-transfer effect in Jupiter's favour.
- Automatic anticipation settings per recipient.

**Exit criteria**

- The golden path splits the payment and anticipates two units; the ledger, the recipient
  balances and the registry agree.
- Property-based tests: split lines always sum to the payment, and allocation never loses
  a minor unit.

**Non-goals**

- No anticipation funded by third parties; Jupiter is the only financier.

---

## Phase 11 — Settlement, payouts and boleto

Money arriving from the network and leaving to merchants, and the boleto rail. Ends
milestone **M3**.

**Deliverables**

- `sim-slc`: a centralized settlement simulator. Jupiter, as a sub-acquirer, receives
  settlement from the network and pays merchants through it, as Res. BCB 522/2025
  requires, including reporting anticipated settlements.
- Payouts: scheduled and on demand, by Pix or bank transfer, with their own lifecycle
  (including a returned payout), minimum amounts and holds.
- **A Go CNAB 240 library**, published as its own package: a remittance writer and a return
  parser for the collection layout (segments P, Q, T, U), movement and occurrence codes as
  typed values, per-bank variations through a `BankProfile`, and fuzz tests.
- Boleto: barcode and typed line with their check digits, the due-date factor across the
  February 2025 rollover, and the hybrid boleto with a Pix QR code.
- `sim-bank`: a bank simulator that accepts remittance files and produces return files
  (registered, rejected, paid, paid by Pix, written off) and account statements.

**Exit criteria**

- The golden path ends with settlement through the SLC simulator and a Pix payout.
- A boleto scenario in CI: issue, remittance, payment by Pix on the hybrid boleto, return
  file, ledger posting.
- The CNAB library round-trips every example in the FEBRABAN layout and survives a fuzzing
  run without a panic.

**Non-goals**

- No CNAB 400; it is bank-specific with no common standard.
- No real bank connection.

---

## Phase 12 — Disputes

Chargebacks and Pix fraud returns in one model.

**Deliverables**

- A dispute lifecycle: notification, evidence, representment, pre-arbitration,
  arbitration, won or lost. Deadlines per scheme and stage come from a versioned table.
- The Brazilian participant liability cap (180 days from authorization) as configuration.
- MED 2.0 for Pix: blocking funds, the notification window, tracing funds across accounts,
  and contestation, in the same dispute model.
- Financial effects: a dispute debits the liable recipient's balance, and when the balance
  is not enough, reduces future receivable units through the registry waterfall.
- Fraud reports kept separate from disputes, and a monitored ratio in the style of Visa's
  acquirer monitoring program, per merchant.

**Exit criteria**

- A dispute raised after the recipient has anticipated all its units is recovered from
  future units in the order the Convenção requires, and the ledger balances throughout.
- Deadline expiry changes a dispute's state without human action, tested in accelerated
  time.

**Non-goals**

- No automated evidence generation.

---

## Phase 13 — Reconciliation

Proving that what Jupiter believes matches what every counterparty reports.

**Deliverables**

- **Three-way reconciliation**: the ledger against each rail's own record (clearing and
  settlement files from the card simulator, SPI statements, CNAB returns, registry
  positions) and against bank statements.
- Matching rules with scores and reasons, automatic resolution where the rule is exact,
  and a **break queue** for everything else, with ageing.
- Daily reconciliation reports per merchant and per counterparty.
- A published algorithm description: the research found no public three-way
  reconciliation algorithm, so Jupiter's is documented in `docs/reconciliation.md`.

**Exit criteria**

- The simulators are made to drop, duplicate and delay records on purpose; reconciliation
  finds every injected break and no false ones.
- The golden path closes with zero breaks.

**Non-goals**

- No accounting close or financial statements; that is an ERP's job, not a PSP's.

---

## Phase 14 — Correctness and performance

Hardening, measured.

**Deliverables**

- The simulation harness extended to every rail: thousands of seeded concurrent scenarios
  with network faults, process crashes, clock skew and duplicated messages, run in CI on
  every push with a fixed seed budget and nightly with random seeds.
- Load tests of the authorization path and the ledger with published throughput and
  p50/p95/p99 latency, and the saturation point stated honestly.
- Profiling results and the optimizations they led to, each with before and after numbers.
- A runbook for every alert.

**Exit criteria**

- `docs/benchmarks/` holds reproducible results with the command and hardware used.
- Nightly simulation has run for a week without an invariant violation, or each violation
  found has a fix and a regression seed.

**Non-goals**

- No throughput target chosen before measuring. The numbers are whatever the design
  delivers, stated with their conditions.

---

## Phase 15 — Live deployment

Jupiter running where anyone can use it. Ends milestone **M4**.

**Deliverables**

- A low-cost public deployment of the API, the worker, the vault and the simulators, in
  test mode only.
- A public status page with live latency and invariant-check results.
- An example integration and a Postman or HTTP collection that walks the golden path.
- A recorded demo of the golden path.
- Backup, restore and a measured recovery drill.

**Exit criteria**

- A stranger can create an account, get test keys and complete a payment from the
  documentation alone.
- A restore drill has been run and its recovery time recorded.

**Non-goals**

- No live mode, ever. Jupiter moves simulated money only.

---

## Beyond this plan

Deliberately not planned, and why:

- **Multi-currency and FX.** A real problem, but a different one; adding it would dilute
  the Brazilian receivables story.
- **Card-present payments** (terminals, EMV kernels). Hardware-bound and poorly served by
  simulation.
- **Open Finance payment initiation.** Pix covers the account-to-account story; Open
  Finance adds consent management that is its own project.
- **Machine-learning fraud scoring.** Needs real labelled data; see phase 6.
- **Billing** (plans, invoices, proration). A billing system sits on top of a PSP, not
  inside it.
