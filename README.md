# Jupiter

A payment service provider backend in Go: the system behind an API like Stripe's or
Pagar.me's. It accepts payments by card, Pix and boleto, splits them between recipients,
carries them through settlement on a double-entry ledger, and pays merchants out.

This repository is the **backend only**.

> ### Current phase: **2 — API foundation**
>
> The ledger (phase 1) and the merchant API's foundation are in place: API keys with
> test and live modes and scopes, idempotency keys that survive a crash between phases,
> date-named versions, typed errors, cursor pagination, and signed webhooks delivered at
> least once. No payment exists yet; payment intents are phase 3. What each phase
> delivers, and what must be true before the next begins, is in
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
| [`docs/api/`](docs/api/) | Error codes and how to receive webhooks |
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
make migrate           # apply database migrations to it
make merchant NAME=x   # create a merchant; prints its API keys once
make ledger-check      # verify the ledger's invariants on it
make down
make help              # every target
```

`make up` starts PostgreSQL, the OpenTelemetry Collector, Jaeger, Prometheus and Grafana.
They listen on 127.0.0.1 only, on ports of their own so they can run beside other local
stacks:

| Service | Address |
|---|---|
| PostgreSQL | `postgres://jupiter:jupiter@127.0.0.1:55432/jupiter` |
| OTLP (gRPC / HTTP) | `127.0.0.1:54317` / `127.0.0.1:54318` |
| Jaeger | http://127.0.0.1:56686 |
| Prometheus | http://127.0.0.1:59090 |
| Grafana | http://127.0.0.1:53000 |

Each port can be changed in a `.env` file; see [`.env.example`](.env.example).

To run the API and the worker against it, export `JUPITER_DATABASE_URL` and a
`JUPITER_SECRET_KEY` (`openssl rand -base64 32`), then `go run ./cmd/api` and
`go run ./cmd/worker`:

```sh
curl -X POST http://127.0.0.1:8080/v1/webhook_endpoints \
  -H "Authorization: Bearer $SK_TEST" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"url": "https://example.com/hooks", "enabled_events": ["*"]}'
```

## Repository layout

```
cmd/                 one binary each: api, worker, vault, jupiterctl, and (from phase 5) sim-<name>
internal/<module>/   a domain module; its root package is its only public interface
internal/money/      amounts, currencies, rates, rounding and allocation
internal/id/         prefixed, time-ordered identifiers
internal/platform/   process lifecycle, PostgreSQL and other shared infrastructure
internal/sim/<name>/ a simulator's code, isolated from Jupiter's domain
pkg/                 libraries meant for other projects too (webhook verification; later BR Code, CNAB 240)
test/architecture/   the test that enforces these boundaries
test/e2e/            the golden path
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
