# Reconciliation

How Jupiter proves that what it believes matches what every counterparty reports. The
research found no public description of a three-way reconciliation algorithm for a
payment service provider, so this is Jupiter's, in full. The code is in
[`internal/reconciliation`](../internal/reconciliation), and the decision in
[ADR 0039](adr/0039-three-way-reconciliation.md).

## The three ways

A movement of money is recorded three times:
- in Jupiter's ledger;
- in the record of the rail it moved by: a clearing file, a settlement grade, a CNAB
  return;
- in the statement of the bank account it moved through.

Each pair must agree. A rail can confirm a movement whose money never reached the account.
An account can receive money no rail told Jupiter of. And either may not be in the ledger
at all.

Reconciliation works on **streams**: one record of one counterparty, read from both
sides.

| Counterparty | Stream | Jupiter's side | The counterparty's side | Key | Direction |
|---|---|---|---|---|---|
| `card_network` | `clearing` | Captures the network acknowledged and refunds it approved, dated by the UTC day sent | The records of its daily clearing files, as imported | RRN | capture in, refund out |
| `slc` | `grade` | Each settled grade, for its total | The SLC's grade of the day, once settled | the day | in |
| `bank` | `cnab_return` | Boletos whose payment Jupiter booked | The paid records of the return files it read | nosso número | in |
| `bank` | `statement` | Boletos booked, on the day their return says they are credited; what settled grades credited (`SLC/<day>`); payouts by bank transfer, and their returns | The bank's statement of the day | nosso número, `SLC/<day>`, transfer id | as the line says |
| `pix_bank` | `statement` | Pix received; refunds, strays and MED claims returned; payouts by Pix; MED returns given back | The Pix bank's SPI statement of the day | end-to-end id, return id, transfer id | as the line says |
| `registry` | `positions` | — | The divergences the registry reconciliation keeps open | what they are about | — |

Jupiter's side is read from the records its ledger postings name: a capture, a refund, a
payout or a grade is in it only once the ledger has posted its money. A boleto the bank
paid but Jupiter could not book, for example because its payment was no longer awaited, is
not on Jupiter's side. So it shows as a break, as it should.

A boleto shows the three ways: booked in the ledger, listed in a CNAB return, credited on
the statement. A card capture goes from the ledger to the clearing file, and its money
reaches the account in the day's SLC grade, which the statement credits. A Pix moves
through the Pix bank alone: its statement is both the rail's record and the account's.

## Records

