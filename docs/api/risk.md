# Risk

Every payment attempt passes the risk engine before it reaches a rail. The decision is one
of `allow`, `review` (let it through, for the merchant to look at), `request_3ds` (ask the
cardholder to authenticate first) or `block` (refuse it: `card_declined` with
`decline_code: blocked_by_risk`). The payment intent shows it as `risk_decision`, and
`GET /v1/risk/decisions` is the log of every decision, with the rules that fired and the
features they saw ([ADR 0022](../adr/0022-risk-engine-rules-velocity-card-testing.md)).

## How a decision is made

1. **Lists.** If the card's fingerprint, the IP or the BIN is on the merchant's allow
   list, the merchant's block list, its own rules and the platform's reviews and 3-D
   Secure requests are set aside, but the platform's blocks and the card-testing
   throttle still apply. Otherwise, on the block list, the payment is blocked. `POST /v1/risk/list_items` adds an entry:
   `{"list": "block", "kind": "ip", "value": "203.0.113.9"}`.
2. **Rules.** The platform's rules, then the merchant's own, each an expression over the
   features below. The decision is the most severe action among those that fired.
3. **Card testing.** A merchant with at least 20 attempts in ten minutes, 20% or more of
   them declined, is throttled for 30 minutes: beyond three attempts a minute, payments
   are blocked.

## Features

| Feature | Meaning |
|---|---|
| `amount`, `currency` | In minor units, and the ISO code |
| `brand`, `bin` | visa, mastercard, amex, elo, hipercard; the first 6 or 8 digits |
| `ip` | `customer_ip` from the payment (an IPv4 or IPv6 address), or empty |
| `installments`, `off_session`, `livemode` | |
| `card_attempts_1h`, `card_attempts_24h`, `card_declines_24h` | This card's attempts and declines, at every merchant |
| `ip_attempts_1h`, `ip_cards_24h` | Attempts, and different cards, from this address at your account |
| `merchant_attempts_1m`, `merchant_attempts_10m`, `merchant_decline_ratio_10m` | Yours |
| `throttled` | Whether you are throttled for card testing |

## Platform rules

| Rule | Action | Expression |
|---|---|---|
| `card_velocity` | block | `card_attempts_1h >= 10` |
| `ip_many_cards` | block | `ip != '' && ip_cards_24h >= 5` |
| `card_declines` | request_3ds | `card_declines_24h >= 3 && !off_session` |
| `large_amount` | review | `amount >= 1000000` |

## Your rules

```sh
curl -X POST http://127.0.0.1:8080/v1/risk/rules -H "Authorization: Bearer $SK_TEST" \
  -d '{"action": "request_3ds", "expression": "amount >= 50000 && brand == \"elo\"", "description": "Authenticate large Elo payments"}'
```

An expression must use the features above and yield true or false; one that does not is
refused when it is created. Comparisons, arithmetic, `&&`, `||`, `!`, `in` a list, `?:`
and the string operators `contains`, `startsWith` and `endsWith` are allowed. Functions,
ranges, variables (`let`) and regular expressions (`matches`) are not, and an expression
is at most 200 terms: a rule runs inside every payment's transaction, so what it costs
must be bounded by its size. A rule that fails when it runs, such as
by dividing by zero, is skipped, and the decision log says so. `GET /v1/risk/rules` lists yours beside the platform's, which
cannot be deleted.
