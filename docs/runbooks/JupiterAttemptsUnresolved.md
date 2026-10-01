# JupiterAttemptsUnresolved

**Warning.** Metric: `jupiter_payments_attempts_unresolved`: card attempts in
`authenticating`, `authorizing`, `capturing`, `voiding` or one of their `_unknown` states
for more than 15 minutes.

## What it means

The rail's answer was lost, or never came, and the resolver has not settled it. The
resolver (`payments.resolve`, every 15 seconds) asks the rail about every operation in
flight for more than a minute. An authorization still unknown after 15 minutes is reversed
(ADR 0014). Anything still open after that means the resolver is not running, or the rail
does not answer it either.

## Impact

The customer's payment shows `processing`. Funds may be held on the card with no capture,
and the merchant cannot capture, cancel or refund until the attempt is resolved.

## Diagnose

```sql
SELECT a.id, a.status, a.updated_at, a.unknown_since, i.livemode
FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
WHERE a.status IN ('authenticating', 'authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
  AND a.updated_at < now() - interval '15 minutes'
ORDER BY a.updated_at LIMIT 20;
```

- Is `payments.resolve` running? Look for
  [JupiterTaskFailing](JupiterTaskFailing.md) or [JupiterTaskStalled](JupiterTaskStalled.md).
- Live mode: is the card network reachable? Look at the acquirer's exchanges:
  ```sql
  SELECT key, kind, state, updated_at FROM acquirer.exchanges WHERE key = '<attempt id>';
  ```
  See also [JupiterNetworkForwardsPending](JupiterNetworkForwardsPending.md).
- `authenticating` waits for the cardholder's 3-D Secure challenge. These normally fail
  after the challenge's expiry. Many at once point at the directory server.

## Fix

- Restore what the resolver needs: the worker, the network connection, the vault.
- Once the resolver runs, it settles everything on its own. Do not change attempt
  statuses by hand: the ledger hold and the rail must move with them.
