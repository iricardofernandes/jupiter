# 0007. Identifiers are a type prefix and a UUIDv7

- Status: Accepted
- Date: 2026-09-30

## Context

Identifiers appear in API responses, webhook payloads, logs, support conversations and
reconciliation reports. A bare UUID says nothing about what it identifies, and pasting a
refund's identifier where a payment's is expected fails confusingly or, worse, succeeds.
Random UUIDv4 keys also scatter inserts across a B-tree index; Shopify measured time-ordered
keys inserting about 50% faster
([research: engineering writeups](../research/notes/06-engineering-writeups.md)).

## Decision

`internal/id` produces identifiers such as `pi_01k6c0q9a8f3v7w2x5y4z6b1cd`: a prefix of 1
to 16 lower-case letters naming the type, an underscore, and a UUIDv7 encoded as 26
lower-case Crockford base32 characters.

- The encoding is the one in the TypeID specification, and the tests include one of its
  published vectors. Jupiter's prefixes are stricter than TypeID's (letters only), so the
  separator is never ambiguous.
- UUIDv7 comes from the Go 1.27 standard library, which is monotonic within a process:
  identifiers sort in creation order as strings and as bytes, so cursor pagination and
  indexes both follow creation time.
- Each module declares its prefixes once (`var PaymentIntent = id.MustPrefix("pi")`), and
  `Prefix.Parse` rejects an identifier of another type.
- The zero value is not an identifier and refuses to serialize, so an unset identifier
  cannot reach the API as an empty string.

## Alternatives rejected

- **UUIDv4.** No ordering, so worse index locality and no creation-order pagination.
- **ULID.** Similar ordering and encoding, but a separate specification with its own
  monotonicity rules. UUIDv7 is an RFC (9562) and now in the standard library.
- **Database sequences.** They leak volume to anyone holding two identifiers, need a round
  trip to generate, and cannot be minted before the row exists, which idempotent retries
  and deterministic simulation both need.
- **`github.com/google/uuid` or a TypeID library.** No longer necessary with the standard
  library; the encoding is a few dozen lines, tested against the specification.

## Consequences

- Storage format (text, or `uuid` with the prefix implied by the column) is chosen with
  the first table, in phase 1.
- Identifiers reveal their creation time to the millisecond. For a PSP's objects that is
  acceptable and useful; nothing secret is encoded in them.
- Deterministic tests build identifiers from fixed UUIDs with `Prefix.FromUUID`.
