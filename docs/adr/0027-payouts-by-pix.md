# 0027. Payouts: held on the balance, sent by Pix, never abandoned

- Status: Accepted
- Date: 2026-09-30

## Context

The golden path ends with the seller paid out by Pix. Phase 11 brings settlement, and
scheduled payouts, bank transfers, returned payouts, minimums and holds. Phase 7 needs
the part that moves money out, on demand, by Pix. It must not pay out the same balance
twice, and it must not lose track of a transfer whose answer was lost: the money may have
left.

## Decision

**A payout** (`POST /v1/payouts`) names an amount in BRL and a Pix key: a CPF or CNPJ with
valid check digits, an e-mail, a phone number in E.164, or a random key. It lives in the
payments module, beside the balance it spends, so the invariant checker sees payments,
refunds and payouts together.

**Held first.** Creating a payout takes a lock on the merchant's balance and checks what is
available: posted, less what is held against it, less refunds not yet confirmed. It then
places a ledger hold from the merchant's balance to Jupiter's Pix account, in the
transaction that records the payout. Two payouts can never spend the same money.

**Then sent.** The transfer is asked of the bank outside any transaction, named by the
payout, so asking again answers with the transfer already made. The answer is applied
atomically: paid posts the hold and records the endToEndId and who DICT says the key
belongs to; failed voids it, and the amount is available again.

**Unknown is resolved, never abandoned.** A transfer without an answer leaves the payout
`pending`. The resolver asks the bank about it, and sends it again only if the bank never
received it. The resolver never gives up, because the money may have left.

**Events** `payout.created`, `payout.paid` and `payout.failed`; scopes `payouts:read` and
`payouts:write`.

## Alternatives rejected

- **A separate payouts module now.** The checker would have to reach into another
  module's tables to know the merchant's balance is right; phase 11 can split it once
  settlement gives payouts their own life.
- **Checking the balance without a lock.** Two concurrent payouts could both see the same
  available amount.
- **Failing a payout whose answer was lost after a while.** Unlike an authorization, a
  sent Pix cannot be reversed by Jupiter; the bank's answer is the only truth.

## Consequences

- Payouts spend the ledger balance, not money settled: until settlement arrives (phase
  11), Jupiter pays out from its own Pix account balance. The ledger shows the Pix
  account going down, and reconciliation (phase 13) will hold it to the bank's statement.
- A merchant can pay out only in BRL, only by Pix, only on demand.
- **Known gap:** every secret key can pay out, to any key, with no limit. A leaked secret
  key could empty the balance. Phase 10 gives payouts their destinations: a recipient's
  registered Pix key or account, verified before money goes to it. Until then, restricted
  keys without `payouts:write` are the only protection, and integrations should use them.
