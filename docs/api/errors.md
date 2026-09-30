# API errors

Every error has the same envelope:

```json
{
  "error": {
    "type": "invalid_request_error",
    "code": "parameter_unknown",
    "message": "Received unknown parameter: colour.",
    "param": "colour",
    "request_id": "req_01k6c0q9a8f3v7w2x5y4z6b1cd",
    "doc_url": "https://github.com/iricardofernandes/jupiter/blob/main/docs/api/errors.md#parameter_unknown"
  }
}
```

`type` says who must act, `code` says what happened, and `request_id` (also sent as the
`Request-Id` header) identifies the request in Jupiter's logs.

| Type | HTTP status | Meaning |
|---|---|---|
| `authentication_error` | 401 | No valid API key |
| `permission_error` | 403 | The key lacks a scope the request needs |
| `card_error` | 402 | The card was refused, as invalid or by the issuer |
| `invalid_request_error` | 400, 404 | The request is wrong; repeating it will fail the same way |
| `idempotency_error` | 409, 422 | The Idempotency-Key conflicts with another request |
| `api_error` | 500, 503 | Jupiter failed; retrying with the same Idempotency-Key is safe |

## Codes

### api_key_invalid
The Authorization header is missing, malformed, or names a key that does not exist or was
revoked. Keys are sent as `Authorization: Bearer sk_test_…`.

### scope_missing
The key is valid but not allowed to do this. Publishable keys hold no scopes; restricted
keys hold only the scopes they were created with.

### api_version_invalid
The `Jupiter-Version` header names a version that does not exist.

### url_invalid
No endpoint exists at this method and path.

### body_missing
The request needs a JSON body and has none.

### body_invalid
The body is not a single valid JSON object, or is larger than 1 MB.

### parameter_unknown
The body has a parameter the endpoint does not accept. Jupiter refuses unknown
parameters rather than ignoring them, so a typo never goes unnoticed. `param` names it.

### parameter_invalid
A parameter has the wrong type or an invalid value. `param` names it when one
parameter is at fault.

### resource_missing
No object with that id exists for this merchant in this mode. Test-mode keys never see
live-mode objects and the reverse.

### idempotency_key_invalid
The `Idempotency-Key` header is longer than 255 characters.

### idempotency_key_in_use
Another request with the same key is still running and did not finish within ten
seconds. Retry later with the same key.

### idempotency_key_mismatch
The key was already used for a different request: another endpoint, another object, or
another body. A key names one request; use a new key for a new request.

### card_declined
The payment was declined. `decline_code` says why:

| decline_code | Meaning |
|---|---|
| `generic_decline`, `do_not_honor` | The issuer declined without saying why |
| `insufficient_funds` | Not enough credit or balance |
| `expired_card`, `incorrect_number` | The card is expired or its number is wrong |
| `transaction_not_allowed` | The issuer refuses this kind of payment, such as an off-session one on a card it has no agreement for |
| `issuer_not_available` | The issuer was down and the network could not stand in |
| `issuer_timeout` | No answer in time: the authorization was reversed, and the customer can pay again at once |
| `network_unavailable` | Jupiter could not reach the card network; nothing was sent |
| `test_mode_live_card` | A real card number in test mode |
| `invalid_account` | The saved card is gone from the vault |
| `blocked_by_risk` | The risk engine blocked it: the decision log says which rule |

The error carries the payment intent, back in `requires_payment_method`.

### payment_intent_authentication_failure
The cardholder failed 3-D Secure authentication, or the issuer rejected it. The payment
intent is back in `requires_payment_method`. It is also what a payment gets when 3-D
Secure could not be reached for 15 minutes: the payment waits, in `processing`, rather
than going on without the authentication that was asked for.

### payment_intent_payment_attempt_expired
The Pix charge expired before the customer paid it. The payment intent is back in
`requires_payment_method`; confirm it again for a new BR Code. A Pix that arrives for the
expired charge anyway is returned to the payer.

### payment_intent_payment_attempt_failed
The Pix charge could not be made. `decline_code` is `pix_charge_refused` when the bank
refused it, or `pix_charge_unresolved` when it did not confirm it within 15 minutes. The
payment intent is back in `requires_payment_method`.

### balance_insufficient
A payout asked for more than the balance has available: what payments posted, less
refunds, payouts and payouts still in flight.

### Refund and payout failures
A refund of a Pix payment that the bank could not return fails with `failure_reason`
`pix_return_failed` (for example, the payer's account is closed) or `pix_return_refused`.
A payout the bank could not make fails with `failure_code` `pix_transfer_failed` and the
bank's reason in `failure_message`; its amount is available again.

### incorrect_number
The card number fails its check digit, or has a length its brand does not use. `param` is
`card[number]`. The number is never repeated in the error.

### invalid_expiry_month
The expiry month is not between 1 and 12.

### invalid_expiry_year
The expiry year is not a four-digit year within the next twenty.

### expired_card
The card expired before the current month.

### invalid_cvc
The security code does not have the length the brand prints: four digits for American
Express, three for the others.

### vault_unavailable
The card vault did not answer. Nothing was saved; retry with the same Idempotency-Key.

### internal_error
Something failed inside Jupiter. Nothing half-done is left visible: retrying with the
same Idempotency-Key finishes the request or repeats it safely.
