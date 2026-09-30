# 0001. A modular monolith, with the card vault as its one separate service

- Status: Accepted
- Date: 2026-09-30

## Context

One card payment touches the payment intent, the ledger, the split between recipients
and, for an installment sale, the receivable units that the registry will track for
months. Each of those writes has to commit or fail together: a capture recorded without
its ledger posting, or a split without its receivables, is money Jupiter can no longer
account for.

Card data is different. Every system that stores, processes or transmits a full card
number is in PCI DSS scope, and so is everything connected to it without segmentation
([research: card rails](../research/notes/04-card-rails.md)). Scope is the largest cost a
PSP's architecture controls.

## Decision

Jupiter's API and worker are one Go module deployed as two processes over one PostgreSQL
database. Domains are modules inside it, `internal/<module>`, and a module's root package
is its only public interface. An architecture test (`test/architecture`) fails the build
when a package imports another module's internals, when the shared kernel imports a
domain, or when anything reaches a simulator except over the wire.

The vault is the exception. It is its own binary (`cmd/vault`) with its own database,
reached only over mutual TLS, and the architecture test forbids it from importing any
domain module. Card numbers enter Jupiter only through it; everything else holds tokens.

## Alternatives rejected

- **Microservices per domain.** They would turn the intent–ledger–split–receivables write
  into a distributed transaction: sagas, compensations and an outbox between every pair
  of services, to solve a scaling problem a PSP of this size does not have. The
  consistency guarantee this project exists to demonstrate would become the hardest thing
  to keep.
- **A monolith that includes the vault.** Simpler to build, but it puts the API database,
  the worker and every log line in PCI scope, and it makes "the API database never holds
  a card number" a convention instead of a property that can be tested (phase 4 tests it).
- **Modules enforced by convention only.** Go's `internal/` rule protects the repository
  from other repositories, not modules from each other. Without the architecture test,
  boundaries erode one convenient import at a time.

## Consequences

- A payment's state changes commit in one PostgreSQL transaction, and background work is
  enqueued in that same transaction (ADR 0002).
- Modules must expose constructors and interfaces from their root package, including the
  ones `cmd/` needs for wiring. That is some ceremony per module.
- A module could still be extracted into a service later: its public interface is already
  the only way in.
- The vault needs its own deployment, certificates and key management from phase 4 on.
