# JupiterReconciliationBreaksAgeing

**Warning.** Metric: `jupiter_reconciliation_breaks_ageing`, by counterparty: breaks open
for more than a day.

## What it means

A movement on one side has had no match on the other for more than a day: Jupiter's
ledger against a card network, the SLC, the bank, the Pix bank or the registry. A record
that is only late resolves its break the day it arrives. One that stays open is missing, a
duplicate, a different amount, or a probable match waiting to be confirmed. The algorithm
is in [docs/reconciliation.md](../reconciliation.md).

## Impact

Jupiter cannot prove that money it believes moved did move, or it holds money it cannot
explain. Ageing breaks are what an audit asks about.

## Diagnose

```sh
jupiterctl reconcile report <day> live
```

The report lists each break, with its kind, key, score and reasons. Then look at both
sides:
- **missing_at_counterparty**: Jupiter's record has no line at the counterparty. Did the
  payout leave? Was the file imported?
- **missing_at_jupiter**: the counterparty lists a movement Jupiter does not have. Was a
  return or a credit not applied?
- **duplicate**: the counterparty listed the same movement twice. Ask them to correct it.
- **amount_mismatch**: the two sides disagree on the amount. One side is wrong.
- **probable_match**: two records whose keys look alike.

## Fix

- Confirm a probable match that is the same movement:
  ```sh
  jupiterctl reconcile confirm <break> <your name>
  ```
- Once the cause is settled with the counterparty, resolve the break with a note. The
  break's records are settled too and will not open another:
  ```sh
  jupiterctl reconcile resolve <break> <your name> "<what was done>"
  ```
- Never resolve a break only to clear the alert: the note is the audit trail.
