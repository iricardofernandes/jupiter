# 0016. The card vault: its own process and database, reached only over mTLS

- Status: Accepted
- Date: 2026-09-30

## Context

ADR 0001 made the vault Jupiter's one separate service, because card data defines PCI
DSS scope. Phase 4 builds it. The research's vault design is a minimal service in its own
segment, reached over mTLS, that tokenizes and hands numbers back only to the component
that sends them to the acquirer ([research: card rails](../research/notes/04-card-rails.md)).
Two kinds of caller need it: Jupiter's API and worker, which save cards and authorize
with them, and a merchant's checkout page, which must be able to hand a card over without
the merchant's server or Jupiter's API ever seeing it.

## Decision

`cmd/vault` is its own binary with its own PostgreSQL database. It serves two listeners:

- **Internal, mTLS only.** Tokenize, describe a token, claim a token, detokenize. The
  server requires a client certificate from Jupiter's CA whose URI SAN is one of two
  identities (`internal/platform/mtls`): `spiffe://jupiter/api` may do everything;
  `spiffe://jupiter/worker`, whose rail calls and request recovery only read and claim
  cards, may not tokenize. No certificate, a certificate from another CA, or another
  identity is refused during the handshake, before any handler runs. Every
  detokenization is logged with its caller, token and owner, never the card.
- **Public, one route.** `POST /v1/tokens` takes a card from a web page, with a
  publishable key, and returns a token. It cannot read a card back, and each address is
  rate-limited.

Only the vault's own packages can decrypt a number: the architecture test forbids
anything outside `internal/vault` from importing its subpackages, and forbids the vault
from importing any domain module. The API sees the vault through `internal/vault`'s root
package, which is the HTTP client and the wire types.

A token is `tok_` and 122 random bits. A UUIDv7, as every other identifier uses (ADR
0007), would reveal when the card was saved.

The exit criterion is a test: callers without a certificate, with another identity, with
the right identity from another CA, or over plain HTTP all fail against a running vault,
and the public listener has no route to card data
(`TestTheVaultRefusesCallersWithoutTheAPICertificate`).

## Alternatives rejected

- **Encrypted columns in Jupiter's database.** The API database, its backups, replicas and
  every query tool that reaches it would be in scope, and "the API database never holds a
  card number" could not be tested; it could only be hoped.
- **A bearer secret instead of client certificates.** A secret travels in every request
  and ends up in configuration, proxies and logs; a private key never leaves its host.
  mTLS also authenticates the vault to its caller.
- **A third-party vault (VGS, Basis Theory, TokenEx).** The right call for a real
  company, but a portfolio that hands card data to a vendor demonstrates nothing about
  holding it.

## Consequences

- The vault has its own deployment, certificates, database and keys. `jupiterctl
  dev-certs` makes a development CA and the two certificates.
- The API and worker still hold a card number in memory: when a merchant saves one by
  number, on its way to the vault, and when the rail authorizes with a saved card. Their
  processes are therefore in scope; their database, logs and backups are not.
  [`docs/pci-scope.md`](../pci-scope.md) draws the line, and ADR 0018 covers how numbers
  pass through.
- A vault outage stops new cards and authorizations with saved cards, but nothing else;
  the API answers `vault_unavailable` and the rail treats it as an unknown outcome.
