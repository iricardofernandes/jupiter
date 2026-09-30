# 0026. A Pix payment waits on a charge; refunds are returns; stray Pix go back

- Status: Accepted
- Date: 2026-09-30

## Context

A Pix is pushed by the payer from their own bank. The receiver cannot pull it, only show
what to pay and wait. The payer's bank authenticates the payer, and the money arrives
settled, in seconds. A receiver can return all or part of a Pix it received (a
devolução). The plan asks for Pix as a rail of the payment intent, `requires_action` with
the QR code until the bank says it was paid. It also asks for an unknown-outcome path
for a notification that never arrives, and for returns mapped to refunds
([research §2, §4](../research/notes/03-pix-and-boleto-specs.md)).

## Decision

**The payment method `pix`.** Confirming an intent with it creates an attempt and, at the
bank, a charge named by the attempt. The charge is immediate (`cob`, payable for
`expires_after_seconds`, a day by default) or has a due date (`cobv`, with the payer
named, and an optional fine, interest per month, and discount until a date). The intent
waits in `requires_action` with `next_action.pix_display_qr_code`: the BR Code, and until
when it can be paid. The risk engine and 3-D Secure do not run: they are for cards.

**Paid.** A Pix received for the attempt's charge settles it in one transaction, even one
whose creation Jupiter has not recorded yet because its answer was lost: the txid names
the attempt. The Pix is posted
from Jupiter's Pix account to the merchant's balance, attempt captured, intent
`succeeded` (a new transition, `requires_action → succeeded`, for payments completed
outside Jupiter). A due-date charge paid late brings its fine and interest, so
`amount_received` may exceed `amount`. The database's check allows that for Pix only.
The bank computes what is due, but Jupiter still bounds it. A Pix for an immediate charge
must be its amount. For a due-date charge it must fall between the amount less its
discount and the amount plus its fine and the interest of every day the charge can be paid
late. A Pix outside the bounds is held apart and returned, like a stray.

**Not paid.** Expiry is not a status at the bank; Jupiter derives it. After a charge
expires and a five-minute grace for Pix in flight, a job asks the bank. If the charge was
paid, it applies the Pix, read from `GET /pix/{e2eid}`. If not, it fails the attempt
(`payment_intent_payment_attempt_expired`). The customer may confirm again, which makes a
new attempt and a new charge. A charge whose creation stays unanswered is read back;
after 15 minutes it is removed and the attempt failed.

**Canceled.** Canceling an intent waiting on a Pix first removes the charge at the bank:
until then the customer can still pay it. A charge found already paid wins, and the intent
succeeds.

**Stray Pix go back.** Some Pix arrive that no current attempt wants: a Pix without a
txid sent to Jupiter's key, one that paid a charge whose attempt already expired or was
canceled, or anything else the bank reports. Such a Pix is posted to a held-apart account
(`pix_unmatched`) and returned to the payer by a job, with a return named after it.
Jupiter never keeps money it cannot attribute. A return that fails stays held for someone
to look at.

**Refunds are returns.** Refunding a Pix payment asks the bank to return that amount of
the Pix that paid it, named by the refund. A refund stays `pending` until the bank
reports the return, by notification or when the resolver asks. It then succeeds, posted
from the merchant's balance back out of the Pix account, or fails (`pix_return_failed`).
Returns of one Pix never add up to more than it: the refund rules already ensure that.

**The invariants** extend to Pix: every intent agrees with its attempt, and the merchant's
balance with what payments received, refunded and paid out. The held-apart account holds
exactly the Pix waiting to be returned.

## Alternatives rejected

- **Polling each charge until paid.** One request per waiting payment per interval.
  Notifications and the period listing do it for less.
- **Expiring by removing the charge.** Removal is for cancellation. An expired charge
  cannot be paid anyway, and asking the bank once at expiry is enough to catch one paid
  at the last second.
- **Keeping stray Pix as merchant revenue.** With no charge there is no merchant to credit,
  and keeping a payer's money without cause is not an option.
- **A PaymentMethod object for Pix.** Nothing about the payer is saved or reused; a
  reserved value `pix` is enough.

## Consequences

- Pix in test mode needs the bank's sandbox; without one, confirming with `pix` answers
  `livemode_unsupported`.
- `amount_received` can exceed `amount` for a Pix with a due date; integrations must read
  what was received.
- Returns under MED (fraud) are phase 12's; only returns Jupiter asks for (`ORIGINAL`) are
  made here.
