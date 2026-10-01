# JupiterAPIErrors

**Critical.** Metric: `jupiter_api_request_duration_seconds_count`, by
`http_response_status_code`: more than 1% of requests answered 5xx for 5 minutes.

## What it means

The API is failing requests on its side. A 5xx is never a merchant's mistake: those are
4xx.

## Impact

Merchants' payments, refunds or reads are failing. Their retries, with the same
`Idempotency-Key`, will succeed once the cause is gone, and do not duplicate anything.

## Diagnose

- Which routes fail?
  ```promql
  sum by (http_route, http_response_status_code) (rate(jupiter_api_request_duration_seconds_count{http_response_status_code=~"5.."}[5m]))
  ```
- The API logs each request with its `request_id`, and each failure with its cause. Search
  for `status` 500 or 503.
- The usual causes:
  - the database is unreachable or saturated
    ([JupiterDatabasePoolSaturated](JupiterDatabasePoolSaturated.md));
  - the vault is down (card numbers cannot be read or saved);
  - a deploy introduced a bug.

## Fix

Restore the dependency, or roll back the deploy. Requests that stopped between phases are
finished by the completer ([JupiterRequestsUnfinished](JupiterRequestsUnfinished.md)).
