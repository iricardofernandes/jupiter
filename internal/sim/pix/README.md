# Pix simulator

`sim-pix` stands for the Pix side of the world: the partner bank that holds Jupiter's
account and offers it the Banco Central's **API Pix**, the SPI that settles between
participants, DICT, which resolves keys, and every payer and their bank. Jupiter reaches it
the way it would reach a real bank: HTTPS with mutual TLS and certificate-bound OAuth
tokens. It notifies Jupiter the same way. Nothing in it is certified. It is not a Pix
participant, and it connects to no real SPI or DICT.

```sh
make certs
JUPITER_SIM_TLS_CERT=.certs/sim-pix.pem JUPITER_SIM_TLS_KEY=.certs/sim-pix-key.pem JUPITER_SIM_TLS_CA=.certs/ca.pem \
JUPITER_SIM_WEBHOOK_CERT=.certs/sim-pix-hook.pem JUPITER_SIM_WEBHOOK_KEY=.certs/sim-pix-hook-key.pem \
JUPITER_SIM_PIX_CLIENTS=jupiter-live:secret:7f6e5d4c-3b2a-4190-8f7e-6d5c4b3a2918:1000000 \
  go run ./cmd/sim-pix    # API Pix on https://127.0.0.1:8586, operator controls on http://127.0.0.1:8587

# a customer pays the BR Code Jupiter showed them
curl -X POST http://127.0.0.1:8587/admin/pay -d '{"brcode": "00020101021226...", "payer_tax_id": "12345678909"}'
```

The request and response types are generated from the official specification
(`pkg/pixapi`, from `api/bacen-pix`, version 2.10.0). The BR Codes are
[`pkg/brcode`](../../../pkg/brcode)'s.

## What it does

| Behaviour | Source |
|---|---|
| OAuth 2.0 client credentials at `POST /oauth/token`, over mutual TLS. The token is bound to the client certificate's SHA-256 thumbprint (`x5t#S256`); a token presented on a connection made with any other certificate is refused (401) | Sourced: Manual de Padrões, Anexo II §3.1 (RFC 8705 certificate-bound tokens) |
| Scopes `cob.*`, `cobv.*`, `pix.*`, `webhook.*`, checked per route | Sourced: the specification's security schemes |
| Immediate charges (`PUT/POST/GET/PATCH /cob`) and charges with a due date (`PUT/GET/PATCH /cobv`). A txid is 26 to 35 letters and digits and is never reused, even after its charge is removed | Sourced: specification and manual Anexo I §5.3.1 |
| Charge statuses `ATIVA`, `CONCLUIDA` and `REMOVIDA_PELO_USUARIO_RECEBEDOR`. Expiry is never a status: a cob expires `expiracao` seconds after creation, and a cobv at the end of `validadeAposVencimento` days after its due date | Sourced: the status enum and calendar fields |
| Each charge gets a payload location, and `pixCopiaECola` carries a dynamic BR Code pointing to it | Sourced: manual §2.5.2, §2.7 |
| A payload location answers with the charge's payload as a compact JWS, content type `application/jose`, and 410 for a charge that is paid, removed or expired | Sourced: manual §2.7.2 (RFC 7515) and §2.7.3 |
| The JWS is signed PS256, with a `jku` header naming the key set at `/jwks` on the same host, which payers check before paying | **Unverified**: the algorithm and headers are set by the Manual de Segurança do SFN, which the research could not obtain |
| cobv amounts on a date: the original, less a fixed or percentage rebate (abatimento), less a discount (modalities 1 and 2 until fixed dates, 3 and 5 per day early), plus a fine (fixed or percentage) and interest (modalities 1 to 4: an amount per day, or a percentage per day, month or year of calendar days) when paid late. A payer asks with `DPP` | Modalities sourced from the specification's domain tables. The arithmetic is **unverified**: percentages are of the original amount, rounded half up to the centavo, a month counts 30 days and a year 365. The business-day modalities (juros 5-8, desconto 4 and 6) are refused: the simulator has no calendar |
| A Pix received: an endToEndId `E` + ISPB + minute in UTC + 11 characters | Sourced: the format as the research describes it |
| `GET /pix` (by period, paged) and `GET /pix/{e2eid}`, with the Pix's returns and, for a cobv, `componentesValor` | Sourced: specification |
| Returns (`PUT/GET /pix/{e2eid}/devolucao/{id}`), partial or whole, never more than the Pix. They start `EM_PROCESSAMENTO` and end `DEVOLVIDO`, or `NAO_REALIZADO` when the payer's account is closed or the client's balance is short | Statuses sourced. The reasons for failure are the simulator's |
| Notifications: `PUT /webhook/{chave}` registers an https URL. Each Pix with a txid received on that key, and each return that reaches a final status, is posted to `{url}/pix` over mutual TLS with the bank's certificate. A Pix without a txid is not notified | Sourced: specification. The body is `{"pix": [...]}`, with the fields of `GET /pix/{e2eid}` |
| Transfers out: `PUT /transferencias/{idEnvio}` with `valor` and `chave`, and `GET /transferencias/{idEnvio}`. The key is resolved in DICT and the transfer settled at once, `REALIZADO`, or refused, `NAO_REALIZADO`: unknown key, closed account, short balance | **The simulator's own**: the API Pix does not cover sending, and each bank has its own interface. This one follows the API's conventions: client-chosen ids, an idempotent PUT, and the same error format |
| Errors as RFC 7807 problems typed `https://pix.bcb.gov.br/api/v2/error/<Type>` (`CobOperacaoInvalida`, `PixDevolucaoInvalida`, `AcessoNegado`, …) | Sourced: specification |

Not simulated: lotecobv, Pix Saque and Pix Troco (refused), Pix Automático (rec, solicrec,
cobr), MED returns, key portability, participants other than the bank itself as receivers,
and time spent in the SPI.

## Payers and operators

The admin handler takes no credentials and must listen on loopback only.

| Route | What |
|---|---|
| `POST /admin/pay` | A payer pays: `{"brcode": "..."}` for a code (a dynamic one is fetched over HTTPS, its signature checked, and paid today), or `{"key": "...", "amount": "10.00"}` for a transfer to a key. Answers the endToEndId, or 422 with why it was refused |
| `POST /admin/dict/keys` | Registers a key: `{"key", "ispb", "name", "tax_id", "closed"}` |
| `POST /admin/payers/{tax_id}/close` | Closes a payer's account: returns to it fail |
| `GET /admin/transfers` | What clients sent |
| `GET /admin/deliveries` | Notifications sent, dropped or refused |
| `GET /admin/balances/{client}` | A client's balance |

## Faults

`Config.Faults` is asked before each notification, transfer, return and charge answer. It
can drop a notification, hold a transfer or return in processing for a while, or carry out
a transfer or create a charge and drop the connection before answering. The tests use these to exercise Jupiter's
unknown-outcome paths.
