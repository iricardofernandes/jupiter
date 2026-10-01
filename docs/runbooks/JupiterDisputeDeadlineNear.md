# JupiterDisputeDeadlineNear

**Warning.** Metric: `jupiter_disputes_due_soon`: disputes in `needs_response` whose
merchant deadline (`due_by`) is less than 48 hours away.

## What it means

A chargeback or MED claim is waiting for the merchant to submit evidence or accept it. The
merchant's deadline is the network's minus a two-day margin (ADR 0036). When it passes,
the worker answers for the merchant: a chargeback with whatever evidence was saved, a
pre-arbitration by accepting it.

## Impact

A merchant who does not answer usually loses the dispute and its amount. Nothing in
Jupiter breaks.

## Diagnose

```sql
SELECT id, merchant_id, kind, reason, amount, due_by FROM disputes.disputes
WHERE status = 'needs_response' AND due_by < now() + interval '48 hours' ORDER BY due_by;
```

## Fix

Tell the merchant: through their account contact, and through the `dispute.*` webhooks,
which they may not be reading. Jupiter does not answer for them before the deadline.
