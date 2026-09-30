# 0028. Subscriptions charge by Pix Automático, one payment intent per cycle

- Status: Accepted
- Date: 2026-09-30

## Context

Pix Automático, live since June 2025, lets a receiver debit a payer periodically without
the payer acting each time, once the payer authorizes a recurrence. The receiver's PSP
offers it through the API Pix:
- recurrences (`rec`);
- requests pushed to the payer's bank (`solicrec`, journey 1);
- QR codes with a recurrence location (journeys 2 to 4);
- recurring charges (`cobr`).

Each charge carries a list of attempts: the first on the due date, and, if the recurrence
allows, up to three retries on different days within seven days of it. Each attempt has
its own endToEndId. The receiver's PSP sends a charge to the payer's bank inside a
scheduling window before its due date
([research §4](../research/notes/03-pix-and-boleto-specs.md); Manual de Padrões, Anexo
IV). The plan asks for a subscription abstraction on top. It also says Jupiter is a PSP,
not a billing system: no plans, prorations or invoices.

## Decision

**A subscription** (`/v1/subscriptions`) is:
- an amount;
- an interval (week, month, quarter, half-year, year);
- a first due date and an optional end;
- what the customer's bank shows them (35 characters);
- the customer, by name and CPF or CNPJ;
- whether retries are allowed;
- how the customer authorizes it: `qr_code` (journey 2) or `payer_request` (journey 1),
  with their bank account.

Journeys 3 and 4, which pay a first charge while authorizing, are not offered.

**Setting up.** Creating a subscription records it, incomplete, and asks the bank for the
recurrence: a location and the QR code, or a request to the customer's bank. The API Pix
has no idempotency here: `POST /rec` makes a new recurrence each time. So the recurrence's
contract is the subscription's id. A setup whose answer was lost is followed by a search
(`GET /rec` by the payer's CPF or CNPJ) for that contract before anything is made again,
and not before ten minutes, in case the listing lags. The worker repeats a setup that
failed. One the bank refuses, or a request the customer's bank cannot receive, rejects
the subscription.

**One at a time.** The API's setup, the hourly job and notifications all advance a
subscription; an advisory lock held while talking to the bank lets only one do it, so a
cycle is never charged twice and a retry never asked for twice. A merchant has at most
one subscription waiting for a given customer's authorization, so a customer's bank is
not flooded with requests.

**The bank decides the status.** The recurrence's status moves the subscription:
- approved makes it active;
- rejected, rejected;
- canceled makes it canceled, by the merchant or the customer, as the cancellation says;
- expired makes it ended.

Notifications (`{webhook}/rec`, `/cobr`) only say which subscription to look at again,
and an hourly look covers lost notifications.

**Cycles are payment intents.**
- **Scheduling.** Eight days before a cycle's due date, the subscription makes a payment
  intent paid by `pix_automatico`, whose attempt's charge is a cobr named by the attempt,
  like any Pix charge. A cycle authorized too late for the minimum notice is due as soon
  as it can be, within its cycle, as the manual allows.
- **Waiting.** The attempt waits, `scheduled`, in a processing intent: no one has anything
  to do.
- **Paid.** The Pix settles it through the same path as any Pix received, so the ledger,
  refunds and invariants are those of Pix.
- **Not paid.** A charge that expires or is rejected fails the intent back to
  `requires_payment_method`. One that is canceled cancels it.
- **Retries.** When an attempt was not debited and the subscription allows retries,
  Jupiter asks for the next one for the following day, the earliest the payer's bank
  takes. That is at most three, within seven days, which the bank enforces too.
- **Cycles follow their intents.** A cycle is paid, failed or canceled as its payment
  intent ends, whatever ended it: the bank's charge, a refusal to make it, or a Pix that
  paid something else and was returned. Cycles still in flight are followed after the
  subscription ends.
- **Past due.** The subscription is `past_due` while its latest finished cycle failed.

**Canceling** a subscription cancels the recurrence at the bank. The bank cancels the
charges not yet due to be debited. A charge whose first debit is today or later in its
retries goes on, since the manual allows a receiver to cancel a charge only until the day
before.

The exit criterion is a test (`test/pixrail`, `TestAYearOfMonthlyCharges`). It covers a
year of monthly charges in accelerated time for two customers, one by each journey:
- a payer without funds who pays on a retry, and one who never does;
- a cancellation by the customer in their bank's app, and one by the merchant.

Every day it checks that each cycle, its payment intent and the bank's charge agree, and
that the ledger balances.

## Alternatives rejected

- **A subscription paying one long-lived payment intent.** An intent is one payment. A
  cycle as an intent keeps refunds, events and the invariants unchanged.
- **Letting merchants create `pix_automatico` intents.** A charge needs an approved
  recurrence and a cycle of its own; only the subscription knows both.
- **Retrying on fixed days (D+1, D+3, D+5).** Asking for the next day as soon as an
  attempt fails is the earliest the rules allow, and the bank refuses what they do not.
- **Plans, prices and prorations.** Billing is out of scope; the amount is fixed per
  subscription.

## Consequences

- A cycle whose charge fails leaves an intent in `requires_payment_method`, which the
  merchant can collect another way.
- Some choices are the simulator's, not the specification's, and its README says so:
  - which rejection and cancellation codes are used for each case;
  - the scheduling window of 10 days before, with at least 2 days of notice (from the
    BCB's FAQ as the research summarized it);
  - business days without holidays.
