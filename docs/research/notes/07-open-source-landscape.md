# Open-source payments software relevant to "Jupiter" (Go PSP backend), as of 2026-09-29

Method note: repository metadata (stars, primary language, SPDX license, last push date, archived flag, latest release) came straight from the GitHub REST API (`gh api repos/...`, `gh api repos/.../releases/latest`) and `gh search repos` on **2026-09-29**. Stars are rounded. "Last push" means the date of the most recent push to any branch. That can be a dependabot or CI commit, so where it matters the notes also give the latest *release* date. Feature claims come from each repo's README unless another source is named. The citation for each repo is its own GitHub URL.

---

## 1. Ledgers (TigerBeetle, Formance, Blnk, pgledger, plain-text accounting, Numscript)

### Takeaway
Two maintained Go ledgers stand out as design references: Formance (Postgres-backed, programmable through Numscript) and Blnk (Postgres, with inflight transactions and built-in reconciliation). TigerBeetle is the gold standard for debit/credit primitives such as pending/two-phase transfers and posted/pending balances, but it is a separate Zig database. pgledger is the most readable "ledger in Postgres" reference and the best fit for a portfolio project that wants to *own* its ledger. For Jupiter, the recommendation is to build the ledger in-house on Postgres, borrowing TigerBeetle's transfer and account semantics and pgledger's per-account versioning, rather than embedding any of these.

