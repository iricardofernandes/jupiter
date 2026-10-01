# JupiterJobsDiscarded

**Warning.** Metric: `jupiter_jobs_discarded`, by kind: jobs given up after their last
attempt in the past hour.

## What it means

A job failed every one of its attempts. For `webhook_delivery`, a merchant's endpoint
refused or did not answer an event on each of its 12 attempts, which back off over about
17 hours.

## Impact

That merchant never received those events by webhook. They can still read them with `GET
/v1/events`.

## Diagnose

```sql
SELECT id, kind, args, attempt, errors[array_length(errors, 1)] AS last_error, finalized_at
FROM river.river_job WHERE state = 'discarded' AND finalized_at > now() - interval '1 hour'
ORDER BY finalized_at DESC LIMIT 20;
```

The job's `args` name the event and the endpoint, and its last error says what the
endpoint answered.

## Fix

- Tell the merchant that their endpoint is failing.
- Once they have fixed it, they can have the events sent again with
  `POST /v1/events/{id}/resend`.
- Many merchants at once point at Jupiter's side instead: outbound network, DNS, or the
  signing secret.
