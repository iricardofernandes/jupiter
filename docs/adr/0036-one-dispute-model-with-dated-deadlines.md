# 0036. One dispute model for chargebacks and MED, moved on by dated deadlines

- Status: Accepted
- Date: 2026-10-01

## Context

Two mechanisms take money back after a payment:
- **Card chargebacks.** The issuer disputes a completion. The acquirer represents with
  evidence; the issuer may reject that into pre-arbitration; arbitration decides.
  Mastercard gives about 45 days to represent and 30 + 30 for pre-arbitration. Visa's
  figures could only be read secondhand
  ([research §6](../research/notes/04-card-rails.md)).
- **MED claims on Pix.** MED 2.0 has been mandatory since February 2026. The payer
  contests in their bank's app, and the receiving bank is notified within 30 minutes. It
  blocks the funds for up to 11 days and returns them if the fraud is confirmed. Since IN
  BCB 766/2026 the receiver has 80 days to contest a return
  ([research §4](../research/notes/03-pix-and-boleto-specs.md)).

Res. BCB 522/2025 limits participants' liability to disputes opened within 180 days of
authorization. The research read only the draft, not the final text.

Every one of these windows has changed in the last year, or may change. Jupiter's
principle is that rules regulation can change are data with an effective date
([ADR 0006](0006-regulatory-rules-as-dated-configuration.md)).

## Decision

**One model.** A dispute is either a chargeback or a MED claim on a captured payment.
- **Stage:** `chargeback`, `pre_arbitration` or `arbitration` for cards; `med_analysis` or
  `med_contestation` for Pix.
- **Status:** `needs_response` (the merchant can act until `due_by`), `under_review`
  (waiting on the network, the bank or an operator), `won` or `lost`.
- **Funds:** what was done to the money: `held`, `withdrawn`, `reinstated` or `released`.

Each change lands in the dispute's history, and merchants see the same object and events
for both kinds.

**Deadlines are a dated table** (`internal/disputes/rules/deadlines.csv`), keyed by
network and stage. Each row carries:
- its effective date;
- its length in calendar days, business days or minutes;
- whether a source the research read backs it (`cited`), it does not (`unverified`), or
  it is Jupiter's own choice (`policy`).

A lookup takes the rows in effect when the clock starts and prefers the exact network to
`*`. The merchant's own deadline is the network's minus a two-day margin (`policy`), so
Jupiter has time to send the response.

**Nothing waits on a person.** Every minute the worker:
- answers for a merchant whose `due_by` passed: a chargeback with the evidence it saved,
  if any; anything else by accepting, as a pre-arbitration left unanswered is accepted;
- disagrees with a MED claim the merchant answered but no operator decided before its
  block ended;
- sends what is pending, retrying after a minute;
- asks the network or the bank about disputes that are quiet for an hour or past their
  deadline.

What Jupiter must send is recorded with the change that requires it and sent after
commit (the outbox pattern), so a crash between the two loses nothing.

**Liability cap.** When a chargeback arrives later than the cap after authorization,
Jupiter:
- takes nothing from the merchant;
- marks the liability `scheme`;
- represents citing the cap.

The network simulator accepts that representment when it agrees on the dates.

**Card networks are reached over HTTP**, the way their dispute systems are (Visa's VROL,
Mastercard's Mastercom), in the simulator's format (`pkg/cardnet/disputes.go`):
- the network signs dispute events with a secret of their own;
- an event is only a hint: Jupiter reads the case back before applying it;
- a version orders changes, and a stale one changes nothing;
- the acquirer's requests carry a bearer token;
- a listing of recently changed cases covers events that never arrived.

**Test mode** has no network. A test network kept in Jupiter's database plays the issuer
from keywords in the evidence:
- `winning_evidence` wins;
- `losing_evidence` goes to pre-arbitration;
- anything else is won when the issuer's time runs out.

`POST /v1/test_helpers/disputes` opens a case on it.

**Fraud reports** (TC40, SAFE) are kept apart from disputes: they move no money. Disputes
and fraud reports over the month's card payments make a ratio in the style of Visa's
VAMP, against a dated threshold (150 bps with 1,500 events). It is shown to the merchant,
and the worker reports merchants above it.

## Alternatives rejected

- **Separate models for chargebacks and MED.** The merchant answers both the same way
  (evidence or acceptance, by a date), and both move money out and maybe back. Two models
  would duplicate the API, the events and the deadline machinery for no difference a
  merchant sees.
- **Deadlines in code, per network.** They change by resolution and by rulebook. A dated
  table records each change and its source, as the authorization validity and settlement
  terms already do.
- **A workflow engine for deadlines** (the research suggested Temporal for one showcase
  flow). The deadlines are rows with a due date that a minute-by-minute pass compares to
  the clock. They need no durable timers beyond PostgreSQL, and tests move the clock.
- **Trusting signed events.** The signature proves the network sent something, not that
  it is still true; a leaked secret would otherwise move money. Reading the case back
  costs one request per event.

## Consequences

- Every deadline can be tested in accelerated time: the tests move the clock months
  ahead, and the disputes close on their own.
- A merchant who saves evidence and forgets to submit it is still represented.
- Visa's, Elo's, Amex's and Hipercard's figures are unverified; changing them is a new row.
- The ratio counts card payments by when they were authorized, since test mode never
  clears. It is close to VAMP's settled transactions, but not the same.
