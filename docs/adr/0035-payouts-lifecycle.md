# 0035. Payouts by Pix or bank transfer, held, returned and scheduled

- Status: Accepted
- Date: 2026-10-01

## Context

Payouts went out by Pix only ([ADR 0027](0027-payouts-by-pix.md)), on demand. The plan
asks for payouts that are:
- scheduled and on demand;
- by Pix or by bank transfer;
- on a lifecycle that includes a returned payout, minimum amounts and holds.

Recipients already have a destination (a Pix key or a bank account), verified with them,
and transfer settings: manual, daily, weekly or monthly.

## Decision

**Methods.** A recipient is paid out to its own destination, by Pix or by bank transfer
through Jupiter's bank (`sim-bank`). The merchant's own payouts stay Pix only. A bank
transfer is named by the payout, so asking again answers the same transfer.

**Minimums** are Jupiter's policy, by method: R$ 1.00 by Pix, R$ 10.00 by transfer.

**Holds.** An operator can hold a recipient's payouts (`jupiterctl recipient
hold-payouts`):
- new ones wait `held`, their amount held on the ledger;
- releasing them (`release-payouts`), only for a verified recipient, lets the worker send
  them.

The merchant's own recipient is verified with the merchant and is not verified again when
its destination changes. A new destination for it holds its payouts instead, since a
stolen key could otherwise send the merchant's money anywhere.

A bank transfer refused outright (400 or 422) fails and releases its amount. Any other
error may hide a transfer that was made, so the payout waits for the bank to be asked.

**Returned.** A paid bank transfer may come back from the receiving bank, for instance from
a closed account. For seven days after one is paid, the worker asks after it; once
returned:
- the amount goes back to the account it was paid from, through Jupiter's account at the
  bank;
- the payout is `returned`, with the bank's reason;
- `payout.returned` is sent.

**Scheduled.** On the days a recipient's transfer settings name, moved to the next
business day, the worker pays out its whole available balance, at most once a day: a
unique index on the recipient and the day enforces it.

## Alternatives rejected

- **Holding by withholding the recipient's verification.** Verification is about who the
  recipient is; a hold is a risk decision about its money, and lifting it must not need a
  new verification.
- **Returned as a new incoming payment.** The money is the same payout coming back; tying
  it to the payout keeps the recipient's balance history readable and the payout's status
  true.
- **Pix payouts that can come back.** A Pix sent is final; returns are by a new Pix (MED),
  which is phase 12's.

## Consequences

- A bank transfer paid more than seven days ago is no longer watched: a later return
  arrives in the bank statement, for phase 13's reconciliation.
- A scheduled payout below the minimum is not made; the balance waits for the next day.
- `held` counts as in flight in the merchant balance invariant.
