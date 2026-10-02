# PCI DSS scope

This document says which parts of Jupiter handle card data, which do not, and what keeps
them that way. It describes a design and the tests that hold it, not a compliance
assessment: Jupiter connects to no real network, and no assessor has looked at it. PCI DSS
requirement numbers come from the research's background knowledge and are
[unverified](research/notes/04-card-rails.md).

## Card data, and where it is

| Data | Where it may be | How |
|---|---|---|
| Full card number (PAN) | The vault's database, encrypted | AES-256-GCM, one data key per card, wrapped by a KMS key ([ADR 0017](adr/0017-envelope-encryption-and-key-rotation.md)) |
| | The memory of the vault, the API and the worker | While a card is saved, and while the rail authorizes with it |
| Security code (CVC) | The vault's memory only, for at most 30 minutes | Given once, to the first authorization; never written anywhere ([ADR 0018](adr/0018-how-card-numbers-enter-and-leave.md)) |
| Expiry date | The vault and Jupiter's database | Not sensitive on its own; stored to show the customer's card |
| BIN (first 8 of 16+ digits, else 6) and last four | The vault and Jupiter's database | The display formats PCI DSS allows |
| Keyed fingerprint | The vault; its first 128 bits in Jupiter's database | HMAC-SHA256 under a key only the KMS holds, so it cannot be matched against the BIN and last four without that key |
| Token (`tok_…`) | Anywhere | Random; worthless without the vault and its owner. An unclaimed token from a web page can be claimed, within an hour, only with a secret key of the merchant whose publishable key made it |

## In scope

- **The vault** (`cmd/vault`), its database and its keys. It stores, encrypts and
  decrypts numbers.
- **The KMS**, which holds the key-encryption keys and the fingerprint key. In
  development it is the local implementation, and its keys sit in the vault's
  environment beside its database URL: whoever reads that environment and the database
  can decrypt every card, and match fingerprints by brute force over the few hundred
  thousand numbers a BIN and last four leave. The design's protections assume a real
  KMS or HSM behind the same port, where the keys never leave it.
- **The API and worker processes**, as systems that transmit card numbers: a merchant that
  saves a card by number sends it through the API on its way to the vault, and the rail
  detokenizes a saved card in the API or worker process to authorize with it. Neither is
  meant to write a number anywhere, and the canary test checks their database and logs;
  a card number held in memory can still reach a crash dump.
- **The network between them and the vault**, which carries numbers inside mTLS, and the
  links that carry them onwards: to the card network, in authorization requests (DE 2),
  in token provisioning requests, and to the 3-D Secure directory server, in
  authentication requests.
- **Network tokens.** The DPAN the network issues for a card is kept in the vault,
  encrypted under the card's data key; Jupiter's database holds only its reference.

## Out of scope

- **Jupiter's database**, its backups and replicas, and everything that reads them.
- **Logs and traces** of every process. The vault logs method, route and status, never a
  body; the card types print redacted (`card ****4242 12/2030`) however they are
  formatted, and refuse to marshal to JSON.
- **Events and webhooks**, which carry object ids, not card data.
- **The fingerprint merchants see**, derived from the vault's with the merchant's id, so
  two merchants cannot match their customers through the API. Someone who can read
  Jupiter's database, which holds the vault's fingerprint, could.
- **Merchants' servers that send tokens**, not numbers.
- **Pix and payouts** (the Pix connector, the payer's tax id on charges with a due date,
  Pix keys): no card data. The payer's CPF or CNPJ is personal data under the LGPD, kept
  in the payment intent's Pix options and sent only to Jupiter's bank.

## What holds the line

Each boundary is a test that runs in CI:

| Claim | Test |
|---|---|
| Jupiter's database never holds a card number | `test/pci`: a canary number, unique to the run, goes through every path a card takes, then every table of every schema is scanned for it as text and as hex; so are the logs of the API, the rail and the vault |
| The vault's database holds numbers only encrypted, and no security code | `TestTheVaultDatabaseHoldsNoCardInTheClear` scans it, and checks the schema has no column for a code |
| Only the API's and worker's certificates reach card data, and the worker cannot save cards | `TestTheVaultRefusesCallersWithoutTheAPICertificate`, `TestTheWorkerMayReadCardsButNotSaveThem` |
| The security code is given once and expires | `TestSecurityCodesLiveOnlyInMemoryAndExpire` |
| Keys rotate without stopping payments | `TestKeyRotationUnderLoad` |
| Nothing outside the vault imports its internals | `test/architecture` |

## Merchants' questionnaires

How a merchant takes a card decides which self-assessment questionnaire it may use:

| Integration | Questionnaire |
|---|---|
| The page is a Jupiter-hosted iframe (hosted fields) that posts the card to the vault | **SAQ A**, if the merchant's site also meets the January 2025 eligibility criterion that it is not susceptible to script attacks |
| The merchant's own page script posts the card to the vault's `POST /v1/tokens` with the publishable key, and its server sends Jupiter only the token | **SAQ A-EP**: the merchant's page is served by the merchant and could be altered to capture the card |
| The merchant's server sends the number to `POST /v1/payment_methods` | **SAQ D**: the number passes through the merchant's systems |

Jupiter builds the vault's route a hosted-fields iframe would call; the iframe itself is
a frontend and outside this repository. Jupiter, as a service provider that stores card
numbers, would assess against SAQ D for service providers or with a Report on Compliance.

## Known gaps

- **The public route checks a publishable key's shape only.** Anyone can create unclaimed
  tokens, which are deleted after an hour. Each address (an IPv6 one by its /64, behind
  trusted proxies the browser's), each publishable key and the route as a whole are
  rate-limited in process, and a real deployment adds an edge with limits of its own. The
  route serves plain HTTP only on loopback or behind an edge that ends TLS.
- **Security codes live in one vault instance's memory, best effort.** With several
  instances, an authorization reaching another instance goes without the code; a
  restart loses them; at most 100,000 are held, of which codes from web pages fill at
  most half, and a code not kept is logged. All of these fail safe.
- **Memory is not scrubbed beyond data keys.** Go strings cannot be cleared, so a number
  or code decrypted into one stays in memory until it is collected.
- **Only the token binds a ciphertext to its row.** Someone who can write the vault's
  database could change a card's owner or expiry without breaking decryption.
- **The API and worker are in scope as transmitters.** Taking them out would need the
  vault to forward authorizations to the card network itself, holding the connector phase
  5 builds inside Jupiter ([ADR 0018](adr/0018-how-card-numbers-enter-and-leave.md)).
- **No deletion.** A saved card cannot yet be removed from the vault.
- **The card network link is plain TCP.** The connector refuses an address off this
  machine unless `JUPITER_CARDNET_PRIVATE_LINK=true` says it is a private circuit; a real
  link would also use TLS, which the connection library supports. Only authorizations
  carry the number: reversals, completions and refunds name the transaction instead, so
  the store-and-forward queue holds none. Messages print with the card masked to its
  last four digits.
- **Development keys and certificates** come from environment variables and
  `jupiterctl dev-certs`. Split knowledge, dual control and certificate rotation belong to
  the real KMS and CA.
