# 0011. API conventions: keys and modes, versions, errors, lists

- Status: Accepted
- Date: 2026-09-30

## Context

Every merchant integration is written against these conventions once and is expensive
to change afterwards. The research found Stripe's the most copied and its `/v2`
namespace correcting several of v1's choices; Brazilian PSPs add little to the grammar
([research: PSP API design](../research/notes/05-psp-api-design.md)).

## Decision

**The contract is a file.** `api/openapi.yaml` is the source; the server's routing,
parameter binding and models are generated from it with oapi-codegen, CI fails when the
generated code is stale, and integration tests check responses against its schemas.

**Keys and modes.** A merchant has secret (`sk_`), publishable (`pk_`) and restricted
(`rk_`) keys, each for test or live mode (`sk_test_…`, `sk_live_…`). A key's mode decides
which data a request sees; every object carries `livemode`, and nothing crosses modes,
including idempotency keys. Keys are 256 random bits, shown once and stored as SHA-256.
Restricted keys carry scopes such as `events:read`, and a key cannot create one with a
scope it does not hold.

**Versions are dates**, pinned to the merchant when it is created and overridable per
request with `Jupiter-Version`. Handlers always build the current shape; on the way out,
a list of changes rewrites each object back through every version newer than the one
requested. Adding a breaking change means adding one entry to that list. Request
parameters follow the current version only: changes to them must be additive.

**Errors** have one envelope with `type` (who must act), `code` (what happened), `param`,
`message`, `request_id` and a `doc_url` into [`docs/api/errors.md`](../api/errors.md).
Unknown parameters are errors, never ignored.

**Lists** are cursor-paginated by object id (`limit`, `starting_after`, `ending_before`),
newest first, and return `has_more`. Identifiers are time-ordered (ADR 0007), so the
cursor is the id itself. `include[]` returns related objects inline, read with the
caller's own scopes.

**Bodies are JSON**; timestamps are Unix seconds.

## Alternatives rejected

- **Code-first OpenAPI** (generating the document from handlers). The document would
  describe whatever the code happened to do; spec-first makes the contract reviewable
  before it is built.
- **Form-encoded bodies** (Stripe v1). JSON is what v2 moved to and what every client
  library handles natively.
- **Stripe v2 page tokens.** Opaque tokens hide the cursor to allow changing the sort
  order later; with time-ordered ids, an id cursor is stable and easier to debug.
- **URL versioning (`/v2/...`).** It versions everything at once. Date versions let one
  change ship without forcing every merchant to migrate every endpoint.
- **Bcrypt or Argon2 for API keys.** They protect low-entropy passwords; a 256-bit random
  key cannot be brute-forced, and a slow hash would add latency to every request.

## Consequences

- The document, the generated code and the server must change together; CI enforces it.
- Old versions cost one downgrade function per breaking change for as long as a merchant
  is pinned to them.
- Livemode separation is enforced in every query by merchant and mode, not by separate
  databases, so a query that forgets the mode is a data leak; module queries take an
  owner value that carries both.
