# JupiterLedgerInvariantViolated

**Critical.** Metric: `jupiter_ledger_violations`, from the worker's `ledger.check` task
(every 5 minutes).

## What it means

The ledger's checker found something that must never happen. For example:
- a transaction whose entries do not sum to zero;
- an account below zero that must not go below zero;
- a pending transfer resolved twice;
- a hold past its expiry and its backstop.

The kinds are in `internal/ledger/check.go`.

## Impact

A balance Jupiter reports, pays out or settles may be wrong. Treat it as an incident
until it is explained.

## Diagnose

1. Run the check by hand. It prints every violation with its kind and subject, and exits
   1 if there is any:
   ```sh
   jupiterctl ledger check
   ```
2. The worker logs each one as `ledger invariant violated`, with `kind`, `subject` and
   `detail`. Find when it started:
   ```sql
   SELECT id, kind, created_at FROM ledger.transactions WHERE id = '<subject>';
   SELECT * FROM ledger.entries WHERE transaction_id = '<subject>' ORDER BY seq;
   ```
3. Find what posted it. A transaction's description names its source: an authorization,
   a capture, a refund, a payout, a settlement. Look that object up in its module's tables.

## Fix

- Do not edit or delete ledger rows: the tables refuse it, by design.
- A wrong posting is corrected by a new one that reverses it. Write it as code in the
  module that posted it, so it is reviewed, tested and replayable. Never post by hand.
- If the cause is a bug, add the failing case as a regression test and a simulation seed
  that reproduces it (`SIM_SEED`).
- Once fixed, `jupiterctl ledger check` must exit 0. The alert clears on the next check.

## Escalate

Always: involve whoever owns the module that posted the transaction, and finance if a
payout or settlement already used the wrong balance.