### Cited Findings
| Repo | Lang | License | Stars | Last push / latest release | Status |
|---|---|---|---|---|---|
| [tigerbeetle/tigerbeetle](https://github.com/tigerbeetle/tigerbeetle) | Zig | Apache-2.0 | ~17.1k | push 2026-09-28; release 0.17.9 (2026-07-06) | Active, pre-1.0 versioning |
| [tigerbeetle/tigerbeetle-go](https://github.com/tigerbeetle/tigerbeetle-go) | Go | Apache-2.0 | ~86 | push 2026-07-06 | Official Go client |
| [formancehq/ledger](https://github.com/formancehq/ledger) | Go | MIT | ~1.4k | push 2026-09-29; v2.4.13 (2026-09-28) | Active |
| [formancehq/numscript](https://github.com/formancehq/numscript) | Go | MIT | ~110 | v0.0.26 (2026-09-25) | Active, still 0.0.x |
| [formancehq/stack](https://github.com/formancehq/stack) | Go | NOASSERTION (mixed) | ~530 | push 2026-09-25 | Active umbrella repo |
| [formancehq/payments](https://github.com/formancehq/payments) | Go | NOASSERTION | ~54 | push 2026-09-29 | Active (connectors to PSPs/banks) |
| [blnkfinance/blnk](https://github.com/blnkfinance/blnk) | Go | Apache-2.0 | ~540 | v0.15.6 (2026-09-28) | Active, pre-1.0 |
| [pgr0ss/pgledger](https://github.com/pgr0ss/pgledger) | Go (plus SQL) | MIT | ~490 | push 2026-09-29; v0.7.0 (2025-12-03) | Active, small |
| [hoophq/sequence](https://github.com/hoophq/sequence) | Clojure | Apache-2.0 | ~510 | push 2026-07-21; only release 0.0.1 (2020) | Low activity |
| [devaccuracy/ledgerforge](https://github.com/devaccuracy/ledgerforge) | Go | Apache-2.0 | ~23 | push 2026-08-16 | Small/new |
| [ledger/ledger](https://github.com/ledger/ledger) (ledger-cli) | C++ | NOASSERTION (BSD-style) | ~6.0k | push 2026-09-22 | Mature, conceptual reference |
| [hledgerorg/hledger](https://github.com/hledgerorg/hledger) | Haskell | GPL-3.0 | ~4.7k | push 2026-09-29 | Mature, conceptual reference |
| [beancount/beancount](https://github.com/beancount/beancount) | Python | GPL-2.0 | ~6.0k | push 2026-08-23 | Mature, conceptual reference |
| [howeyc/ledger](https://github.com/howeyc/ledger) | Go | ISC | ~510 | push 2026-09-05 | Go port of ledger-cli |
| [Modern-Treasury/modern-treasury-go](https://github.com/Modern-Treasury/modern-treasury-go) | Go | MIT | ~14 | push 2026-09-29 | API client only; Modern Treasury's ledger itself is not OSS |

- TigerBeetle's README demo builds its account model from `create_transfers` with `debit_account_id`, `credit_account_id`, `amount`, `ledger`, and `code`. Each account exposes `debits_pending`, `debits_posted`, `credits_pending`, and `credits_posted`. Those pending/posted pairs are the primitives behind authorize/capture and holds. — [tigerbeetle/tigerbeetle](https://github.com/tigerbeetle/tigerbeetle)
- Formance Ledger describes itself as "an atomic multi-postings transactions system, account-based modeling, and is programmable in numscript". It uses PostgreSQL as the main transactional store and ships ledger logs to replica stores for OLAP. It exposes a `/api/ledger/v2` API, and amounts use an `asset` with explicit precision, e.g. `"USD/2"`, with a `world` source account. — [formancehq/ledger](https://github.com/formancehq/ledger)
- Blnk describes a double-entry ledger with "balance monitoring, balance snapshots, historical balances, inflight transactions, scheduling, overdrafts, bulk transactions". It also offers reconciliation that matches "bank statements or payment processor exports" against ledger records using custom matching rules, plus tokenized identity data. — [blnkfinance/blnk](https://github.com/blnkfinance/blnk)
- pgledger is "a double entry ledger implementation in PostgreSQL". Accounts are single-currency, and each account has a `version`. Entries record `account_previous_balance` and `account_current_balance`. FX is modelled as two simultaneous transfers across four accounts, including per-currency liquidity accounts, through `pgledger_create_transfers`. IDs are ULIDs derived from UUIDv7. — [pgr0ss/pgledger](https://github.com/pgr0ss/pgledger)
- moov-io's awesome-fintech list includes Blnk, Formance Ledger and hoophq/sequence under "Banking Infrastructure". — [moov-io/awesome-fintech](https://github.com/moov-io/awesome-fintech)

### Inferences
- **Learn from TigerBeetle:** use pending vs posted balances, two-phase transfers (post or void a pending transfer), `ledger` for the currency/asset partition, `code` as a transfer-type reason, and enforce balance limits at write time. These fit authorization holds, capture, split settlement and payout reserves directly. **Avoid** running TigerBeetle as Jupiter's store. It adds a separate cluster and a Zig database, which dilutes the goal of showing Postgres/Go ledger design, though it is a good "future work / pluggable backend" note.
- **Learn from Formance:** asset-with-precision notation (`BRL/2`), the `world` account for external money, immutable logs, and a DSL for multi-leg postings. Numscript is a good model for splits such as "send 100 BRL from buyer to seller 90% / platform 10%, remaining to X". Jupiter could implement a much smaller Go-typed split-rule engine rather than adopt Numscript (0.0.x).
- **Learn from pgledger:** per-account `version` plus previous/current balance on each entry gives cheap optimistic concurrency and auditability. Single-currency accounts plus liquidity accounts is the cleanest FX model.
- **Learn from Blnk:** inflight transactions (≈ holds) and reconciliation with matching rules. Blnk is the closest open-source analogue to Jupiter's "ledger + reconciliation" combination.
- ledger-cli, hledger and beancount matter conceptually (balance assertions, plain-text audit trail). They could inspire an export format such as a "Jupiter → beancount export" for auditability.
- License caution: Formance `stack` and `payments` report NOASSERTION (mixed licenses). Read the LICENSE files before copying code. MIT (Formance ledger, pgledger) and Apache-2.0 (Blnk, TigerBeetle) are safe to learn from and borrow with attribution.

### Gaps
- The internal locking and concurrency strategies of Formance v2 and Blnk (Redis locks vs Postgres row locks) were not verified from source code in this pass.
- No open-source clone of Modern Treasury's ledger product was found; only its API client is public.

---

## 2. Payment platforms / orchestrators (Hyperswitch, Kill Bill, Lago, Medusa, Fineract, Mojaloop, Moov)

### Takeaway
Hyperswitch (Rust, ~45k stars) is the most relevant open-source reference for a PSP/orchestrator: connectors, routing, retries, vault, reconciliation and cost observability. Mojaloop is the best open-source reference for *scheme/switch* design (DFSPs, account lookup, quotes, central ledger, settlement), which is conceptually close to Pix/SPI. Moov's Go libraries are production-grade file-format and network libraries, but they are US-rail-specific. moov-io/wire is **archived**, and moov-io/paygate has had **no release since 2021** (maintenance commits only).

### Cited Findings
| Repo | Lang | License | Stars | Last push / latest release | Status |
|---|---|---|---|---|---|
| [juspay/hyperswitch](https://github.com/juspay/hyperswitch) | Rust | Apache-2.0 | ~45k | v1.127.0 (2026-09-28) | Very active |
| [juspay/hyperswitch-card-vault](https://github.com/juspay/hyperswitch-card-vault) | Rust | Apache-2.0 | ~69 | push 2026-09-29 | Active |
| [killbill/killbill](https://github.com/killbill/killbill) | Java | Apache-2.0 | ~5.8k | killbill-0.24.22 (2026-09-29) | Active, mature |
| [getlago/lago](https://github.com/getlago/lago) | Go (per GitHub; the core API is Ruby) | AGPL-3.0 | ~10.6k | v1.53.0 (2026-09-08) | Active; billing, not PSP |
| [flexprice/flexprice](https://github.com/flexprice/flexprice) | Go | AGPL-3.0 | ~6.9k | push 2026-09-29 | Active; usage billing in Go |
| [openmeterio/openmeter](https://github.com/openmeterio/openmeter) | Go | Apache-2.0 | ~2.4k | push 2026-09-29 | Active; metering/billing in Go |
| [medusajs/medusa](https://github.com/medusajs/medusa) | TypeScript | NOASSERTION (MIT per project) | ~36.5k | push 2026-09-29 | Active; commerce, payment-provider abstraction |
| [apache/fineract](https://github.com/apache/fineract) | Java | Apache-2.0 | ~2.5k | 1.15.0 (2026-07-14) | Active; core banking / loans |
| [mojaloop/central-ledger](https://github.com/mojaloop/central-ledger) | JavaScript | NOASSERTION (Apache-2.0 per project) | ~58 | v20.2.0 (2026-09-17) | Active |
| [mojaloop/ml-api-adapter](https://github.com/mojaloop/ml-api-adapter) | JavaScript | NOASSERTION | ~11 | push 2026-09-28 | Active |
| [mojaloop/central-settlement](https://github.com/mojaloop/central-settlement) | JavaScript | NOASSERTION | ~8 | push 2026-09-25 | Active |
| [mojaloop/project](https://github.com/mojaloop/project) | n/a | n/a | ~37 | push 2026-09-22 | Issue tracker / index |
| [moov-io/ach](https://github.com/moov-io/ach) | Go | Apache-2.0 | ~570 | v1.63.7 (2026-09-24) | Active, mature |
| [moov-io/achgateway](https://github.com/moov-io/achgateway) | Go | Apache-2.0 | ~85 | push 2026-09-28 | Active |
| [moov-io/ach-test-harness](https://github.com/moov-io/ach-test-harness) | Go | Apache-2.0 | ~17 | push 2026-09-25 | Active; scenario simulator |
| [moov-io/wire](https://github.com/moov-io/wire) | Go | Apache-2.0 | ~99 | push 2026-08-26 | **ARCHIVED**: "Legacy FAIM Fedwire… format deprecated July 2025" |
| [moov-io/wire20022](https://github.com/moov-io/wire20022) | Go | none | ~8 | push 2026-09-29 | **DEPRECATED** (points to moov-io/fedwire20022) |
| [moov-io/fed](https://github.com/moov-io/fed) | Go | Apache-2.0 | ~130 | push 2026-09-25 | Active |
| [moov-io/watchman](https://github.com/moov-io/watchman) | Go | Apache-2.0 | ~510 | v1.0.2 (2026-09-25) | Active; sanctions screening |
| [moov-io/paygate](https://github.com/moov-io/paygate) | Go | Apache-2.0 | ~130 | last release v0.10.4 (**2021-07-21**); push 2026-09-02 (deps/CI only) | **Effectively unmaintained** |
| [moov-io/imagecashletter](https://github.com/moov-io/imagecashletter) | Go | Apache-2.0 | ~78 | push 2026-09-28 | Active (US checks) |
| [moov-io/metro2](https://github.com/moov-io/metro2) | Go | Apache-2.0 | ~130 | push 2026-09-29 | Active (US credit reporting) |
| [moov-io/bai2](https://github.com/moov-io/bai2) | Go | Apache-2.0 | ~26 | push 2026-09-28 | Active (bank statements) |
| [moov-io/pinblock](https://github.com/moov-io/pinblock) | Go | Apache-2.0 | ~17 | push 2026-09-25 | Active |
| [malwarebo/conductor](https://github.com/malwarebo/conductor) | Go | MIT | ~38 | push 2026-09-19 | Small; Go "smart payment switch" |
| [activemerchant/active_merchant](https://github.com/activemerchant/active_merchant) | Ruby | MIT | ~4.6k | push 2025-11-04 | Slowing; gateway abstraction |
| [stripe/stripe-mock](https://github.com/stripe/stripe-mock) | Go | MIT | ~1.6k | push 2026-09-25 | Active; mock server pattern |
| [OpenBankProject/OBP-API](https://github.com/OpenBankProject/OBP-API) | Scala | AGPL-3.0 | ~1.7k | push 2026-09-29 | Active; open banking |
| [hyperledger-firefly/firefly](https://github.com/hyperledger-firefly/firefly) | Go | Apache-2.0 | ~600 | push 2026-09-25 | Active; blockchain-oriented, low relevance |

- Hyperswitch describes "modular, open-source payments infrastructure", with modules for Cost Observability ("detect hidden fees, downgrades, and penalties"), Revenue Recovery (retries "tuned by card bin, region, method"), a PCI-compliant Vault with bring-your-own-vault (VGS, TokenEx), and Intelligent Routing. — [juspay/hyperswitch](https://github.com/juspay/hyperswitch)
- Mojaloop's core services include central-ledger ("managing transfers between DFSPs"), ml-api-adapter (translation to/from the Mojaloop API), account-lookup-service ("managing party accounts across a Mojaloop environment") and Helm charts. — [mojaloop/project](https://github.com/mojaloop/project)
- Conductor is "an open-source payment switch" that unifies Stripe, Xendit, Airwallex and Razorpay for payments, subscriptions and disputes. It has an experimental LLM-based fraud check. — [malwarebo/conductor](https://github.com/malwarebo/conductor)
- The moov-io/paygate README says it is sponsored by Moov and refers users to Moov's commercial payouts documentation. Its latest GitHub release is v0.10.4 from 2021-07-21, and its September 2026 commits are CI/dependency fixes. — [moov-io/paygate](https://github.com/moov-io/paygate)
- moov-io/ach-test-harness provides "programmatic and configurable ACH scenario testing of returns, NOC/corrections, reconciliation". — [moov-io/ach-test-harness](https://github.com/moov-io/ach-test-harness)

### Inferences
- **Hyperswitch:** learn the payment-intent/attempt split, the connector trait abstraction, retry and routing rules, and the separation of the vault into its own service (hyperswitch-card-vault). This is the model for Jupiter's "card via simulator" boundary. Do not copy code (Rust), but its docs are a good source for the state-machine vocabulary.
- **Mojaloop:** use it to design Jupiter's Pix-like flows: account lookup (≈ DICT), quote, transfer prepare/fulfil (a two-phase commit, like TigerBeetle pending/post), and settlement windows. The central-ledger and central-settlement split mirrors SPI vs PI-account settlement at BCB.
- **Kill Bill:** its payment state machine and plugin architecture are well-documented Java references for retries and payment-state transitions. Lago, Flexprice and OpenMeter are billing and metering tools, outside Jupiter's PSP core, though Flexprice and OpenMeter are good examples of Go project structure.
- **Moov:** the most reusable *patterns* are file readers/writers/validators with fuzz tests (ach, bai2) and the test harness that simulates bank returns and corrections. Jupiter should build an equivalent "CNAB return simulator" and "Pix/SPI simulator" in the same style. Watchman can serve directly as a sanctions/PEP screening sidecar for the risk module (Apache-2.0).
- **stripe-mock:** a good pattern for shipping a mock server generated from OpenAPI, which Jupiter could offer for merchant integrators.
- **Avoid** paygate and wire as dependencies (archived or stale).

### Gaps
- The license files of Medusa and Mojaloop report NOASSERTION in the GitHub API. The "MIT/Apache per project" notes above come from general project knowledge and were not re-verified in this pass.
- The Hyperswitch payment-attempt state list was not extracted from source in this pass.

---

## 3. Card / ISO 8583 / 3DS2 / EMV / Luhn / BIN

### Takeaway
Go has a clear winner for ISO 8583: **moov-io/iso8583** (v0.26.1, Aug 2026) with **moov-io/iso8583-connection** for TCP framing, request/response matching, echo messages (0800/0810) and timeouts. Jupiter should use both directly to build its acquirer→network→issuer simulator. Open-source 3DS2 server/ACS implementations are scarce and immature (a handful of repos with fewer than 10 stars). Jupiter will need to write its own 3DS2 simulator from the EMVCo message flow (AReq/ARes/CReq/CRes/RReq), which is itself a differentiator.

### Cited Findings
| Repo | Lang | License | Stars | Last push / release | Status |
|---|---|---|---|---|---|
| [moov-io/iso8583](https://github.com/moov-io/iso8583) | Go | Apache-2.0 | ~530 | v0.26.1 (2026-08-13) | Active, de facto Go standard |
| [moov-io/iso8583-connection](https://github.com/moov-io/iso8583-connection) | Go | Apache-2.0 | ~120 | v0.9.0 (2025-08-26); push 2026-09-02 | Active |
| [jpos/jPOS](https://github.com/jpos/jPOS) | Java | AGPL-3.0 | ~720 | tag v3_0_1; push 2026-09-27 | Active, reference implementation |
| [jpos/jPOS-EE](https://github.com/jpos/jPOS-EE) | Java | AGPL-3.0 | ~130 | push 2026-09-27 | Active |
| [kpavlov/jreactive-8583](https://github.com/kpavlov/jreactive-8583) | Kotlin/Java | Apache-2.0 | ~355 | push 2026-09-29 | Active |
| [juks/iso-8583-socket-queue](https://github.com/juks/iso-8583-socket-queue) | JavaScript | MIT | ~210 | push 2026-07-15 | Active |
| [rkbalgi/isosim](https://github.com/rkbalgi/isosim) | Go (+React) | Apache-2.0 | ~116 | push **2022-11-24** | **Stale** |
| [nao1215/iso8583tool](https://github.com/nao1215/iso8583tool) | Go | MIT | ~6 | push 2026-09-28 | New, small (message inspection CLI) |
| [fatihbulut/paymentlab](https://github.com/fatihbulut/paymentlab) | HTML | MIT | ~2 | push 2026-08-04 | Tiny; acquirer/issuer/switch simulator |
| [moov-io/pinblock](https://github.com/moov-io/pinblock) | Go | Apache-2.0 | ~17 | push 2026-09-25 | Active; PIN block formats |
| [greenboxal/emv-kernel](https://github.com/greenboxal/emv-kernel) | Go | none | ~100 | push **2020-01-30** | **Stale**, no license |
| [heyvito/tlvp](https://github.com/heyvito/tlvp) | Go | MIT | ~9 | push 2020 | Stale; EMV TLV parser CLI |
| [copyleftdev/emv-3ds](https://github.com/copyleftdev/emv-3ds) | Rust | MIT | ~4 | push 2026-07-19 | New/small; 3DS2 messages + state machine |
| [iicuriosity/3DS_Server](https://github.com/iicuriosity/3DS_Server) | Java | MIT | ~3 | push 2024-04-21 | Small |
| [inikolaev/3ds](https://github.com/inikolaev/3ds) | Kotlin | none | ~4 | push 2021 | Stale, educational |
| [gpayments/3ds-requestor-springboot](https://github.com/gpayments/3ds-requestor-springboot) | Java | none | ~4 | push 2021 | Stale; requestor demo for a commercial 3DS server |
| [clearhaus/3DSv2-api-documentation](https://github.com/clearhaus/3DSv2-api-documentation) | HTML | none | ~5 | push 2026-09-15 | Docs for a commercial 3DS server |
| [Lekkie/hsm-emulator](https://github.com/Lekkie/hsm-emulator) | Scala | other | ~20 | push 2018 | Stale; Thales HSM emulator |
| [gjyoung1974/hsm-simulator](https://github.com/gjyoung1974/hsm-simulator) | Java | none | ~12 | push 2019 | Stale |
| [durango/go-credit-card](https://github.com/durango/go-credit-card) | Go | other | ~140 | push 2022-07-15 | Stale-ish; Luhn + brand detection |
| [osamingo/checkdigit](https://github.com/osamingo/checkdigit) | Go | MIT | ~115 | push 2025-03-28 | Maintained; Luhn, Verhoeff, Damm, ISBN… |
| [theplant/luhn](https://github.com/theplant/luhn) | Go | MIT | ~19 | push 2024-03-31 | Small |

- moov-io/iso8583 defines message specs with Go types and struct tags. It handles unknown TLV tags and states that ISO 8583 "is used by major card networks around the globe including Visa, Mastercard, and Verve". — [moov-io/iso8583](https://github.com/moov-io/iso8583)
- moov-io/iso8583-connection handles "sending, receiving and matching ISO 8583 messages between client and server… both for acquiring and issuing services". It offers `SendTimeout`, `IdleTime` (automatic ping/echo), `ReadTimeout`, `InboundMessageHandler` and `ConnectionClosedHandler`, and its example builds an `0810` echo response. — [moov-io/iso8583-connection](https://github.com/moov-io/iso8583-connection)
- isosim is "a ISO8583 simulator built using Go, React…". It lets users define ISO specifications, run servers from the UI or standalone, and build and send transactions as a client. Its last push was 2022. — [rkbalgi/isosim](https://github.com/rkbalgi/isosim)
- copyleftdev/emv-3ds describes an "EMV 3-D Secure 2.x (3DS2) protocol — AReq/ARes/CReq/CRes messages, transaction state machine". — [copyleftdev/emv-3ds](https://github.com/copyleftdev/emv-3ds)
- GitHub search for "3ds server" returned mostly Nintendo 3DS projects. The payment-related hits were iicuriosity/3DS_Server (3 stars) and unravelin/ravelin-3ds-demo (1 star, Go, a client for a commercial 3DS server). — [GitHub search results, 2026-09-29](https://github.com/search?q=3ds+server&type=repositories)
- BIN lookups: search results were dominated by thin wrappers around the binlist.net web service, with no strong open-source BIN dataset library. — [GitHub search "bin list card"](https://github.com/search?q=bin+list+card&type=repositories)

### Inferences
- **Reuse directly:** moov-io/iso8583 and iso8583-connection (Apache-2.0). Define a "Jupiter network" spec (a subset of the Visa/Mastercard-style DE fields: 2, 3, 4, 7, 11, 12, 14, 22, 37, 38, 39, 41, 42, 49, 55), plus acquirer and issuer processes connected over TCP, with 0100/0110 auth, 0200/0210 financial, 0400/0410 reversal and 0800/0810 network management.
- **Learn from jPOS:** the design of channels, packagers, MUX and TransactionManager, and the idea of a "switch" with participants. It is AGPL-3.0, so learn from it but do not copy code.
- **Build your own 3DS2 simulator:** no mature open-source 3DS server or ACS exists. Model it as the 3DS Server ↔ Directory Server ↔ ACS flow with frictionless vs challenge outcomes (transStatus Y/N/U/A/C/R), ECI and CAVV/AAV generation, fed into the ISO 8583 auth (DE 55 or private fields). This is a portfolio differentiator in itself. copyleftdev/emv-3ds (Rust, MIT) can serve as a cross-check of message fields.
- **Luhn/BIN:** Luhn is trivial and should be implemented in-house (or taken from osamingo/checkdigit). Keep a small in-repo static BIN table for test cards, because no well-maintained open-source BIN dataset was found.
- PIN, HSM and DUKPT fall outside Jupiter's scope. moov-io/pinblock is available if PIN blocks are ever simulated. The HSM emulators found are stale.

### Gaps
- No actively maintained open-source 3DS2 ACS or Directory Server was found. EMVCo specifications are not in any repo, and the spec documents have to be obtained from EMVCo.
- No maintained open-source card network or issuer simulator in Go was found beyond stale isosim and tiny hobby repos.

---

## 4. Brazil-specific (Pix, BR Code, DICT, boleto, CNAB, CPF/CNPJ incl. alphanumeric CNPJ)

### Takeaway
The authoritative Pix artifact is **bacen/pix-api** (OpenAPI 3.0 spec, release 2.10.0 on 2026-08-19, ~3k stars). Jupiter's Pix API should mirror its resource model (`/cob`, `/cobv`, `/pix`, `/webhook`, devolução). In Go, **fonini/go-pix** (MIT, active) covers BR Code copia-e-cola and QR generation. Boleto and CNAB tooling is strongest in PHP and Ruby (laravel-boleto, OpenCnabPHP, brcobranca). Go CNAB libraries are almost nonexistent (rafaeljusto/gocnab, a generic fixed-width marshaler, and raykavin/gocnab, 1 star), so a Go CNAB 240/400 library is a real gap Jupiter can fill. The alphanumeric CNPJ started being issued on **31 July 2026**. The Go libraries paemuri/brdoc and phenpessoa/br have commits implementing alphanumeric CNPJ validation.

### Cited Findings
**Pix / BR Code / DICT**
| Repo | Lang | License | Stars | Last push / release | Status |
|---|---|---|---|---|---|
| [bacen/pix-api](https://github.com/bacen/pix-api) | OpenAPI (YAML) | none on GitHub (README badge says Apache-2.0) | ~3.0k | release 2.10.0 (2026-08-19) | Official BCB spec, active |
| [bacen/pix-dict-api](https://github.com/bacen/pix-dict-api) | OpenAPI | none | ~430 | push **2022-10-24** | Official, not updated since 2022 on GitHub |
| [fonini/go-pix](https://github.com/fonini/go-pix) | Go | MIT | ~89 | v1.1.2 (2026-09-28) | Active; BR Code + QR generation |
| [thalesog/go-pix-utils](https://github.com/thalesog/go-pix-utils) | Go | MIT | ~3 | push 2024-08-19 | Small; parse/generate/validate |
| [thiagozs/go-pixgen](https://github.com/thiagozs/go-pixgen) | Go | NOASSERTION | ~13 | push 2025-10-20 | Small |
| [dongri/emv-qrcode](https://github.com/dongri/emv-qrcode) | Go | MIT | ~60 | push 2026-09-03 | Active; generic EMV MPM/CPM QR |
| [mercari/go-emv-code](https://github.com/mercari/go-emv-code) | Go | MIT | ~70 | push 2023-08-17 | Stale; EMV QR encode/decode |
| [mvallim/emv-qrcode](https://github.com/mvallim/emv-qrcode) | Java | Apache-2.0 | ~136 | push 2026-05-12 | Active; used for Pix in the Java world |
| [naomijub/brcode](https://github.com/naomijub/brcode) | Rust | LGPL-3.0 | ~62 | push 2026-01-02 | Maintained; Pix BR Code parser |
| [renatomb/php_qrcode_pix](https://github.com/renatomb/php_qrcode_pix) | PHP | CC0-1.0 | ~218 | push 2026-08-18 | Popular PHP reference |
| [NascentSecureTech/pix-qrcode-utils](https://github.com/NascentSecureTech/pix-qrcode-utils) | TypeScript | MIT | ~82 | push 2026-09-28 | Active; EMV merchant QR parser/generator |
| [Gustavosta/pix-utils](https://github.com/Gustavosta/pix-utils) | Python | MIT | ~33 | push 2022 | Stale |
| [guibranco/PIX-BACEN-SDK-dotnet](https://github.com/guibranco/PIX-BACEN-SDK-dotnet) | C# | MIT | ~22 | push 2026-09-28 | Active; contracts generated from bacen/pix-api |
| [efipay/sdk-go-apis-efi](https://github.com/efipay/sdk-go-apis-efi) | Go | MIT | ~2 | push 2025-09-23 | PSP client (Efí) |
| [IgorGrieder/DICT-PIX-Simulator](https://github.com/IgorGrieder/DICT-PIX-Simulator) | Go | none | ~5 | push 2026-01-20 | Hobby; DICT simulator |
| [arinellidu/pix-sandbox-pay](https://github.com/arinellidu/pix-sandbox-pay) | Go | Apache-2.0 | 0 | push 2026-08-01 | New; "drop-in Pix emulator" |
| [Paulo-Marcos-Lucio/pix-automatico-reference](https://github.com/Paulo-Marcos-Lucio/pix-automatico-reference) | Java | MIT | ~4 | push 2026-09-28 | New; Pix Automático + Open Finance consent |

**Boleto / CNAB**
| Repo | Lang | License | Stars | Last push / release | Status |
|---|---|---|---|---|---|
| [eduardokum/laravel-boleto](https://github.com/eduardokum/laravel-boleto) | PHP | MIT | ~630 | 0.11.2 (2026-04-29) | Active; boletos + remessa/retorno for many banks |
| [openboleto/openboleto](https://github.com/openboleto/openboleto) | PHP | none | ~610 | push 2026-02-13 | Maintained; boleto generation |
| [openboleto/OpenCnabPHP](https://github.com/openboleto/OpenCnabPHP) | PHP | MIT | ~215 | v2.6.1 (2026-01-05) | Active; CNAB 240/400 remessa + retorno, multi-bank |
| [kivanio/brcobranca](https://github.com/kivanio/brcobranca) | Ruby | MIT | ~380 | v11.0.0 (2024-01-12); push 2026-08-28 | Maintained |
| [eduardordm/cnab240](https://github.com/eduardordm/cnab240) | Ruby | MIT | ~58 | push **2019** | Stale |
| [manoelcampos/retorno-boletophp](https://github.com/manoelcampos/retorno-boletophp) | PHP | MIT | ~147 | **ARCHIVED** | Archived |
| [TracyWebTech/cnab240](https://github.com/TracyWebTech/cnab240) | Python | MIT | ~36 | push **2017** | Stale |
| [Trust-Code/python-cnab](https://github.com/Trust-Code/python-cnab) | Python | MIT | ~39 | push **2021** | Stale |
| [vintasoftware/aurorae](https://github.com/vintasoftware/aurorae) | Python | MIT | ~21 | push 2022 | Stale; CNAB240 payroll files |
| [s2way/cnab240-nodejs](https://github.com/s2way/cnab240-nodejs) | JavaScript | BSD-2 | ~18 | push 2022 | Stale |
| [rafaeljusto/gocnab](https://github.com/rafaeljusto/gocnab) | Go | MIT | ~24 | push 2023-03-05 | Stale-ish; generic CNAB (un)marshaler with struct tags |
| [raykavin/gocnab](https://github.com/raykavin/gocnab) | Go | MIT | ~1 | push 2026-09-13 | New; CNAB 240 remessa SDK |
| [fonini/go-boleto-utils](https://github.com/fonini/go-boleto-utils) | Go | MIT | ~7 | push 2025-02-24 | Small; boleto parse/validate |
| [hubcash/boleto](https://github.com/hubcash/boleto) | Go | MIT | ~14 | push **2017** | Stale |
| [italolelis/go-boleto](https://github.com/italolelis/go-boleto) | Go | MIT | ~6 | push 2019 | Stale |
| [marcelo-lourenco/cnab](https://github.com/marcelo-lourenco/cnab) | JavaScript | MIT | ~6 | push 2026-06-22 | Tool; CNAB 150/240/400 visualizer |

**CPF / CNPJ (incl. alphanumeric)**
| Repo | Lang | License | Stars | Last push | Alphanumeric CNPJ |
|---|---|---|---|---|---|
| [paemuri/brdoc](https://github.com/paemuri/brdoc) | Go | Unlicense | ~147 | 2026-09-29 (tag v3.0.1) | Yes: commits "Implement alphanumeric CNPJ validations" / "Refactor alphanumeric CNPJ code" |
| [phenpessoa/br](https://github.com/phenpessoa/br) | Go | MIT | ~46 | 2026-09-28 | Yes: commit "AlphaNumerical method for CNPJ" |
| [brazanation/go-documents](https://github.com/brazanation/go-documents) | Go | MIT | ~44 | 2024-08-19 | Not verified (predates the rollout) |
| [klassmann/cpfcnpj](https://github.com/klassmann/cpfcnpj) | Go | Apache-2.0 | ~24 | **2020** | Stale |
| [gabrielfroes/novo-cnpj](https://github.com/gabrielfroes/novo-cnpj) | TypeScript | none | ~45 | 2026-04-07 | Yes (dedicated) |

- The bacen/pix-api README: "O presente repositório define as especificações funcionais em formato OpenAPI 3.0 referentes à API Pix." Latest release is 2.10.0, dated 2026-08-19. — [bacen/pix-api](https://github.com/bacen/pix-api)
- Receita Federal began issuing the alphanumeric CNPJ on 31 July 2026. It keeps 14 characters, and the first 12 positions accept 0-9 and A-Z. Numeric and alphanumeric CNPJs coexist, existing numbers stay valid, and the rules come from IN RFB 2.119/2022 and 2.229/2024. — [Receita Federal – implantação a partir de 31 de julho](https://www.gov.br/receitafederal/pt-br/assuntos/noticias/2026/julho/implantacao-do-cnpj-alfanumerico-ocorrera-a-partir-de-31-de-julho); [Receita Federal – CNPJ Alfanumérico page](https://www.gov.br/receitafederal/pt-br/acesso-a-informacao/acoes-e-programas/programas-e-atividades/cnpj-alfanumerico); [Receita Federal gera o primeiro CNPJ alfanumérico](https://www.gov.br/receitafederal/pt-br/assuntos/noticias/2026/julho/receita-federal-gera-o-primeiro-cnpj-em-formato-alfanumerico)
- OpenCnabPHP describes itself as a "biblioteca multibanco para geração e leitura de arquivos Cnab240 e Cnab400 de remessa e retorno". — [openboleto/OpenCnabPHP](https://github.com/openboleto/OpenCnabPHP)
- guibranco/PIX-BACEN-SDK-dotnet provides "PIX/SPI contracts from @bacen/pix-api", which shows the pattern of generating typed clients from the BCB OpenAPI. — [guibranco/PIX-BACEN-SDK-dotnet](https://github.com/guibranco/PIX-BACEN-SDK-dotnet)

### Inferences
- **Reuse bacen/pix-api directly:** generate Go server stubs or types (e.g. with oapi-codegen) from `openapi.yaml` so Jupiter's Pix endpoints are spec-compliant. This is a strong, verifiable portfolio signal. Pin to release 2.10.0.
- **BR Code:** fonini/go-pix is usable (MIT) for generation. Jupiter should still implement its own *parser* and validator (the EMV TLV structure with IDs 00/26/52/53/54/58/59/60/62/63 and a CRC16-CCITT check), because parsing is where most Go libraries are thin. dongri/emv-qrcode and naomijub/brcode (Rust) are good cross-checks for test vectors.
- **DICT/SPI:** no maintained open-source DICT or SPI simulator exists (only hobby repos). bacen/pix-dict-api (last pushed 2022) still gives the DICT resource model. Jupiter's in-house SPI/DICT simulator is a genuine gap.
- **CNAB:** use OpenCnabPHP, laravel-boleto and brcobranca as *layout references*: their per-bank YAML/PHP field maps (positions, picture clauses) are the most complete public source of the FEBRABAN 240 and bank-specific 400 layouts. Build a Go CNAB library in the style of rafaeljusto/gocnab (struct tags) with moov-io/ach-quality validation and fuzz tests. A maintained Go CNAB library does not exist, so this is a real contribution.
- **Boleto:** barcode (44 digits) ↔ linha digitável (47 digits) conversion, mod-10/mod-11 check digits and the fator de vencimento are small enough to implement in-house. fonini/go-boleto-utils is a reference. Note that the FEBRABAN due-date factor rolled over in Feb 2025 (to be verified; see Gaps).
- **CPF/CNPJ:** use paemuri/brdoc or phenpessoa/br, or implement in-house. Either way, the ID type must store CNPJ as a **string** (never an integer) and validate the alphanumeric DV. Under the Receita rule the value of each character is its ASCII code minus 48, followed by the usual mod-11 weights. That algorithm description comes from general knowledge and was not verified in the sources fetched here (see Gaps).

### Gaps
- The exact alphanumeric CNPJ check-digit algorithm (ASCII−48 plus mod 11) was not confirmed from a fetched Receita document in this pass.
- The boleto due-date factor reset (fator de vencimento rolling over from 9999 in Feb 2025) was not verified from a source in this pass.
- Contents of bacen/pix-api 2.10.0 (e.g. Pix Automático/recorrência endpoints) were not diffed. Check the CHANGELOG in the repo.
- Whether bacen/pix-dict-api has moved elsewhere, given no GitHub updates since 2022, is unknown.

---

## 5. Go infrastructure commonly used in fintech Go services

### Takeaway
The mainstream 2026 stack is healthy and actively maintained: pgx v5 plus sqlc for Postgres, River for transactional background jobs, Watermill (or a small outbox library) for events, Temporal for long-running workflows, and OpenTelemetry Go for telemetry. For money, integer minor units (int64 centavos) are the safest core representation. shopspring/decimal (last release April 2024) is fine for rates and FX but should not be the ledger's source of truth.

### Cited Findings
| Repo | Lang | License | Stars | Latest release | Notes |
|---|---|---|---|---|---|
| [jackc/pgx](https://github.com/jackc/pgx) | Go | MIT | ~14.3k | v5.11.0 (2026-09-07) | Postgres driver/toolkit |
| [sqlc-dev/sqlc](https://github.com/sqlc-dev/sqlc) | Go | MIT | ~18.3k | v1.31.1 (2026-04-22) | Type-safe code from SQL |
| [riverqueue/river](https://github.com/riverqueue/river) | Go | MPL-2.0 | ~5.7k | v0.47.0 (2026-08-31) | Postgres job queue, transactional enqueue |
| [ThreeDotsLabs/watermill](https://github.com/ThreeDotsLabs/watermill) | Go | MIT | ~9.9k | v1.5.3 (2026-08-25) | Pub/sub incl. SQL (Postgres) backend |
| [temporalio/sdk-go](https://github.com/temporalio/sdk-go) | Go | MIT | ~980 | v1.49.0 (2026-09-14) | Workflow SDK |
| [temporalio/temporal](https://github.com/temporalio/temporal) | Go | MIT | ~23.4k | push 2026-09-29 | Temporal server |
| [open-telemetry/opentelemetry-go](https://github.com/open-telemetry/opentelemetry-go) | Go | Apache-2.0 | ~6.6k | v1.46.0 (2026-08-25) | Tracing/metrics |
| [shopspring/decimal](https://github.com/shopspring/decimal) | Go | NOASSERTION (MIT) | ~7.5k | v1.4.0 (**2024-04-12**) | Arbitrary-precision decimal; slow release cadence |
| [cockroachdb/apd](https://github.com/cockroachdb/apd) | Go | Apache-2.0 | ~810 | push 2026-03-23 | General decimal arithmetic |
| [govalues/decimal](https://github.com/govalues/decimal) | Go | MIT | ~250 | push **2025-01-18** | Correctly rounded, fixed-size decimals |
| [ericlagergren/decimal](https://github.com/ericlagergren/decimal) | Go | BSD-3 | ~580 | push 2024-04-11 | Stale-ish |
| [Rhymond/go-money](https://github.com/Rhymond/go-money) | Go | MIT | ~1.9k | v1.0.15 (2025-04-30) | Fowler Money pattern, int64 minor units, Split/Allocate |
| [bojanz/currency](https://github.com/bojanz/currency) | Go | MIT | ~640 | push 2026-09-06 | Currency handling / CLDR formatting (decimal-string based) |
| [nikolayk812/pgx-outbox](https://github.com/nikolayk812/pgx-outbox) | Go | MIT | ~150 | push 2026-03-02 | Transactional outbox for Postgres/pgx |
| [oagudo/outbox](https://github.com/oagudo/outbox) | Go | MIT | ~134 | push 2026-03-03 | Outbox lib, DB-agnostic |
| [testcontainers/testcontainers-go](https://github.com/testcontainers/testcontainers-go) | Go | MIT | ~5.0k | push 2026-09-29 | Integration tests with real Postgres |
| [pressly/goose](https://github.com/pressly/goose) | Go | NOASSERTION (MIT) | ~11.5k | push 2026-09-28 | Migrations |
| [golang-migrate/migrate](https://github.com/golang-migrate/migrate) | Go | NOASSERTION (MIT) | ~18.9k | push 2026-09-09 | Migrations |
| [go-chi/chi](https://github.com/go-chi/chi) | Go | MIT | ~22.9k | push 2026-09-29 | Idiomatic net/http router |
| [ThreeDotsLabs/wild-workouts-go-ddd-example](https://github.com/ThreeDotsLabs/wild-workouts-go-ddd-example) | Go | MIT | ~6.5k | push 2026-08-27 | DDD/clean-arch reference project |
| [rafael-piovesan/go-rocket-ride](https://github.com/rafael-piovesan/go-rocket-ride) | Go | MIT | ~160 | push 2022 | Go port of Stripe-style idempotency keys (after Brandur's "rocket-ride") |
| [openfga/openfga](https://github.com/openfga/openfga) | Go | Apache-2.0 | ~5.9k | push 2026-09-28 | Authorization (merchant/sub-account permissions) |

- River: "By enqueueing jobs transactionally along with… application data… Jobs are guaranteed to be enqueued if their transaction commits, are removed if their transaction rolls back, and aren't visible for work until commit." — [riverqueue/river](https://github.com/riverqueue/river)
- Watermill supports Kafka, RabbitMQ, HTTP, and "SQL (MySQL / PostgreSQL) Pub/Sub" through `watermill-sql/v4`. — [ThreeDotsLabs/watermill](https://github.com/ThreeDotsLabs/watermill)
- go-money "represents monetary values as integers, in cents" and provides `Split` and `Allocate`. It also offers `NewFromFloat`, which a PSP should avoid. — [Rhymond/go-money](https://github.com/Rhymond/go-money)
- shopspring/decimal's latest GitHub release is v1.4.0 from 2024-04-12, although the repo received pushes up to 2026-08-19. — [shopspring/decimal](https://github.com/shopspring/decimal)

### Inferences
- Recommended Jupiter baseline: **pgx v5 + sqlc** (explicit SQL suits ledger invariants); **River** for webhooks, payouts, settlement batches and CNAB file generation, enqueued in the same transaction as the state change (this acts as a built-in outbox); **OpenTelemetry**; **testcontainers-go** for Postgres integration tests. Temporal is powerful for sagas such as a payout lifecycle or dispute timelines, but it adds a heavy dependency. River plus an explicit state machine in Postgres is simpler to run and easier to explain in a portfolio. If Temporal is used, limit it to one showcase flow (e.g. disputes with deadlines).
- Watermill is optional. River's transactional enqueue already covers outbox needs for a single service, so add Watermill only if you want Kafka fan-out.
- Money: define a custom `Amount{ Minor int64; Currency string }` type (or use go-money but ban `NewFromFloat`). Use Allocate-style largest-remainder splitting for splits and MDR fees, and use a decimal library only for rates (MDR %, FX) and round explicitly.
- License note: River is **MPL-2.0** (file-level copyleft). Using it as a dependency is fine, but modified River files must stay MPL.

### Gaps
- No benchmark comparison of decimal libraries was collected in this pass.
- Temporal vs River adoption in Brazilian fintechs is not documented in any source found here.

---

## 6. Awesome lists and curated indexes

### Takeaway
The most useful curated list for Jupiter is **moov-io/awesome-fintech**, which is maintained (push 2026-09-24) and covers ISO 8583, ISO 20022, ledgers and compliance. **kdeldycke/awesome-billing** (CC0, ~1.3k stars, active) is the best conceptual reading list on billing and payments. The "awesome-payments" name did not resolve to a meaningful maintained repo, since GitHub search for it returns spam and unrelated repos.

### Cited Findings
| Repo | Stars | License | Last push | Notes |
|---|---|---|---|---|
| [moov-io/awesome-fintech](https://github.com/moov-io/awesome-fintech) | ~380 | none | 2026-09-24 | Sections: Payments & Integrations, Banking Infrastructure, Compliance & Sanctions, Money/Currency, Billing, Learning Resources |
| [kdeldycke/awesome-billing](https://github.com/kdeldycke/awesome-billing) | ~1.35k | CC0-1.0 | 2026-09-28 | "Billing & Payments knowledge for cloud platforms" |
| [7kfpun/awesome-fintech](https://github.com/7kfpun/awesome-fintech) | ~360 | CC0-1.0 | 2026-02-25 | General financial libraries |
| [jplock/awesome-fintech](https://github.com/jplock/awesome-fintech) | ~110 | CC0-1.0 | 2026-08-25 | Companies, not code |
| [brandonhimpfen/awesome-fintech](https://github.com/brandonhimpfen/awesome-fintech) | ~25 | n/a | 2026-09-06 | Small |

- moov-io/awesome-fintech lists jPOS ("used in production gateways and switches since 1998"), moov-io/iso8583, iso8583-connection, jreactive-8583, iso20022.js, Prowide ISO 20022, Blnk, Formance Ledger and hoophq/sequence. — [moov-io/awesome-fintech](https://github.com/moov-io/awesome-fintech)
- The previously assumed repos `paymentsdb/awesome-payments` and `lorenzodb1/awesome-payments` return 404 on the GitHub API, and a GitHub search for "awesome-payments" returned only unrelated or spam repos. — [GitHub search "awesome-payments"](https://github.com/search?q=awesome-payments&type=repositories)

### Inferences
- Use moov-io/awesome-fintech as the starting index and kdeldycke/awesome-billing for concepts (dunning, proration, idempotency, reconciliation). No Brazil-specific awesome list for Pix, CNAB and boleto was found. A curated "awesome-pagamentos-br" section in Jupiter's README would be a small, visible contribution.
- **Overall gap map for Jupiter's positioning (from all sections):** well served by open source are ledgers (Formance, Blnk, pgledger), ISO 8583 in Go (moov) and US rails (moov). Poorly served or absent are a Go CNAB 240/400 library, a Pix SPI/DICT simulator, an open 3DS2 server/ACS simulator, a card network/issuer simulator in Go, and an integrated Brazilian PSP reference (split + Pix + boleto + card). Jupiter can credibly claim that combined space.

### Gaps
- No Brazil-focused awesome list (Pix/boleto/CNAB/Open Finance Brasil) was found.
- Star counts are snapshots (2026-09-29) and will drift.
