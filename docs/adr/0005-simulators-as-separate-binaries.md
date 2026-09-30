# 0005. Every counterparty is a simulator in its own binary

- Status: Accepted
- Date: 2026-09-30

## Context

Jupiter is not a licensed payment institution and connects to no real network. Yet the
parts worth demonstrating (timeouts that leave an authorization unknown, a late ISO 8583
response after a reversal, a Pix webhook that never arrives, a CNAB return file with a
rejected title) happen at the boundary with other companies. The research found no
maintained Go card-network simulator, no open 3DS2 access control server and only hobby
Pix settlement simulators ([research: open source](../research/notes/07-open-source-landscape.md)).

## Decision

Each counterparty is a simulator shipped in this repository, as its own binary
(`cmd/sim-<name>`, with code under `internal/sim/<name>`), and Jupiter reaches it only
over the protocol a real counterparty would use: ISO 8583 over TCP, the BCB Pix API over
mutual TLS, CNAB files, HTTPS webhooks.

The architecture test (ADR 0001) enforces the separation both ways. A simulator imports
only its own packages, `internal/platform` and `pkg/`; no Jupiter package outside `test/`
imports a simulator. Code both sides legitimately share, such as the BR Code codec or the
CNAB library, lives in `pkg/`, where it is public by design.

Each simulator's README separates the behaviour taken from a primary specification from
the behaviour the research could not verify.

## Alternatives rejected

- **In-process fakes behind Jupiter's interfaces.** Faster tests, but they would test
  Jupiter against its own assumptions. A fake that shares Jupiter's types cannot catch a
  wire-format mistake, a framing bug or a TLS misconfiguration.
- **One simulator binary for everything.** Simpler to run, but it lets simulators share
  state that real counterparties never share, such as the card network knowing what the
  bank did.
- **Third-party sandboxes (a real acquirer's or PSP's test environment).** They require
  commercial accounts, cannot be run in CI, cannot be made to fail on demand, and would
  make the project depend on someone else's uptime.

## Consequences

- Every protocol gets two implementations, Jupiter's side and the simulator's, which is
  more code, and exactly what verifies each against the specification's own examples.
- Fault injection (drop, duplicate, delay, respond late) lives in the simulators, where
  the simulation harness and the reconciliation tests can drive it.
- Nothing in Jupiter claims to be certified or connected; the simulators say so.
