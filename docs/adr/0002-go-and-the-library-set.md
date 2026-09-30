# 0002. Go, and the libraries Jupiter builds on

- Status: Accepted
- Date: 2026-09-30

## Context

A PSP backend is network-bound, concurrent and long-running, and it has to be read and
audited by people other than its author. The research found the payments ecosystem in Go
unusually strong where Jupiter needs it (moov-io's ISO 8583 stack, pgx, River) and absent
where Jupiter intends to contribute (CNAB, a Pix settlement simulator)
([research: open source](../research/notes/07-open-source-landscape.md)). Go runs payments
at scale in production: Monzo's bank and Mercado Libre's platform are the published
examples ([research: engineering writeups](../research/notes/06-engineering-writeups.md)).

Every dependency is code Jupiter must trust with money, so each one needs a reason.

## Decision

Go, at the latest stable release (1.27 at the time of writing), with:

| Need | Choice | Why this one |
|---|---|---|
| PostgreSQL driver | `jackc/pgx/v5` | Native protocol, `COPY`, batch queries and precise error codes, which the ledger's constraint handling depends on |
| Queries | `sqlc` (from phase 1) | SQL stays SQL and is checked against the schema at generation time; no ORM hides a lock or a query plan |
| Background jobs | `riverqueue/river` (from phase 2) | Jobs are rows enqueued **in the same transaction** as the state change, which gives the outbox pattern without an outbox |
| HTTP API | `oapi-codegen` (from phase 2) | The OpenAPI document is the source; the server is generated from it |
| ISO 8583 | `moov-io/iso8583`, `iso8583-connection` (from phase 5) | Maintained, Apache-2.0, and handles framing, matching and echo |
| Observability | OpenTelemetry → Collector → Jaeger / Prometheus / Grafana | Vendor-neutral; the Collector is the only thing a real deployment would change |
| Tests | stdlib `testing`, `pgregory.net/rapid`, `testcontainers-go` | Property tests that shrink failures; integration tests against real PostgreSQL, not mocks |
| UUIDs | standard library `uuid` (Go 1.27) | Monotonic UUIDv7 with no dependency (ADR 0007) |
| Lint and vulnerabilities | `golangci-lint` v2, `govulncheck` | Pinned in `tools/go.mod`, so every machine runs the same versions |

Tools are pinned in a separate module (`tools/go.mod`) and run with `go tool
-modfile=tools/go.mod`, so their dependency graphs never enter Jupiter's.

## Alternatives rejected

- **Temporal for payment state machines.** Durable execution solves timeouts and retries
  well, and Coinbase and Uber use it or its predecessor. But it adds a cluster to operate
  and hides the state machine this project is meant to show. Payment state machines are
  explicit Go code driven by River jobs; phase 3 records the full comparison.
- **An ORM (GORM, ent).** The ledger's correctness lives in constraints, locks and
  isolation levels, all of which an ORM makes harder to see and to control.
- **A general message broker (Kafka, RabbitMQ) for internal events.** Nothing in a
  modular monolith needs one; River gives transactional enqueue on the database already
  there.
- **`shopspring/decimal` for money.** Its last release is from April 2024, and decimal
  arithmetic for amounts invites the precision bugs ADR 0003 exists to prevent. The
  linter now forbids importing it.

## Consequences

- One language and one database for everything except the observability stack.
- River is MPL-2.0: Jupiter can use it unmodified; changes to River's own files would have
  to be published.
- Decisions marked "from phase N" are made here but verified when the phase arrives; a
  phase that finds one wrong writes a superseding ADR.
