# 0024. Network tokens are provisioned in the background and kept in the vault

- Status: Accepted
- Date: 2026-09-30

## Context

A network token replaces a card number with one the scheme issues for one token requestor
(Visa Token Service, Mastercard MDES). Each payment carries a one-time cryptogram. When
the bank replaces the card, the network moves the token to the new card and tells the
requestor. Schemes push for it, and it raises approval rates. Vault tokens and network
tokens are two layers, not alternatives ([research: card rails, tokenization](../research/notes/04-card-rails.md)).
The provisioning APIs are behind scheme onboarding; none of their details were sourced.

## Decision

The card network simulator runs a token service, and the acquirer connector is its token
requestor.

**Provisioning.** The worker claims live payment methods without a token and provisions
one for each. It reads the card from the vault without taking its security code, which
stays for the payment it is meant for. It asks the network for a token and keeps the token
number (a DPAN) in the vault, encrypted under the card's own data key. Payments stores
only the reference and a status. A card the network will not tokenize is marked failed and
keeps being paid with its number; one the network could not be reached for stays claimed,
and is taken up again after five minutes. Only the worker's identity may store a token in
the vault, and a provisioning run whose claim was taken over by a later one records
nothing.

**Payments.** A card with an active token is authorized with the DPAN in DE 2 and a
cryptogram fetched for that amount in DE 48 TC. The network checks the cryptogram, once,
for that amount, and swaps in the real card before the issuer decides. Without a
cryptogram the payment falls back to the card number.

**Lifecycle.** The network signs its events (`Cardnet-Signature`) and dates them; they may
arrive out of order, and one older than the last applied to a token is ignored. When a card behind a
token is replaced, the merchant's payment method shows the new last four and expiry, and
later payments reach the new card through the same token. A suspended token sends payments
back to the card number.

A test runs the whole cycle: provisioning, a tokenized payment the network sees as such,
a replacement and a suspension (`test/cardrail`).

## Alternatives rejected

- **Provisioning while the merchant saves the card.** It would make saving a card depend
  on the network's token service being up.
- **Keeping the DPAN in Jupiter's database.** A DPAN is less sensitive than a card number,
  but it is payment data; the vault already protects it, at no extra cost.
- **Failing the payment when no cryptogram comes.** The card number still works; a token
  is an improvement, not a requirement.

## Consequences

- Test mode has no network, and no tokens.
- The DPAN is decrypted in the worker's and the rail's memory, as the card number is; it
  is less sensitive, being useless without a cryptogram, but is kept as carefully.
- Network tokens here are the acquirer's, not portable to another PSP; a token requestor id
  owned by the merchant is outside this plan.
- The token events do not yet reach merchants as webhooks; the payment method changes, and
  a `payment_method.updated` event is for a later phase.
