# Jupiter

A payment service provider backend in Go: the system behind an API like Stripe's or
Pagar.me's. It accepts payments by card, Pix and boleto, splits them between recipients,
carries them through settlement on a double-entry ledger, and pays merchants out.

This repository is the **backend only**.

> ### Current phase: **6 — 3-D Secure, network tokens and risk** · milestone M1 reached
>
> A correct ledger, a Stripe-grade API with idempotency and signed webhooks, card numbers
> kept in a separate vault, live payments over ISO 8583 to a card network and issuer
> simulator, and now the decisions around them: a risk engine with velocity rules,
> merchant rules and card-testing detection, 3-D Secure 2 authentication against a
> simulated directory server and ACS, frictionless or with a challenge, and network
> tokens with their lifecycle. A deterministic simulation runs 10,000 payments with faults
> on every push. Next is Pix (phase 7), which reaches milestone M2; see
> [`docs/plan.md`](docs/plan.md).

---

## What it demonstrates

- **Money correctness under failure.** An append-only, zero-sum ledger with pending and
  posted balances, invariants checked continuously, and a payment core in which a timeout
  is an unknown outcome to be resolved, never a decline.
- **Protocol fidelity.** Card payments over ISO 8583, Pix per the Banco Central API
  specification with mTLS and certificate-bound tokens, boleto and FEBRABAN CNAB 240 —
  each verified against its specification's own examples.
- **A domain global PSPs do not model.** Credit sales in installments become registered
  receivable units that financiers, courts and chargebacks compete for. Jupiter models
  them, their contract effects, anticipation and centralized settlement as parts of the
  ledger.

A universal core — payment intents, the ledger, idempotency, split, payouts, disputes and
reconciliation — carries the Brazilian rails behind adapters.

## Simulated counterparties

Jupiter is not a licensed payment institution and connects to no real network. Every
external party is a simulator shipped in this repository and reached over the protocol a
real one would use:

| Simulator | Stands for |
|---|---|
| `sim-card-network` | Card network and issuer, over ISO 8583 |
| `sim-3ds` | 3DS2 directory server and access control server |
| `sim-pix` | Pix settlement (SPI) and key directory (DICT) |
| `sim-bank` | A bank exchanging CNAB files and statements |
| `sim-registry` | A card receivables registry |
| `sim-slc` | Centralized settlement for sub-acquirers |

Where a simulator's behaviour rests on a claim the research could not verify against a
primary source, its documentation says so.

## Documentation

| | |
|---|---|
| [`docs/plan.md`](docs/plan.md) | Phases, deliverables, exit criteria, non-goals, milestones |
| [`api/openapi.yaml`](api/openapi.yaml) | The API contract; the server is generated from it |
| [`docs/api/`](docs/api/) | Error codes, receiving webhooks, test cards and amounts, and the risk engine |
| [`docs/pci-scope.md`](docs/pci-scope.md) | What handles card data, what does not, and the tests that keep it so |
| [`docs/cardnet/`](docs/cardnet/) | The card network's ISO 8583 specification, field by field, sourced or not |
| [`docs/adr/`](docs/adr/) | Architecture decisions, each with the alternatives rejected |
| [`docs/benchmarks/`](docs/benchmarks/) | Measurements, with the command and hardware that produced them |
| [`docs/research/`](docs/research/) | How the payments market works, globally and in Brazil, and what it implies for Jupiter |

## Getting started

Requirements: Go 1.27 and Docker with Compose. Every other tool is pinned in
[`tools/go.mod`](tools/go.mod) and runs through `go tool`, so there is nothing else to
install.

```sh
make check             # format, vet, lint, vulnerabilities, unit and architecture tests
make test-integration  # tests against a real PostgreSQL in Docker
make up                # local infrastructure; returns once every service is healthy
make migrate           # apply database migrations to it, Jupiter's and the vault's
make certs             # a development CA and the vault's and API's mTLS certificates
make merchant NAME=x   # create a merchant; prints its API keys once
make ledger-check      # verify the ledger's invariants on it
make demo              # walk through the golden path, step by step
make test-simulation   # the deterministic simulation (SIM_PAYMENTS, SIM_SEED)
make down
make help              # every target
```

`make up` starts PostgreSQL, a separate PostgreSQL for the vault, the OpenTelemetry
Collector, Jaeger, Prometheus and Grafana.
They listen on 127.0.0.1 only, on ports of their own so they can run beside other local
stacks:

| Service | Address |
|---|---|
| PostgreSQL | `postgres://jupiter:jupiter@127.0.0.1:55432/jupiter` |
| PostgreSQL (vault) | `postgres://vault:vault@127.0.0.1:55433/vault` |
| OTLP (gRPC / HTTP) | `127.0.0.1:54317` / `127.0.0.1:54318` |
| Jaeger | http://127.0.0.1:56686 |
| Prometheus | http://127.0.0.1:59090 |
| Grafana | http://127.0.0.1:53000 |

Each port can be changed in a `.env` file; see [`.env.example`](.env.example).

To run the binaries, start the vault first (`go run ./cmd/vault`) and, for live mode, the
card network and 3-D Secure simulators (`go run ./cmd/sim-card-network`,
`go run ./cmd/sim-3ds`), then the API and the worker
(`go run ./cmd/api`, `go run ./cmd/worker`). [`.env.example`](.env.example) lists
what each reads: databases, keys (`openssl rand -base64 32`) and the certificates from
`make certs`. Then save a card and pay with it:

```sh
curl -X POST http://127.0.0.1:8080/v1/payment_methods -H "Authorization: Bearer $SK_TEST" \
  -d '{"type": "card", "card": {"number": "4242424242424242", "exp_month": 12, "exp_year": 2030}}'
curl -X POST http://127.0.0.1:8080/v1/payment_intents \
  -H "Authorization: Bearer $SK_TEST" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"amount": 60000, "currency": "brl", "payment_method": "pm_…", "confirm": true}'
```

[Testing payments](docs/api/testing.md) lists the test cards, and how a checkout page
hands a card to the vault without the merchant's server seeing it.

## Repository layout

```
cmd/                 one binary each: api, worker, vault, jupiterctl, and (from phase 5) sim-<name>
internal/<module>/   a domain module; its root package is its only public interface
internal/vault/      the card vault; its root package is the client, the rest only the vault imports
internal/money/      amounts, currencies, rates, rounding and allocation
internal/id/         prefixed, time-ordered identifiers
internal/platform/   process lifecycle, PostgreSQL and other shared infrastructure
internal/sim/<name>/ a simulator's code, isolated from Jupiter's domain
pkg/                 libraries meant for other projects too: webhook verification, the card network and 3-D Secure protocols, the load generator; later BR Code, CNAB 240
test/architecture/   the test that enforces these boundaries
test/e2e/            the golden path
test/pci/            the test that the API database never holds a card number
test/cardrail/       live payments through the card network and 3-D Secure simulators
test/risk/           a card-testing burst from the load generator
test/simulation/     the deterministic simulation
deploy/              configuration for the local infrastructure
tools/               pinned development tools
```

The boundaries are enforced, not conventional: a package that reaches into another
module's internals, a shared package that imports a domain, or domain code that imports a
simulator fails the build. The rules are in
[`test/architecture`](test/architecture/architecture.go) and the reasoning in
[ADR 0001](docs/adr/0001-modular-monolith-with-isolated-vault.md).

## Stack

Go · PostgreSQL · pgx + sqlc · River · oapi-codegen · moov-io/iso8583 · OpenTelemetry ·
testcontainers-go · Docker Compose · GitHub Actions
