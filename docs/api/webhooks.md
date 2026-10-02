# Receiving webhooks

Jupiter sends an event to each webhook endpoint subscribed to its type, as an HTTP POST
with a JSON body: the same event object `GET /v1/events/{id}` returns, rendered in the
endpoint's API version.

An account has at most 16 endpoints in each mode.

## Verify every request

Each request carries a signature header:

```
Jupiter-Signature: t=1790780646,v1=25d19eca3225c3497205…
```

`v1` is the hex HMAC-SHA256 of `"<t>.<raw request body>"` keyed with the endpoint's
secret (`whsec_…`, returned once when the endpoint is created or its secret rolled).
Go receivers can use `pkg/webhook`:

```go
err := webhook.Verify(body, r.Header.Get(webhook.SignatureHeader), secret, webhook.DefaultTolerance, time.Now())
```

Verify against the raw body, before parsing it. Reject timestamps more than five minutes
from your clock: that stops a captured request from being replayed later.

## Expect duplicates

Delivery is at least once: a request that timed out may still have been processed, and
`POST /v1/events/{id}/resend` delivers again on purpose. Record the ids of events you
have handled and acknowledge a repeat without acting on it again.

## Expect any order

Retries mean events can arrive out of order. Events are thin: they say what changed,
not what it looks like now. Fetch the object from `related_object.url`, or ask for it
inline with `include[]=related_object`, to act on its current state.

## Answer quickly

Answer 2xx within ten seconds and do the work afterwards. Any other answer, a timeout
or a refused connection is retried with exponential backoff, twelve attempts over about
17 hours. Redirects are not followed.

An endpoint that has taken no delivery for three days is disabled when an event's last
attempt to it fails. Fix it, then enable it again with `POST /v1/webhook_endpoints/{id}`
`{"status": "enabled"}`; events since can be sent again with `POST
/v1/events/{id}/resend`.

## Rotating the secret

`POST /v1/webhook_endpoints/{id}/roll_secret` issues a new secret. The old one keeps
signing alongside it for `expire_current_in` seconds (a day by default), so requests
carry two `v1` signatures and a receiver holding either secret accepts them. Deploy the
new secret, then let the old one expire.
