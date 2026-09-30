# Testing payments

Test-mode keys (`sk_test_…`) pay through Jupiter's test rail, which never moves money
and answers according to the payment method and the amount.

## Test payment methods

| `payment_method` | Result |
|---|---|
| `pm_card_visa`, `pm_card_mastercard` | Approved |
| `pm_card_declined` | Declined: `card_declined` / `generic_decline` |
| `pm_card_insufficient_funds` | Declined: `card_declined` / `insufficient_funds` |
| `pm_card_authentication_required` | `requires_action`; complete it with the test helper below |

A decline answers 402 with a `card_error` that carries the payment intent, now back in
`requires_payment_method`. Confirm it again with another payment method: each try is a
new attempt.

## Amounts that misbehave

On an approving card, the last two digits of the amount in minor units choose a failure
the network could produce. The intent stays in `processing`, and Jupiter works out what
happened within about a minute; a `payment_intent.*` webhook reports the result.

| Amount ends in | What the rail does | How Jupiter resolves it |
|---|---|---|
| `91` (R$ 600.91) | Approves, but the answer is lost | Queries the rail and finds the approval |
| `92` | Never receives the first request | Finds nothing, sends it again with the same key |
| `93` | Answers late | Finds it pending, then approved |

An authorization whose outcome stays unknown for 15 minutes is reversed and the intent
returns to `requires_payment_method` with `authorization_unresolved`; the test rail
always answers eventually, so only a broken rail reaches that path.

## Completing an action

```sh
curl -X POST http://127.0.0.1:8080/v1/test_helpers/payment_intents/pi_…/complete_action \
  -H "Authorization: Bearer $SK_TEST" -d '{"outcome": "succeeded"}'
```

`succeeded` resumes the authorization; `failed` returns the intent to
`requires_payment_method`. Test helpers refuse live-mode keys.

## Live mode

Live mode has no rail until the card network connector (phase 5); confirming answers
`livemode_unsupported`.
