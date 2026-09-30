# Card network and issuer simulator

`sim-card-network` stands for a card network and the issuers behind it. Jupiter's
acquirer connector reaches it the way it would reach a real one: ISO 8583 over TCP, as
[docs/cardnet/iso8583.md](../../../docs/cardnet/iso8583.md) specifies, and clearing files
over HTTP. Nothing in it is certified, and no scheme's proprietary behaviour is modelled.

```sh
go run ./cmd/sim-card-network   # ISO 8583 on 127.0.0.1:8583 (JUPITER_CARDNET_ADDR), files on 127.0.0.1:8584 (JUPITER_HTTP_ADDR)
curl -X POST 'http://127.0.0.1:8584/admin/close-day?date=2026-10-01'
curl http://127.0.0.1:8584/v1/acquirers/10000000001/clearing/2026-10-01
```

## What it does

| Behaviour | Source |
|---|---|
| Answers every request (0100, 0200, 0400) and advice (0220/0221, 0420/0421) with its response type | Sourced: the ISO 8583:1987 message classes |
| Holds an authorization's amount against the card's credit, releases it on reversal, and turns it into a completion for up to its amount, releasing the rest | Sourced in principle (dual-message authorization and capture); the amounts are the simulator's |
| Acknowledges a reversal of something it never saw, or already reversed | Sourced: an acquirer reverses what it cannot be sure did not happen, and the issuer must process it |
| Refuses a merchant-initiated authorization that does not quote an earlier network transaction id of the same card (57) | Unverified: the stored credential framework as the research described it |
| Stands in for an unavailable issuer, approving up to R$ 500.00 and flagging it (DE 48 SI) | Unverified: stand-in processing as commonly described; the limit is the simulator's |
| Approves half the amount (10) only when the request says it can take a partial approval | Unverified: the partial approval code and capability flag |
| Closes each business day at midnight, or when told, into one clearing file per acquirer | The file format is the simulator's; real clearing is proprietary |

Each card has a credit limit of R$ 100,000.00. Expired cards (DE 14 in the past) are
declined with 54, numbers failing the Luhn check with 14.

## Test cards

Any other valid number is approved while its limit lasts.

| Number | Behaviour |
|---|---|
| 4000000000000002 | Declined: do not honour (05) |
| 4000000000009995 | Declined: insufficient funds (51) |
| 4000000000000069 | Declined: expired card (54) |
| 4000000000000119 | Approved and held, but the answer is lost: the acquirer must reverse it |
| 4000000000000168 | The request is lost before the issuer: nothing is held |
| 4000000000000127 | Approved, answered after the acquirer's timeout |
| 4000000000000135 | Approved, answered twice |
| 4000000000000143 | Half approved (10) when partial approval is offered, fully approved otherwise |
| 4000000000000150 | The issuer is down: approved in stand-in up to R$ 500.00, 91 above |

## Faults

In-process, `Config.Faults` is asked about every request and advice and can lose the
request, lose the answer, answer late (after `Config.LateAfter`) or answer twice. The
deterministic simulation uses it to draw faults from its seed.
