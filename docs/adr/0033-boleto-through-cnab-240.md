# 0033. Boletos go to the bank in CNAB 240 files, and their returns settle the intent

- Status: Accepted
- Date: 2026-10-01

## Context

A boleto is a bill the payer pays at any bank, app or lottery outlet. Every boleto is
registered in Núclea's central base before it can be paid. The beneficiary's bank registers
it, from the remittance files the beneficiary sends; it reports what happened in return
files ([research §5](../research/notes/03-pix-and-boleto-specs.md)).
- **The files.** Both are FEBRABAN's CNAB 240, v10.11: fixed-width records of 240
  characters, in batches.
- **The hybrid boleto.** A boleto can carry a Pix QR code. Distribution code `P` or `Q`
  asks the bank to register one; the return's segment Y-03 carries its location; a
  payment by Pix comes back as a settlement through channel 61.
- **The research gaps.** It found no maintained Go CNAB library, and no per-bank free-field
  layouts.

The plan asks for a CNAB 240 library of its own (segments P, Q, T and U, typed codes, a
`BankProfile`, fuzz tests), the barcode and typed line across the February 2025 factor
rollover, the hybrid boleto, and a bank simulator.

## Decision

**`pkg/cnab240`** writes remittances and parses returns of the collection layout. It
covers:
- the file and batch headers and trailers;
- segments P, Q and Y-03 in remittances, and T, U and Y-03 in returns;
- every field at its FEBRABAN position.

Reading is as strict as writing:
- every record is 240 characters;
- sequence numbers and counts must match;
- a numeric field holds only digits;
- text is printable ASCII.

Movement, occurrence and reason codes are typed values. Two fuzz targets feed the parsers
arbitrary bytes, and anything they accept must write back byte for byte.

A `BankProfile` numbers the titles and lays out the 25-digit free field. The FEBRABAN
profile is Jupiter's own generic choice, since no bank's was available:
- **nosso número:** 11 digits plus a mod-11 check digit;
- **free field:** branch, nosso número, account, and the portfolio digit.

**`pkg/boleto`** builds and parses:
- **the barcode:** mod-11 general check digit;
- **the typed line:** mod-10 field check digits;
- **the due-date factor:** days since 1997-10-07, back to 1000 on 2025-02-22 and every
  9000 days after. A factor is decoded to the date of its cycle nearest a reference date;
  the research found no FEBRABAN rule for that.

**In payments**, a boleto is a payment method, as the research recommended:
1. **Confirm.** Confirming an intent with `payment_method: "boleto"` and `boleto` terms
   (due date, days payable after it, payer) issues it. The intent waits in
   `requires_action`, with `next_action.boleto_display_details` showing the line, the
   barcode and the due date.
2. **Register.** Once registered, it also shows the Pix code, built from the location the
   bank returned.
3. **Paid.** A payment reported in a return posts the amount received from Jupiter's
   account at the bank to the merchant's balance, and the intent succeeds.
4. **Written off.** A boleto the bank writes off after its term fails the intent with
   `boleto_expired`.

There are no refunds, as with Stripe's boletos; boletos have no fee.

**A cancel** stays `processing` until the boleto is gone:
- **Never sent.** It is canceled at the next remittance.
- **Already sent.** A write-off request goes to the bank, and the intent is canceled when
  the return confirms it.

**`internal/bank`**, the connector:
1. **Issue.** Numbers each boleto from a per-mode sequence, inside the transaction that
   records it.
2. **Remit.** Builds each remittance once and keeps it as built, so one whose answer was
   lost is sent again the same; the bank takes a file again by its number without
   applying it twice.
3. **Import.** Reads return files in order, each in a transaction of its own that records
   it as read. A file must be the one after the last read, and say so in its header; a
   gap stops the import rather than skipping files.
4. **Check.** A title's status only moves forward. A payment must be for the boleto's
   amount, to Jupiter's account, of a boleto it issued that still waits; anything else
   is recorded in `bank.exceptions` for an operator, never posted. A payment that does
   not match is money at the bank all the same.

**`sim-bank`** stands for the bank:
- it checks and registers titles;
- it gives hybrid ones a Pix location;
- it takes payments by line, barcode or Pix code;
- it writes off what is past its term or asked to be;
- it writes a return file at each day's close.

## Alternatives rejected

- **A struct-tag marshaler (as rafaeljusto/gocnab).** The records are few and fixed; plain
  encode and decode functions per segment are easier to read against the layout tables and
  to make strict.
- **Boleto refunds by Pix or transfer.** They need the payer's account, which a boleto
  never gives. Stripe refuses them too.
- **CNAB 400.** It is bank-specific; the plan excludes it.

## Consequences

- The channel reason codes (`C047`) are FEBRABAN's. The rejection reasons the simulator
  gives (`08` nosso número, `16` due date, and the like) follow the common bank tables and
  were not checked against v10.11's list; the simulator's README says so.
- A boleto's money arrives at the bank the business day after payment. The ledger records
  it at the return, as the bank reports it. Reconciling that with the bank's statement is
  phase 13's.
- A title the returns never mention again stays registered here; the bank's write-off at
  the end of its term is what closes it.
