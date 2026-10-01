# JupiterLedgerBalanceDrift

**Critical.** Metric: `jupiter_ledger_drifted_accounts`, sampled by the worker every
minute.

## What it means

An account's cached balance (`ledger.balances`) disagrees with the sum of its entries.
The checker has marked the account drifted, and the ledger now refuses to read or post
to it (`ErrBalanceDrift`), rather than act on a wrong number. ADR 0008 explains the cached
and batched balances.

## Impact

Every operation on that account fails: payments of the merchant it belongs to, payouts
from it, or, for a platform account, every payment in its mode.

## Diagnose

```sql
SELECT b.account_id, a.code, b.drifted_at FROM ledger.balances b
JOIN ledger.accounts a ON a.id = b.account_id WHERE b.drifted_at IS NOT NULL;
```

Compare the cached balance with what the entries say:
```sql
SELECT layer, sum(amount) FROM ledger.entries WHERE account_id = '<account>' GROUP BY layer;
SELECT * FROM ledger.balances WHERE account_id = '<account>';
SELECT count(*) FROM ledger.balance_queue WHERE account_id = '<account>';
```

A drift means a balance update was lost or applied twice. Look for the deploy or the
incident that happened around `drifted_at`.

## Fix

The repair trusts the entries. First make sure they are right: the ledger's invariants
hold (`jupiterctl ledger check` reports nothing else), and the account's entries are what
its postings say. A drift caused by a missing or wrong entry is a ledger bug: fix that
first, and have a second person review before repairing.

Then rebuild the account's cached balance from its entries:
```sh
jupiterctl ledger repair <account>
```

The repair locks the account and the batched-balance applier, recomputes the balance, and
lets the account be used again. Then run `jupiterctl ledger check`. Find the cause before
closing the incident: a drift is a bug.

## Escalate

If the account is a platform account, such as `network_receivable` or `pix_settlement`,
payments in that mode are stopped. Escalate at once.
