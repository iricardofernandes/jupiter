# 0013. Thin events written with the change, delivered at least once by River

- Status: Accepted
- Date: 2026-09-30

## Context

Merchants learn about asynchronous outcomes, such as a Pix payment settling or a
dispute opening, from webhooks. A webhook system can lose events (the change commits
but the notification is never sent), invent them (the notification is sent for a change
that rolled back), deliver stale data, or be used to make Jupiter call internal
addresses. Stripe's design is at-least-once delivery with signed, thin events and
receivers that deduplicate ([research: PSP API design](../research/notes/05-psp-api-design.md)).

## Decision

- **Written with the change.** `events.Publish` takes the transaction of the change that
  caused it: the event row and one River job per subscribed endpoint are inserted in
  that transaction. An event exists exactly when its change commits, with no outbox
  table to poll.
- **Thin.** An event carries its type and a reference to the object, not a snapshot.
  Receivers fetch current state, so a late or out-of-order delivery never acts on stale
  data. `include[]=related_object` returns the object inline when reading events.
- **One body.** A webhook's body is the event exactly as `GET /v1/events/{id}` renders it,
  in the endpoint's API version; the API owns the format and the events module calls it.
- **Signed.** `Jupiter-Signature: t=<unix>,v1=<HMAC-SHA256("<t>.<body>")>`, verified by
  receivers with a five-minute tolerance. `pkg/webhook` implements both sides.
- **Rotation with overlap.** Rolling a secret keeps the old one signing beside the new
  one for up to seven days (one by default); during the overlap each request carries a
  signature per active secret.
- **Secrets sealed.** Signing secrets must be recoverable, so they are stored encrypted
  with AES-256-GCM under `JUPITER_SECRET_KEY`, bound to their endpoint and secret ids.
- **Retries.** A non-2xx answer, a timeout (ten seconds) or a connection failure is
  retried with exponential backoff from 30 seconds, capped at 12 hours per wait: twelve
  attempts over about 17 hours. Every attempt is recorded. Deliveries to a deleted or disabled
  endpoint are cancelled. `POST /v1/events/{id}/resend` enqueues a new delivery.
- **No SSRF.** The delivery client checks the resolved address of every connection and
  refuses loopback, private, link-local, shared (CGNAT), benchmarking, reserved and
  unspecified addresses, and the NAT64 and 6to4 prefixes that could reach them; it never follows
  redirects and ignores proxy settings. Live-mode endpoints must use HTTPS.
  `JUPITER_WEBHOOK_ALLOW_PRIVATE` lifts the address check for local development only.

## Alternatives rejected

- **Publishing after commit** (send, or enqueue, once the transaction returns). A crash
  between commit and publish loses the event forever.
- **A hand-written outbox table and poller.** River gives the same transactional insert
  plus retries, backoff, cancellation and a job table to inspect, and it was already in
  the library set (ADR 0002).
- **Fat events** (a snapshot of the object). Convenient, but a retried delivery arrives
  with data that may be hours old, and every schema change becomes a change to past
  events.
- **Checking the webhook URL's hostname against a blocklist.** DNS can resolve an
  innocent name to an internal address; only the address actually dialled can be
  trusted.

## Consequences

- Receivers must deduplicate by event id and tolerate any order; the webhooks guide
  says so.
- Webhook delivery depends on the worker running River.
- Every module that emits events needs the events service in its write path, and every
  event type is listed in `events.Types`.
