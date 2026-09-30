# Testing payments

Test-mode keys (`sk_test_…`) pay through Jupiter's test rail, which never moves money
and answers according to the card and the amount.

## Test cards

Save a card, then pay with the payment method it becomes:

```sh
curl -X POST http://127.0.0.1:8080/v1/payment_methods \
  -H "Authorization: Bearer $SK_TEST" -H "Idempotency-Key: $(uuidgen)" \
  -d '{"type": "card", "card": {"number": "4242424242424242", "exp_month": 12, "exp_year": 2030, "cvc": "123"}}'
```

| Number | Brand | Result |
|---|---|---|
| `4242424242424242`, `4000056655665556` | Visa | Approved |
| `5555555555554444`, `2223003122003222` | Mastercard | Approved |
| `378282246310005` | American Express | Approved |
| `6362970000457013` | Elo | Approved |
| `6062825624254001` | Hipercard | Approved |
| `4000000000000002` | Visa | Declined: `generic_decline` |
| `4000000000009995` | Visa | Declined: `insufficient_funds` |
| `4000002500003155` | Visa | `requires_action`; complete it with the test helper below |
| Any other valid number | | Declined: `test_mode_live_card`, as a real card in test mode is |

Any future expiry works. A security code is optional; American Express takes four
digits, the others three.

Sending the number to the API puts your server in PCI DSS scope. To keep it out, have the
checkout page send the card to the vault with your publishable key, and send Jupiter only
the token ([PCI scope](../pci-scope.md)):

```sh
# from the browser
curl -X POST http://127.0.0.1:8083/v1/tokens -H "Authorization: Bearer $PK_TEST" \
  -d '{"number": "4242424242424242", "exp_month": 12, "exp_year": 2030, "cvc": "123"}'
# from your server, within an hour
curl -X POST http://127.0.0.1:8080/v1/payment_methods -H "Authorization: Bearer $SK_TEST" \
  -d '{"type": "card", "card": {"token": "tok_…"}}'
```

## Test payment methods

Shortcuts that pay without saving a card first:

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

On an approving card, saved or a shortcut, the last two digits of the amount in minor units choose a failure
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

## Installments and stored cards

`installments: {"count": 6, "financed_by": "merchant"}` on a payment intent in BRL pays in
2 to 12 installments, financed by the merchant (parcelado lojista) or the issuer
(parcelado emissor). `setup_future_usage: "off_session"` on a payment with a saved card
stores it for later payments made without the customer; confirm those with
`off_session: true`. Both work in test and live mode.

## Live mode

Live keys (`sk_live_…`) pay over ISO 8583 through the card network, which in this
repository is the simulator (`go run ./cmd/sim-card-network`, then set
`JUPITER_CARDNET_ADDR`). Live mode takes saved cards only; without a network configured,
confirming answers `livemode_unsupported`. The network's issuer answers by card number:

| Number | Result |
|---|---|
| any valid number, such as `4242424242424242` | Approved up to a limit of R$ 100,000.00 |
| `4000000000000002` | Declined: `do_not_honor` |
| `4000000000009995` | Declined: `insufficient_funds` |
| `4000000000000069` | Declined: `expired_card` |
| `4000000000000119`, `4000000000000168`, `4000000000000127` | No answer in time, or a late one: reversed and declined with `issuer_timeout` |
| `4000000000000135` | Approved, answered twice: charged once |
| `4000000000000150` | Approved by the network in stand-in up to R$ 500.00; `issuer_not_available` above |

The simulator's [README](../../internal/sim/cardnetwork/README.md) has the details.

## 3-D Secure

`request_three_d_secure: "any"` asks the cardholder to authenticate; `automatic`, the
default, leaves it to the risk engine. In live mode the directory server simulator
answers by card number ([README](../../internal/sim/3ds/README.md)). Any card not below is
authenticated frictionless, and liability for fraud moves to the issuer.

| Number | Result |
|---|---|
| `4000000000003220` | Challenge: `requires_action` with `next_action.redirect_to_url`. Send the customer there; the code is `123456`. They return to your `return_url` with `?payment_intent=pi_…`, and the payment goes on by itself: watch for `payment_intent.succeeded` |
| `4000000000003238`, `4000000000003246` | Not authenticated or rejected: `payment_intent_authentication_failure` |
| `4000000000003253` | Unavailable: paid without authentication |
| `4000000000003261` | Attempted: paid, liability moved |

In test mode there is no directory server: a payment that asks for 3-D Secure waits in
`requires_action` for the test helper below.

## Risk

The risk engine decides on every payment; the intent's `risk_decision` says what it
decided. Send the customer's `customer_ip` for its address rules. [Risk](risk.md)
describes the rules, the lists and card-testing detection; `go run ./cmd/loadgen
-scenario card-testing` sends a burst that trips it.

## Pix

Pix in test mode goes to the bank's sandbox (`JUPITER_PIX_TEST_*`); the simulator plays
it locally. Confirm with the payment method `pix`, show the customer the BR Code, and pay
it as a customer would, through the simulator:

```sh
curl -X POST http://127.0.0.1:8080/v1/payment_intents -H "Authorization: Bearer $SK_TEST" \
  -H "Idempotency-Key: $(uuidgen)" -d '{"amount": 1500, "currency": "brl", "payment_method": "pix", "confirm": true}'
# next_action.pix_display_qr_code.data is the BR Code
curl -X POST http://127.0.0.1:8587/admin/pay -d '{"brcode": "<data>", "payer_tax_id": "12345678909"}'
```

The intent succeeds when the bank notifies Jupiter. [Accepting Pix](pix.md) covers due
dates, refunds and what happens to a charge nobody pays.

## Subscriptions

In the sandbox, the customer's side of [subscriptions](subscriptions.md) goes through the
simulator's controls:
- **authorize:** pay the QR code with `/admin/pay`, or accept or reject the request with
  `/admin/recurrence-requests/{id}/decide`;
- **run out of funds:** `/admin/payers/{tax_id}/funds`;
- **revoke:** `/admin/recurrences/{idRec}/cancel`.

The simulator's clock moves the charges every ten seconds (`/admin/tick` moves them at
once). Its [README](../../internal/sim/pix/README.md) lists the routes.
