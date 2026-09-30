# 0017. Envelope encryption with a data key per card, rotated by rewrapping

- Status: Accepted
- Date: 2026-09-30

## Context

PCI DSS requires stored card numbers to be unreadable, hashes of them to be keyed, and
keys to be rotated at the end of their cryptoperiod. It also warns that a truncated form
and a hash of the same number, stored together, can be correlated to rebuild it
([research: card rails](../research/notes/04-card-rails.md), requirement numbers
unverified). Key rotation must not stop payments.

## Decision

**A data key per card.** Each number is sealed with AES-256-GCM under a fresh 256-bit data
key. The data key is stored only wrapped by a key-encryption key (KEK) that the KMS holds,
and the row records the KEK's id. Both the ciphertext and the wrapped key are bound to the
token as associated data, so a ciphertext copied to another row does not open.

**A KMS port.** `internal/vault/kms.KMS` wraps and unwraps data keys and computes MACs; the
keys never leave it. The local implementation keeps KEKs from configuration
(`JUPITER_VAULT_KEKS=id:base64,…`, `JUPITER_VAULT_ACTIVE_KEK`). A cloud KMS or an HSM
implements the same four methods.

**Rotation by rewrapping.** A new KEK is added and made active; new cards are wrapped under
it at once. The vault then moves every data key from older KEKs to it, in batches, taking
rows with `FOR UPDATE SKIP LOCKED`, so several instances can share the work and reads
never wait. The numbers are not re-encrypted, only their data keys. A background loop
does this every thirty seconds, and `vault rewrap` runs it to completion. `vault keys`
counts the cards under each KEK, and a KEK is retired only when none remain. The exit
criterion is a test that rotates while eight clients tokenize and detokenize, with no
failed operation, and then reads every card after the old KEK is removed
(`TestKeyRotationUnderLoad`).

**A keyed fingerprint.** HMAC-SHA256 of the number under a key the KMS holds, which is
never rotated: rotating it would stop the same card matching its earlier tokens. Without
the key, the BIN and last four beside it narrow the number to a few million candidates,
but none can be checked. The vault gives Jupiter the first 128 bits. Merchants see
`HMAC(vault fingerprint, merchant id)`: the same card has the same fingerprint at one
merchant and unrelated ones at two.

## Alternatives rejected

- **One key for every card.** Rotation would re-encrypt the whole table, and one leaked
  key would open every card.
- **Re-encrypting numbers on rotation.** Needs the numbers in the clear again, card by
  card, for no benefit over rewrapping: a data key is as strong as the KEK that wraps it.
- **An unkeyed hash for matching cards.** Brute-forceable in seconds from the BIN and the
  last four, which PCI DSS requires to be prevented.
- **Showing merchants the vault's fingerprint.** Two merchants could match their
  customers by it.

## Consequences

- A cryptoperiod is a configuration change and a background job, not a migration.
- The fingerprint key cannot be rotated without rebuilding every fingerprint, which the
  vault could do only by decrypting every number. That is accepted: it guards against
  correlation, not against disclosure of the numbers themselves.
- A card tokenized by an instance that read the active key just before the switch may be
  stored under the old key after a rewrap pass. Retirement therefore goes in this order:
  activate the new key on every instance; wait longer than a request can take (a
  minute); run `vault rewrap`; check that `vault keys` shows nothing under the old key;
  only then remove it. The vault refuses to call the active key, or one with cards under
  it, retirable.
- Keys come from environment variables in development. That is the local KMS's
  limitation, not the design's: split knowledge and dual control belong to the real KMS.
