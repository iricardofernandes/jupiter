# 0038. MED claims: held, traced, answered, and contestable

- Status: Accepted
- Date: 2026-10-01

## Context

MED (Mecanismo Especial de Devolução) lives in the DICT, between participants. The
payer's bank reports an infraction; the receiving bank blocks the money and analyses the
claim; if it agrees, it returns the money with a devolução of nature `MED_FRAUDE`, which
the API Pix defines. MED 2.0 adds three things:
- it traces funds that moved on to other accounts;
- it blocks for up to 11 days;
- it requires the payer to be able to contest in their bank's app.

IN BCB 766/2026 gives the receiver 80 days to contest a return
([research §4](../research/notes/03-pix-and-boleto-specs.md)).

Jupiter receives Pix through its account at a bank ([ADR 0025](0025-pix-through-a-partner-bank.md)).
So a claim against one of its merchants reaches it from that bank, which the API Pix does
not cover. The research could not read the DICT's operational manual or the MED 2.0
resolution.

## Decision

**The bank relays the DICT** (`pkg/pixapi/med.go`, in the simulator's format after the
DICT's names):
- it notifies `{webhook}/infracoes` over the same mutual TLS as Pix notifications;
- it answers `GET /infracoes/{id}` and `GET /infracoes?inicio&fim`;
- it takes Jupiter's answer at `POST /infracoes/{id}/analise` and a contestation at
  `POST /infracoes/{id}/contestacao`.

A notification is read back before it is applied, and the worker reads the last
fortnight's claims every five minutes, for notifications that were lost.

**Held.** On a new claim Jupiter holds its amount on the merchant's balance, as far as the
balance has it: a ledger hold that payouts cannot spend. A merchant whose money already
left by payout has the rest traced: the payouts since the Pix, with their end-to-end ids,
go to the bank with Jupiter's answer. Holds must be applied within 30 minutes of the
bank's notice; the disputes check reports any applied later.

**Answered.**
- The merchant answers within three days: by accepting, or with evidence an operator
  weighs (`jupiterctl dispute decide <id> agreed|disagreed <operator>`).
- Agreeing asks the bank to return the amount held, which it does as a devolução
  `MED_FRAUDE`. Once the return is made, the hold is posted and the dispute is lost.
- Disagreeing ends the hold, and the dispute is won.
- A claim the merchant leaves unanswered is agreed with.
- One it answered that no operator decides before the 11-day block ends is disagreed
  with.
- A claim the payer made more than 80 days after the Pix is disagreed with at once.

**Contested.** A merchant whose money a claim returned may contest the return, once,
within the window in effect (30 days before IN 766, 80 after), by submitting evidence. If
the payer's bank upholds the contestation, the bank credits the amount back, and the
dispute is won with its funds reinstated. A contestation the bank refuses leaves the
dispute lost.

## Alternatives rejected

- **Agreeing whenever the merchant does not prove otherwise, without an operator.** It
  would make every answered claim a loss. The receiving institution is the one that
  analyses, and evidence is what a person weighs; the plan sets automated evidence
  generation as a non-goal.
- **Holding the whole claim regardless of the balance.** The balance cannot go below what
  the merchant has; what already left is followed by tracing, as MED 2.0 has the DICT do.
- **A return requested through `PUT /pix/{e2eid}/devolucao` with nature `MED_FRAUDE`.** The
  API Pix allows the nature, but in MED the return follows the receiving bank's
  analysis. Sending the analysis and the return separately would let one happen without
  the other.

## Consequences

- MED's windows are rows in the deadlines table: changing one is a new row with its date.
- The relay's format is the simulator's, flagged as such, until a bank's real interface
  or the DICT's manual can be read.
- Tracing reports the next hop. Following it further is the DICT's job, between other
  participants.
- A Pix whose claim arrives after it was refunded holds only what is left of it.