Each side's record of a movement carries:
- **identity**, what names it on its side: Jupiter's object, or the counterparty's line
  (`2026-10-01/3`, a statement line's id). A line listed twice is two records;
- **key**, what both sides know the movement by;
- **direction**, in or out of Jupiter's account;
- **amount**, in centavos;
- **value date**: the day the counterparty records it, or, for Jupiter, the day it should
  appear there;
- the **merchant** it belongs to, when Jupiter's side knows.

Records are kept once read, with what they were matched to.

## A reconciliation

Reconciling a mode through day D, after the counterparties have closed it:

1. **Read.**
   - Each stream's counterparty side, for every day since the last reconciliation through
     D, and the three days before again, for what a counterparty adds to a day it had
     closed. On the first run, the week before D.
   - Each stream's Jupiter side, from 31 days before the first day read, so that a record
     a counterparty lists late still finds Jupiter's. On the first run, from the first
     day read: what came before has no counterparty side to match.
   - Records already kept are skipped, so reading a day again changes nothing.
2. **Match** each stream's unmatched records, with the rules below.
3. **Open** a break for each record left that is dated on or before D. A record dated
   after D is not late yet.
4. **Resolve** the open breaks whose records are matched now.
5. **Mirror** the divergences other reconciliations keep: the registry's. A new one opens
   a break, and one no longer listed is resolved.

The worker reconciles each mode through yesterday every hour. A record that arrives late
is therefore matched, and its break resolved, the day it arrives. One mode is reconciled
at a time, under an advisory lock, and at most a month of days per run: a mode far behind
catches up over several runs.

A stream that cannot be read whole is reported, and the others go on. It is not matched,
so a counterparty that does not answer opens no false break, and the run is not recorded
as done, so the next one reads those days again. A record that cannot be matched (no key,
no identity, no positive amount) is reported and left out; one side of a stream gives at
most 200,000 records per read.

## Matching rules

Within a stream, in this order:

1. **Exact.** The same key, amount and direction. Matched at once, whatever the dates: a
   record that arrives a day late is the same movement. This is the only rule that
   resolves anything without a person.
2. **Amount mismatch.** The same key and direction, another amount. A break holding both
   records.
3. **Duplicate.** A record whose key, amount and direction were matched already, in this
   run or an earlier one: the second line of a statement listed twice. A break on the
   extra record.
4. **Probable match.** Of what is left, one of Jupiter's records and one of the
   counterparty's whose keys are alike, of the same amount and direction, no more than two
   days apart. Keys are alike when one is the other cut short, or when, of one length (8
   or more), they differ in at most two characters: a mistyped or truncated reference,
   never just two movements of one amount on one day. A break holding both, for a person to confirm (`jupiterctl reconcile
   confirm`) or resolve.
5. **Missing.** Anything left: `missing_at_counterparty` for Jupiter's records,
   `missing_at_jupiter` for the counterparty's.

**Scores**, out of 100, say how alike two records are:

| Agreement | Points |
|---|---|
| The same key | 50 (20 if alike) |
| The same amount | 30 |
| The same direction | 10 |
| The same day | 10 (5 a day apart) |

Every break keeps its score and the reasons for it: "the same key", "amounts 10 apart",
"a day apart". An exact match scores 90 or more.

## Breaks

A break is `open` until it is resolved:
- `matched`: its record was matched by a later reconciliation. The record arrived late.
- `superseded by …`: its record became part of another break, such as a probable match.
- `fixed at the counterparty`: a divergence no longer listed.
- an operator's resolution (`jupiterctl reconcile resolve <break> <operator> <note>`), such
  as a duplicate the counterparty withdrew. Its records are settled as they are, and open
  no break again. Every resolution names who and why, and a
  merchant sees only how it ended: `matched`, `superseded`, `fixed_at_counterparty`,
  `confirmed` or `resolved_by_operator`.

A break's age is the days since it opened. Reports count open breaks by age: up to a
day, up to a week, up to a month, older.

## Reports

A day's report gives, per counterparty and stream:
- how many of Jupiter's records of that day matched, and for how much;
- the breaks open at the day's end, opened that day and resolved that day, with their
  ages.

Operators read every merchant's (`jupiterctl reconcile report <day> [live]`). Merchants
read their own (`GET /v1/reconciliation/reports/{date}`). A break no merchant's record is
on, such as money on a statement Jupiter knows nothing of, is Jupiter's to look into, and
no merchant sees it.

## What it does not do

- **Balances.** Reconciliation matches movements. It does not prove that a statement's
  closing balance equals the ledger's balance of the account; timing differences make that
  a separate check.
- **Networks' netting.** Refunds of settled installments and chargebacks credit
  `network_receivable`, and the network nets them from later settlements. No simulated
  file reports that netting, so it is not reconciled.
- **The API Pix's own records.** Pix received are read from the bank every five minutes
  and applied (phase 7). Here, the SPI statement stands for the Pix bank.
- **Closing books.** No accounting close or financial statements; that is an ERP's job.

## How it is tested

The simulators lose, duplicate and delay records on purpose:
- statement lines at the bank and at the Pix bank;
- return records at the bank;
- clearing records at the card network.

The integration tests check two things. Reconciliation finds exactly the breaks
injected, and no others. A delayed record's break resolves the day it arrives. The golden
path reconciles every counterparty and ends with no break open.
