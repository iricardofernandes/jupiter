# JupiterNetworkForwardsPending

**Warning.** Metric: `jupiter_acquirer_forwards_pending`: reversals (0420) and advices
(0220, 0400) the card network has not acknowledged for more than 10 minutes.

## What it means

When the network does not answer a request in time, Jupiter reverses it. A capture or
void that gets no answer is advised instead. Either way, the message is repeated until the
network acknowledges it (ADR 0019): `acquirer.retry_forwards`, every 5 seconds, with
backoff up to 5 minutes.

## Impact

Until a reversal is acknowledged, the issuer may still hold the customer's funds for an
authorization Jupiter gave up on. Until an advice is acknowledged, the network may not
clear a capture.

## Diagnose

```sql
SELECT key, kind, state, stan, forward_stan, next_forward_at, created_at
FROM acquirer.exchanges WHERE next_forward_at IS NOT NULL ORDER BY created_at LIMIT 20;
```

- Is the connection to the network up? The worker logs connection errors from the
  acquirer connector.
- Many pending at once point at the network, or at the connection. One alone points at
  that message: look for a rejection in the logs.

## Fix

Restore the connection; the repeats carry on by themselves. Never delete an exchange: it
is the record of what was said to the network.
