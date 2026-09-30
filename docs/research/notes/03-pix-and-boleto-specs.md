# Pix and Boleto/CNAB: Technical Specifications (as of Sept 2026), for Jupiter adapters and a counterparty simulator

Research date: 2026-09-29. Primary sources I read directly: the BCB **Manual de Padrões para Iniciação do Pix v2.10.0 (19/08/2026)** (full PDF text), the **bacen/pix-api OpenAPI v2.10.0** (full YAML, 9,124 lines), the **FEBRABAN CNAB 240 layout v10.11 (31/07/2023)** (full PDF text) and the **FEBRABAN "Layout Padrão de Arrecadação com Código de Barras" v06**. I also verified the BR Code CRC16 and the boleto DV/linha digitável algorithms by running code against official examples (see Inferences).

---

## 1. Pix architecture: SPI, DICT, PSP direto/indireto, chaves, endToEndId, txid, liquidação, conta PI

### Takeaway
The pieces Jupiter must model are these. SPI is BCB's real-time gross settlement engine, which uses ISO 20022 pacs.008 for payments, pacs.004 for returns and pacs.002 for status. DICT is the key directory. Each payment carries an `endToEndId` of 32 characters, formatted `E` + 8-char ISPB (or CNPJ root) + `yyyyMMddHHmm` in UTC + 11 alphanumeric characters. A charge (cobrança) carries a `txid` of 26 to 35 alphanumeric characters, unique per receiver CPF/CNPJ. Recurrences carry a `idRec` of 29 characters starting with `RR` or `RN`.

### Cited Findings
- **Message flow.** The payer PSP app reads the txid. After the payment is confirmed, the txid "é enviado para o SPI via pacs.008". The receiver PSP gets a pacs.008 containing the txid and can then notify the receiving user — [pix-api openapi.yaml v2.10.0](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **EndToEndId in the spec.** The OpenAPI schema is `pattern: "[a-zA-Z0-9]{32}"`, min and max 32, described as "EndToEndIdentification que transita na PACS002, PACS004 e PACS008" — [pix-api openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **EndToEndId format.** It is `ExxxxxxxxyyyyMMddHHmmkkkkkkkkkkk` (32 chars, case sensitive):
  - `E` is fixed.
  - `xxxxxxxx` is the ISPB of the direct participant, the ISPB of the indirect participant, or the first 8 digits of the payment initiator's CNPJ.
  - `yyyyMMddHHmm` is in UTC.
  - `k…` is an 11-character alphanumeric sequence, unique within each minute.
  — [Cielo API Pix manual](https://developercielo.github.io/en/manual/apipix); see also [Go generator gist](https://gist.github.com/arantesxyz/b78f00ac3589f93deeefa331e1f1fdb3) and [bacen/pix-api issue #592](https://github.com/bacen/pix-api/issues/592)
- **Who generates the EndToEndId.** Since 25/09/2022 the initiator must generate it for Open Finance-initiated Pix. Before that, the direct participant, the indirect participant or the initiator could generate it — [Manual de Padrões v2.10.0 §3.2](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaodoPix.pdf)
- **The payer's backend.** "O backend do PSP do pagador é o servidor do PSP do pagador que está conectado ao SPI via interface ICOM na Rede do Sistema Financeiro Nacional". The app reaches SPI only indirectly, through that server — [Manual de Padrões §2.3, fn 4](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Key formats in BR Code and DICT:**
  - e-mail as-is
  - CPF as 11 digits
  - CNPJ as 14 characters, now possibly **alphanumeric** (e.g. `12ABC34501DE35`)
  - phone as `+5561912345678`
  - EVP (random key) as a hyphenated UUID

  — [Manual de Padrões §2.5.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Alphanumeric identifiers.** pix-api 2.9.0 "allow[s] alphanumeric values in CNPJ, ISPB, and recurrence ID fields". The ISPB pattern in the spec is now `^[0-9A-Z]{8}$` and the CNPJ pattern is `^[0-9A-Z]{14}$` — [pix-api changelog](https://raw.githubusercontent.com/bacen/pix-api/master/changelog.md); [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **txid for cobranças:**
  - It is 26 to 35 characters, `A-Z a-z 0-9`.
  - The pair CPF/CNPJ + txid must be unique per PSP, and "não podendo ser repetido, ainda que a cobrança original tenha sido cancelada ou baixada".
  - For `cob`, the receiver may delegate txid generation to the PSP (`POST /cob`).

  — [Manual de Padrões Anexo I §5.3.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf). The OpenAPI `TxId` schema is `[a-zA-Z0-9]{26,35}`, while the query and static contexts use `[a-zA-Z0-9]{1,35}` — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **txid in static QR:** EMV 62-05 holds up to **25 chars** of `[a-zA-Z0-9]`. `***` means "no txid". The PSP cannot enforce uniqueness, so reconciliation is up to the receiver — [Manual de Padrões §2.6.2, Anexo I §5.3.2](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **idRec (Pix Automático recurrence id).** It is 29 characters: `RR` or `RN` + `xxxxxxxx` (ISPB) + `yyyyMMdd` + 11-char alphanumeric sequence.
  - `RR` means post-due retries are allowed.
  - `RN` means no retries.
  - Example: `RR1234567820240115abcdefghijk`.

  — [openapi.yaml RecId](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Devolução natureza codes.** A return of a normal Pix uses pacs.004 codes `MD06`, `BE08`, `FR01` or pacs.008 `REFU`. A return of a Saque/Troco uses `MD06` or `SL02`. The API nature enum is `ORIGINAL`, `RETIRADA`, `MED_OPERACIONAL`, `MED_FRAUDE`, `MED_PIX_AUTOMATICO` — [openapi.yaml DevolucaoNatureza](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Fixed-point amounts.** Valor fields use the pattern `\d{1,10}\.\d{2}`, i.e. strings with 2 decimals — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)

### Inferences
- The simulator should mint E2E IDs as `E{ISPB}{UTC yyyyMMddHHmm}{11 random alnum}`. It should also validate the ISPB portion as `[0-9A-Z]{8}`, because the alphanumeric ISPB and CNPJ changes landed in 2.9.0.
- Jupiter's cob adapter should generate its own 26–35-char txids (for example a ULID/UUID without hyphens, padded) and never reuse them. That also makes PUT `/cob/{txid}` idempotent.
- The model should treat pacs.008, pacs.004 and pacs.002 as the three SPI events: payment credited, return, and status/rejection.

### Gaps
- I could not fetch these primary sources in this session: the SPI Manual / Catálogo de Mensagens, the DICT Manual Operacional (key limits per account, portability/claim flows, anti-scraping rate limits), the Regulamento Pix text on PSP direto/indireto, and conta PI (Conta Pagamentos Instantâneos at BCB) rules. Background knowledge that the report writer should verify before stating:
  - Direct participants hold a conta PI at BCB and connect to SPI via RSFN/ICOM.
  - Indirect participants settle through a direct participant.
  - Individuals (PF) may register up to 5 keys per account and legal entities (PJ) up to 20.
  - SPI settles 24x7 with target end-to-end times of seconds.
  - Transfers between the conta PI and the conta Reservas Bancárias happen via STR windows.
- The DICT API spec (github.com/bacen/pix-dict-api) was not reviewed.

---

## 2. API Pix (bacen/pix-api): resources, auth, lifecycle, errors, versions

### Takeaway
The current spec is **v2.10.0** (OpenAPI 3.0, matched to Manual de Padrões v2.10.0 of 19/08/2026). It covers cob, cobv, lotecobv, loc/locrec, pix + devolução, webhooks (pix/rec/cobr), and the Pix Automático resources (rec, solicrec, cobr) that were added in 2.7.0. Auth is OAuth2 client_credentials over mTLS with certificate-bound tokens (RFC 8705). Errors follow RFC 7807 with typed URIs under `https://pix.bcb.gov.br/api/v2/error/`.

### Cited Findings
- **Version:** `info.version: "2.10.0"` — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml); [release 2.10.0](https://github.com/bacen/pix-api/releases/tag/2.10.0). The manual says the API doc version with the same identifier is synchronized with the manual version — [Manual de Padrões Anexo I §2](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Full path list (v2.10.0)** — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml):

  | Path | Methods |
  |---|---|
  | `/cob/{txid}` | PUT, PATCH, GET |
  | `/cob` | POST, GET |
  | `/cobv/{txid}` | PUT, PATCH, GET |
  | `/cobv` | GET |
  | `/lotecobv/{id}` | PUT, PATCH, GET |
  | `/lotecobv` | GET |
  | `/loc` | POST, GET |
  | `/loc/{id}` | GET |
  | `/loc/{id}/txid` | DELETE |
  | `/locrec` | POST, GET |
  | `/locrec/{id}` | GET |
  | `/locrec/{id}/idRec` | DELETE |
  | `/pix` | GET |
  | `/pix/{e2eid}` | GET |
  | `/pix/{e2eid}/devolucao/{id}` | PUT, GET |
  | `/{pixUrlAccessToken}` | GET (the payload location for cob, returns a JWS) |
  | `/cobv/{pixUrlAccessToken}` | GET |
  | `/rec/{recUrlAccessToken}` | GET |
  | `/webhook/{chave}` | PUT, GET, DELETE |
  | `/webhook` | GET |
  | `/webhookrec` | PUT, GET, DELETE |
  | `/webhookcobr` | PUT, GET, DELETE |
  | `/rec/{idRec}` | (see spec) |
  | `/rec` | (see spec) |
  | `/solicrec` | (see spec) |
  | `/solicrec/{idSolicRec}` | (see spec) |
  | `/cobr/{txid}` | (see spec) |
  | `/cobr` | (see spec) |
  | `/cobr/{txid}/retentativa/{data}` | (see spec) |

- **OAuth scopes (from `flows.clientCredentials`):** `cob.write/read`, `cobv.write/read`, `lotecobv.write/read`, `payloadlocation.write/read`, `payloadlocationrec.write/read`, `pix.write/read`, `webhook.write/read`, `webhookrec.write/read`, `webhookcobr.write/read`, `cobr.write/read`, `rec.write/read`. `solicrec.*` follows the same pattern, but I did not confirm the literal line — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Mandatory security for PSPs** — [Manual de Padrões Anexo II §3.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf):
  - TLS ≥1.2 with forward-secrecy cipher suites only.
  - OAuth 2.0 (RFC 6749) with mTLS (RFC 8705). Client certificates are issued by the PSP or an external CA, never self-signed.
  - Each PSP runs its own Authorization Server and Resource Server, both with mTLS.
  - The AS issues **Client Certificate-Bound Access Tokens** (RFC 8705 §3).
  - The RS checks that the certificate thumbprint bound to the token equals the TLS client certificate.
- **Cob / CobV status enum:** `ATIVA`, `CONCLUIDA`, `REMOVIDA_PELO_USUARIO_RECEBEDOR`, `REMOVIDA_PELO_PSP`. The status describes the *registro*, not whether the charge is overdue or expired. Note the inconsistency: the prose says `REMOVIDO_…` but the enum says `REMOVIDA_…` — [openapi.yaml CobStatus](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Cob calendário:** `criacao` and `apresentacao` are RFC 3339 UTC timestamps. `expiracao` is in seconds and defaults to **86400**. `revisao` starts at 0 and increments on each PUT/PATCH change. The payload also carries `status`, `chave`, `txid`, `valor.original`, `valor.modalidadeAlteracao`, `devedor`, `solicitacaoPagador` and `infoAdicionais` — [Manual de Padrões §2.7.1.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **CobV (with due date):**
  - `calendario.dataDeVencimento` must be ≥ the creation date, and `validadeAposVencimento` is in days and ≥ 0.
  - Discount modalities 1–2 are fixed-date and 3–6 are percentage/daily.
  - Juros, multa and abatimento objects are validated against the schema.
  - The payer PSP must send **`codMun`** (IBGE municipality of the payer) and **`DPP`** (Data de Pagamento Pretendida) when fetching the cobv payload, so the receiver PSP can compute the amount, including local holidays.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml); [Manual de Padrões §2.7.1.2](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **LoteCobV:**
  - `PUT /lotecobv/{id}` returns **202** (asynchronous).
  - Each item has status `EM_PROCESSAMENTO` or `NEGADA` until it is created.
  - Items that are processing or denied do not exist in `GET /cobv`.
  - A cobv created via `PUT /cobv` can never be attached to a batch later.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Devolução:**
  - `PUT /pix/{e2eid}/devolucao/{id}`, where `id` is client-generated and matches `[a-zA-Z0-9]{1,35}`.
  - Final statuses are `DEVOLVIDO` and `NAO_REALIZADO`. The example responses show `EM_PROCESSAMENTO` first.
  - The webhook fires when a devolução reaches a final status.
  - 2.6.0 added `descricao`, and 2.6.1 adjusted the natures `BE08`/`FR01`.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml); [changelog](https://raw.githubusercontent.com/bacen/pix-api/master/changelog.md)
- **Webhooks:**
  - Registration is per chave: `PUT /webhook/{chave}`.
  - The callback is `POST {webhookUrl}/pix`, mTLS-protected, and fires when Pix with a txid is received or a devolução reaches a final status.
  - Separate callbacks exist at `{webhookUrl}/rec` and for cobr.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Error model:** RFC 7807 `Problema` with required fields `type`, `title`, `status` (plus `detail` and `violacoes`). The `type` is `https://pix.bcb.gov.br/api/v2/error/<Tipo>`. Types include:
  - generic: `RequisicaoInvalida`, `AcessoNegado`, `NaoEncontrado`, `PermanentementeRemovido` (410), `ErroInternoDoServidor`, `ServicoIndisponivel`, `IndisponibilidadePorTempoEsgotado`
  - cob/cobv/lotecobv: `CobOperacaoInvalida`, `CobConsultaInvalida`, `CobNaoEncontrado`, `CobVOperacaoInvalida`, `LoteCobVOperacaoInvalida`
  - payload: `CobPayloadNaoEncontrado`, `CobPayloadOperacaoInvalida`, `PayloadLocation*`
  - pix/devolução: `PixDevolucaoInvalida`, `PixNaoEncontrado`
  - webhooks: `Webhook*`
  - Pix Automático: `Rec*`, `SolicRec*`, `CobR*`, `WebhookRec*`, `WebhookCobR*`

  The endpoint descriptions list the specific "violações" per endpoint. — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml)
- **Recent changelog** — [changelog.md](https://raw.githubusercontent.com/bacen/pix-api/master/changelog.md):
  - 2.6.0: `pixCopiaECola` in charge responses, and `componentesValor` (juros, multa, desconto) in Pix queries.
  - 2.7.0-RC1: introduced the Pix Automático tags `Rec`, `SolicRec`, `CobR` and their webhooks.
  - 2.7.0: added `rec.dadosQR`, and `cobr.ajusteDiaUtil` defaults to true.
  - 2.8.x: `destinatario` is mandatory in solicrec, a `convenio` field was added, and descriptions are capped at 400 chars.
  - 2.9.0: alphanumeric CNPJ, ISPB and idRec.
  - 2.10.0: optional `encerramento.rejeicao.motivo`, new `finalidadeMED` query parameter on `GET /pix`, cancellation codes `PCFD`/`ERSL` deprecated, and `AGPSS` deprecated in favor of `AGFSS`.
- **Saque/Troco:**
  - The agent modality values are `AGTEC`, `AGTOT`, `AGPSS` (deprecated) and `AGFSS`.
  - Troco only allows `AGTEC`/`AGTOT`.
  - Static QR indicates Saque via the `fss` field, which holds an 8-char ISPB.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml); [Manual §2.6.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)

### Inferences
- The cob lifecycle for Jupiter's state machine is `ATIVA → CONCLUIDA` (paid), `ATIVA → REMOVIDA_PELO_USUARIO_RECEBEDOR` (PATCH status), or `ATIVA → REMOVIDA_PELO_PSP`. Expiration is *not* a status: it is derived from `criacao + expiracao`. A payment against a cob moves it to CONCLUIDA, and the Pix object (e2eid) holds a list of devoluções, each `EM_PROCESSAMENTO → DEVOLVIDO | NAO_REALIZADO`.
- The simulator should expose the same OpenAPI (it can be generated from the yaml with oapi-codegen). It should enforce mTLS plus a cert-bound token (compare `cnf.x5t#S256` to the client cert thumbprint), sign payload-location responses as JWS, and push webhooks over mTLS to Jupiter.

### Gaps
- I did not extract the exact multa and juros modality code tables for cobv (Anexo III of the manual has them). The discount modalities are confirmed as 1–6.
- I did not extract the exact webhook JSON body (`{"pix":[{endToEndId, txid, valor, horario, infoPagador, devolucoes[]}]}` is the known shape but was not re-verified in this pass).
- The JWS requirements for payload locations (alg PS256/RS256, `x5t`/`jku` header, TLS cert of the domain) live in the **Manual de Segurança do SFN**, which I did not fetch. A WebFetch summary claimed specific header requirements, but it was unreliable, so I discarded it.

---

## 3. BR Code / EMV QR: static, dynamic, composto; TLV; CRC; Copia e Cola

### Takeaway
A BR Code is EMV MPM TLV (2-digit ID + 2-digit length + value). Pix uses GUI `br.gov.bcb.pix` inside template 26–51:
- A **static** code carries a chave (sub 01), plus optional infoAdicional (02) and fss (03).
- A **dynamic** code carries a URL (sub 25, max 77 chars, no `https://`) that resolves to a signed JSON payload.
- A **composto** code (Pix Automático) adds a template 80–99 with a recurrence URL at sub 25.

It ends with ID 63 CRC16 (polynomial 0x1021, init 0xFFFF), computed over the whole string including `6304`.

### Cited Findings
- **Merchant Account Information:** template 26..51 is 23–99 chars, with GUI `br.gov.bcb.pix` (14 chars, case-insensitive). Unreserved template 80..99 is optional, except for QR composto where it is **mandatory** and carries the recurrence URL — [Manual de Padrões §2.5](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Static QR subfields** — [Manual §2.6, §2.6.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf):
  - `00` GUI (M)
  - `01` chave, 1–77 chars (M)
  - `02` infoAdicional, 1–72 chars (O)
  - `03` fss, 8 chars (O; an ISPB, which makes it a Pix Saque QR)

  These three subfields compete for the 99-char template budget. Static QR can also carry `54` amount and `62-05` txid.
- **Dynamic QR:** template 26 sub `00` GUI and sub `25` URL (1–77, M).
  - Fields 54 (amount) and 62-05 (txid) "não devem ser preenchidos" in dynamic QR. If present, they are ignored and the JSON payload prevails, because one dynamic BR Code can be reused and amount/txid can change per fetch.
  - Also: "O tamanho máximo da URL completa (sem o prefixo de protocolo) é de 77 caracteres". The URL layout is `{fqdnPspRecebedor}/{endpointOpcional}/{cob|cobv|rec}/{urlAccessToken}`; with no fragment it means cob.
  - Access must be HTTPS only, after validation of the URL and the authorized domain.

  — [Manual §2.5.2, §2.7](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Official static example**, with a line-broken form given in the manual:
  - `00020126580014br.gov.bcb.pix0136123e4567-e12b-12d1-a456-4266554400005204000053039865802BR5913Fulano de Tal6008BRASILIA62070503***63041D3D`
  - Line-broken: `00 02 01 / 26 58 [00 14 br.gov.bcb.pix][01 36 <uuid>] / 52 04 0000 / 53 03 986 / 58 02 BR / 59 13 Fulano de Tal / 60 08 BRASILIA / 62 07 [05 03 ***] / 63 04 1D3D`
  - The CRC footnote says "Polinômio 0x1021, C.I. 0xFFFF. A ordem dos objetos modifica o CRC".
  - The merchant name shown to the payer is the one returned by DICT, not ID 59.

  — [Manual §2.6.3](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Field sizes** (the manual summary also lists 59 max 25, 60 max 15, 61 postal code max 8, 62 max 99): the payload is TLV with 11 mandatory fields and 1 optional field, ending in CRC-16/CCITT-FALSE — [CriarQR explainer](https://criarqr.com/como-funciona-pix/); [BR Code Manual (BCB)](https://www.bcb.gov.br/content/estabilidadefinanceira/spb_docs/ManualBRCode.pdf)
- **QR composto variants:**
  - Recurrence only: URL at 80-25, and 62-05 txid.
  - Static + recurrence: 54 amount, 62-05, and 80-25.
  - Dynamic + recurrence: 26-25 payment URL plus 80-25 recurrence URL. **Both URLs must share the same `fqdnPspRecebedor`.**

  — [Manual §2.8](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Payload JSON for cob:** `revisao`, `calendario{criacao, apresentacao, expiracao}`, `devedor`, `valor{original, modalidadeAlteracao, retirada{saque|troco}}`, `chave`, `txid`, `solicitacaoPagador`, `infoAdicionais[]`, `status`, plus a **JWS signature** (details in the Manual de Segurança do SFN) — [Manual §2.7.1.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Pix Copia e Cola** is the exact same BR Code string as text, valid for static, dynamic and composto codes — [Manual §3.1](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)

### Inferences
- **Verified by code:** a CRC-16/CCITT-FALSE implementation (poly 0x1021, init 0xFFFF, no reflection, no xorout) over the example string up to and including `6304` yields `1D3D`, which matches the manual. Jupiter can use this as a unit-test fixture.
- The QR builder should emit IDs in ascending order, compute lengths in characters, cap 59 at 25 and 60 at 15, and for dynamic codes omit 54 and set 62-05 to `***`.
- The simulator's payer side should parse the TLV, validate the CRC, check the URL (no scheme, ≤77 chars, allow-listed domain), then GET `https://{url}` and verify the JWS before showing the amount.

### Gaps
- The Manual de Segurança do SFN (JWS alg, header, certificate requirements) was not retrieved.
- The standalone BR Code manual's full field-length table was not re-read. The lengths for 59/60/61 above come from secondary summaries plus the manual summary.

---

## 4. MED / MED 2.0, devolução rules, bloqueio cautelar, Pix Automático, Pix Garantido/Parcelado, Pix por aproximação / JSR

### Takeaway
- **MED 2.0** became mandatory on **2 Feb 2026**. It traces funds across intermediate accounts, sets an up-to-11-day window for recovery and blocking, and requires self-service (in-app) fraud contestation. From **1 Sep 2026** (IN BCB 766/2026), receivers have 80 days (previously 30) to contest a MED return.
- **Pix Automático** has been live since **16 Jun 2025**. It has four QR/API authorization journeys plus Open Finance. Receivers send charges 2–10 days before due, and the retry policy is `NAO_PERMITE` or `PERMITE_3R_7D`.
- **Pix por aproximação (NFC) and JSR** launched in Feb 2025 with an R$500 per-transaction cap, which is removed from **1 Oct 2026**.
- **Pix Parcelado** is operating via PSPs in 2026, but sources on its regulatory status conflict. **Pix Garantido** has not launched.

### Cited Findings
- **MED 2.0 in force:**
  - Mandatory for all Pix participants since **2 Feb 2026**.
  - Includes cascade tracking and expanded bloqueio cautelar.
  - Self-service contestation in the app became mandatory in Feb 2026.
  - Returns can be taken from accounts the money passed through, not only the first recipient.
  - Analysis and return attempts take 7–11 days.

  — [Transfeera: MED Pix 2.0](https://transfeera.com/blog/med-pix-2-0/); [Matera: MED 2.0](https://www.matera.com/br/blog/med-mecanismo-especial-de-devolucao/)
- **Agência Brasil (Feb 2026) on MED 2.0:**
  - The originating institution must notify the receiving one within **30 minutes**.
  - Recovery takes up to **11 days** after the dispute.
  - Funds suspected of fraud are blocked automatically for up to 11 days during analysis.
  - Victims can contest in the app without a human agent.

  — [Agência Brasil, Feb 2026](https://agenciabrasil.ebc.com.br/economia/noticia/2026-02/novas-regras-de-seguranca-do-pix-entram-em-vigor-veja-mudancas)
- **Deadlines:**
  - Victims have 80 days from the transfer to request a MED return (pre-existing rule).
  - **IN BCB nº 766/2026**, effective **1 Sep 2026**, extends from 30 to **80 days** the deadline for *receivers* to contest or request cancellation of a MED return.
  - The inter-institution analysis takes up to 7 days.

  — [meutudo (Sep 2026)](https://meutudo.com.br/blog/noticias/2026/09/02/prazo-para-contestacao-de-pix-devolvido-pelo-med-passa-de-30-para-80-dias-entenda)
- **Classic bloqueio cautelar and devolução timing:** the receiving PSP may block up to **72 h**, and the return happens within **96 h** once fraud is confirmed — [secondary aggregation via search; law-firm blogs](https://www.tafelliritz.com.br/blog/med-2-0-e-bloqueio-cautelar-do-pix-o-que-sua-empresa-precisa-saber-para-nao-ter-recebiveis-retidos-pelo-banco/). This is lower confidence and conflicts in framing with the "up to 11 days" MED 2.0 block reported by [Agência Brasil](https://agenciabrasil.ebc.com.br/economia/noticia/2026-02/novas-regras-de-seguranca-do-pix-entram-em-vigor-veja-mudancas).
- **Sep 2026 widening of devolução rules:** a news item (16 Sep 2026) reports that the BCB widened the devolução rules. I could not read it (HTTP 403) — [Mix Vale](https://www.mixvale.com.br/2026/09/16/banco-central-amplia-regras-para-devolucao-de-valores-do-pix/)
- **MED in the API:**
  - Devolução natures `MED_OPERACIONAL`, `MED_FRAUDE`, `MED_PIX_AUTOMATICO`.
  - Pix Automático returns are limited to `ORIGINAL`, `MED_PIX_AUTOMATICO`, `MED_FRAUDE`.
  - v2.10.0 added a `finalidadeMED` filter to `GET /pix`.

  — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml); [changelog](https://raw.githubusercontent.com/bacen/pix-api/master/changelog.md)
- **Pix Automático start:** launched **16 Jun 2025**, after tests from 28 Apr to 6 Jun 2025 — [Agência Gov](https://agenciagov.ebc.com.br/noticias/202506/pix-automatico-chega-em-16-de-junho). The receiver PSP must send the payment instruction **between 10 and 2 days** before the settlement date — [BCB FAQ Pix Automático](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/pix-automatico-FAQ-participantes.pdf) (via search summary).
- **Pix Automático mechanics:**
  - After the payer authorizes, the receiver sends periodic charges to its PSP via the API or a standardized file.
  - The receiver PSP sends the instruction to the payer PSP, which schedules and settles automatically.
  - The settlement date equals the due date, or the next business day if the receiver chooses.
  - Journeys map to Regulamento art. 11-Q §1º VII a–e, which Resolução BCB 402/2024 inserted. The Open Finance journey is item "e".

  — [Manual §3.3](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Journeys** — [Manual Anexo IV §4.4, §3.3.2](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf):
  - **J1:** the receiver pushes a confirmation request (`solicrec` to pain.009) to the payer PSP. This is the only journey that can end in REJEITADA.
  - **J2:** QR composto with only the recurrence location.
  - **J3:** QR with a cob location plus a recurrence location. The first payment is immediate.
  - **J4:** QR with a cobv location plus a recurrence location, or static data plus a recurrence location.
  - Recurring charges (cobr) have **no QR**.
- **Retries:**
  - Intraday retry for insufficient funds is the payer PSP's job, with no messaging.
  - Intraday retry for settlement error means a new instruction the same day. A new `RIFL` attempt gets a new endToEndId.
  - Post-due retry follows `politicaRetentativa`: `NAO_PERMITE` or `PERMITE_3R_7D` (up to 3 retries on different days within 7 calendar days of the original date).
  - Adjustments for late payment can only go onto the *next* charge.

  — [Manual Anexo I defs XIV–XVI, Anexo IV §2.1.5](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Pix Automático enums (v2.10.0)** — [openapi.yaml](https://raw.githubusercontent.com/bacen/pix-api/master/openapi.yaml):
  - Rec status: `CRIADA, APROVADA, REJEITADA, EXPIRADA, CANCELADA`.
  - Rec journey: `JORNADA_1..4, AGUARDANDO_DEFINICAO`.
  - Periodicidade: `SEMANAL, MENSAL, TRIMESTRAL, SEMESTRAL, ANUAL`.
  - SolicRec status: `CRIADA, ENVIADA, RECEBIDA, REJEITADA, ACEITA, EXPIRADA, CANCELADA`.
  - CobR status: `CRIADA, ATIVA, CONCLUIDA, EXPIRADA, REJEITADA, CANCELADA`.
  - Rec cancellation actors: `PSP_PAGADOR, USUARIO_PAGADOR, PSP_RECEBEDOR, USUARIO_RECEBEDOR`.
  - Rec cancellation codes: `ACCL, CPCL, DCSD, ERSL*, FRUD, PCFD*, SLCR, SLDB` (* deprecated).
  - Rec rejection codes: `AP13, AP14, AP15`.
  - CobR cancellation codes: `ACCT, BLCK, CCLD, FAIL, OTHR, SLBD, SLCR`.
  - EXPIRADA is set by the receiver PSP after `calendario.dataFinal`.
- **Pix por aproximação (NFC):**
  - The receiver device emits a "Pix Copia e Cola" encoded in a URI over NFC.
  - The flow is then identical to a QR read: the URL is resolved, security checks run, and the JSON payload is fetched.

  — [Manual §3.4](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **JSR (Jornada Sem Redirecionamento):**
  - Launched in Feb 2025. The wallet is linked to the bank account via Open Finance once, and later payments need no redirect.
  - The limit was R$500 per transaction. **From 1 Oct 2026** the regulatory R$500 cap is removed and the user's account limits apply instead.

  — [Brasil 247](https://www.brasil247.com/economia/bc-elimina-limite-do-pix-por-aproximacao-e-amplia-flexibilidade-aos-usuarios); [Mix Vale (25 Sep 2026)](https://www.mixvale.com.br/2026/09/25/pix-por-aproximacao-perde-limite-de-r-500-em-outubro/); [Sensedia on JSR obligation 2026](https://www.sensedia.com.br/post/fique-por-dentro-da-obrigatoriedade-da-jsr-no-open-finance-em-2026)
- **Open Finance initiation:** the minimum dataset uses initiation types `MANU, DICT, INIC, QRES, QRDN, AUTO`. `AUTO` (Pix Automático via Open Finance) was added in manual v2.10.0 — [Manual §3.2 and revision history](https://www.bcb.gov.br/content/estabilidadefinanceira/pix/Regulamento_Pix/II_ManualdePadroesparaIniciacaoDoPix.pdf)
- **Pix Parcelado:** the BCB said in Apr 2025 that it would launch in Sept 2025 — [Agência Brasil, Apr 2025](https://agenciabrasil.ebc.com.br/economia/noticia/2025-04/pix-parcelado-deve-ser-lancado-em-setembro-diz-banco-central). A 2026 blog says it "já funciona nos principais bancos", with credit extended by the payer PSP — [eCarts](https://ecarts.com.br/tecnologia/pix-no-credito-2026-pix-parcelado-pix-garantido-e-como-usar/). The latter is a low-quality source, and I found no BCB primary on whether standardized rules are in force.
- **Pix Garantido:** it uses future Pix receivables as loan collateral, for merchants and companies only. It is still in design, expected 2026–2027 — [Dock](https://dock.tech/fluid/blog/banking/pix-garantido/); [eCarts](https://ecarts.com.br/tecnologia/pix-no-credito-2026-pix-parcelado-pix-garantido-e-como-usar/)

### Inferences
- For a portfolio simulator, MED can be modeled as a separate flow:
  1. a fraud notification (infração) against an e2eid,
  2. a bloqueio cautelar on the receiver account,
  3. analysis (≤7 days) and then a return via pacs.004 with nature `MED_FRAUDE`,
  4. with MED 2.0, "cascade" hops to downstream accounts inside the 11-day window,
  5. the receiver's contest window of 80 days (post IN 766/2026).
- Pix Automático is the most "Jupiter-worthy" differentiator. It needs the rec/solicrec/cobr resources, the 2–10 day scheduling validator, the 3R_7D retry scheduler, and the webhookrec/webhookcobr callbacks.

### Gaps
- I could not read the primary BCB texts: the Regulamento Pix (Res. BCB 1/2020 as amended), Resolução BCB 402/2024, the MED 2.0 resolution number, and IN 766/2026. Exact MED legal deadlines (72 h / 96 h vs 11 days) should be verified against the Regulamento and the Manual Operacional do DICT (MED lives in DICT: infração, notificação, solicitação de devolução).
- Pix Parcelado's regulatory status in 2026 is unclear and sources conflict.
- Transaction limits (nightly limits, R$200 limit for new devices and web browsers under the 2024–2025 security rules) were not researched.

---

## 5. Boleto: código de barras (44), linha digitável (47), fator de vencimento (2025 rollover), DVs, registration (CIP/Núclea NPC), boleto híbrido

### Takeaway
The cobrança barcode is 44 digits:

| Positions | Field |
|---|---|
| 1–3 | bank |
| 4 | currency (9 = Real) |
| 5 | DV geral (mod 11) |
| 6–9 | fator de vencimento |
| 10–19 | valor in cents |
| 20–44 | campo livre (bank-defined) |

The 47-digit linha digitável rearranges these into 5 fields. Fields 1–3 each carry a mod-10 DV, field 4 is the DV geral, and field 5 is fator + valor. The fator counts days from 07/10/1997. It hit 9999 on 21/02/2025 and **restarted at 1000 on 22/02/2025**. All boletos are registered in the CIP/Núclea centralized base (Nova Plataforma de Cobrança). Boleto híbrido adds a Pix QR (usually dynamic cobv).

### Cited Findings
- **Barcode layout:** 44 digits, "três para o banco, um para a moeda, um dígito verificador geral, quatro para o fator de vencimento, dez para o valor em centavos e vinte e cinco para o campo livre". The 47-digit linha digitável is the same content reorganized into five fields, three of them with their own mod-10 DV — search summary of [FEBRABAN layout](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20-%20C%C3%B3digo%20de%20Barras%20ATUALIZADO.pdf) / [Efí: campos dos boletos](https://sejaefi.com.br/blog/campos-dos-boletos-linha-digitavel)
- **Physical barcode:** it is 46 positions (44 data digits plus start and stop). The numeric representation is split into blocks of 11 with a DV (mod 10 or 11). This is from FEBRABAN's **arrecadação** layout (concessionárias and tributos, starting with `8`, 48-digit line), version 06, in force from 01/11/2020 — [FEBRABAN Layout Código de Barras v06](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20-%20C%C3%B3digo%20de%20Barras%20ATUALIZADO.pdf)
- **FEBRABAN mod-10 DAC:** multipliers 2,1,2,1… from right to left. **Mod-11 DAC:** multipliers 2..9 cycling, from right to left — [FEBRABAN Layout Código de Barras v06 §07–10](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20-%20C%C3%B3digo%20de%20Barras%20ATUALIZADO.pdf)
- **Fator de vencimento rollover:**
  - Days since the base date 07/10/1997.
  - It reached **9999 on 21/02/2025** and **reset to 1000 on 22/02/2025**, then increments daily.

  — [KMEE](https://kmee.com.br/blog/mudanca-fator-vencimento-boletos-2025/); [Senior](https://documentacao.senior.com.br/noticias/2025/2025-01-29-fator-vencimento-febraban.htm); [ACBr forum on divergent due dates](https://www.projetoacbr.com.br/forum/topic/82120-boletos-vencimento-do-boleto-divergente-ao-fator-de-vencimento-novo-regra-febraban/)
- **Registration and NPC:**
  - On payment, the centralized CIP base is consulted and the registered data is shown to the payer for confirmation.
  - Núclea (formerly CIP) has operated it since 2017 under FEBRABAN self-regulation.
  - The "CMP" evolution project brings D+0 settlement for boletos paid by late afternoon, a single irrevocable "baixa", and a study to include a Pix QR in boletos.

  — [Núclea / FEBRABAN via search summary](https://www.nuclea.com.br/plataforma-centralizada-de-recebiveis/); [FEBRABAN Nova Plataforma](https://portal.febraban.org.br/pagina/3150/1094/pt-br/servicos-novo-plataforma-boletos); [Caixa FAQ NPC](https://www.caixa.gov.br/Downloads/cobranca-caixa/Perguntas_Frequentes_Nova_Plataforma_de_Cobranca_v1-06.pdf)
- **Boleto híbrido:** the boleto is registered in CIP, cleared and settled crosswise, and printed with both the traditional barcode and a Pix QR Code — [Corner Pix](https://cornerpix.com.br/boletos/corner-pix-boleto-hibrido/)
- **Boleto híbrido in CNAB 240:**
  - Remessa movement `'61'` = "Alteração para inclusão/manutenção de QR Code Pix".
  - Distribution codes `'P'` (bank registers, client distributes boleto with QR Pix) and `'Q'` (bank registers and distributes).
  - Segment Y-03 (movement/e-mail/SMS record) carries `Tipo de Chave PIX` (pos 81), `Chave PIX / URL do QRCode` (82–158, 77 chars) and `TXID` (159–193, 35 chars). "A partir da URL retornada… o beneficiário deverá formatar o QRCode Dinâmico conforme manuais de Padrões para Iniciação do Pix e do BR Code".
  - Return reasons: `P1` registered with QR Pix, `P2` registered without QR Pix, `P3` invalid key, `P4` key not in DICT, `P5` key incompatible with CNPJ, `P8`/`P9` QR alteration or cancellation not allowed.
  - Liquidation channel `'61'` = "Liquidado via Pix".

  — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf)

### Inferences
- **Verified by code** on a real barcode (`23797404300001240200448056168623793601105800`, Bradesco, sample from [TTrix](https://www.ttrix.com/apple/iphone/boletoscan/boletoanatomia.html)):
  - **DV geral** is mod 11 over the 43 digits excluding position 5, with weights 2..9 cycling from right to left. With `r = 11 − (sum mod 11)`, the DV is 1 when r ∈ {0, 10, 11} and r otherwise. This yields 7, which matches position 5.
  - The **linha digitável** is `field1 = barcode[0:4]+barcode[19:24] + mod10`, `field2 = barcode[24:34] + mod10`, `field3 = barcode[34:44] + mod10`, `field4 = barcode[4]`, `field5 = barcode[5:19]`. That gives 47 digits.
  - The **boleto mod 10** uses weights 2,1 from the right, sums the digits of each product, and computes `(10 − sum mod 10) mod 10`.
  - Fator 4043 on the old base is 2008-11-01, and valor = 1240.20.
- **Fator decoding after the rollover:** the new cycle maps 1000 to 2025-02-22 and 9999 to 2049-10-13. For factors < ~4000 there is ambiguity with the old cycle. A practical decoder picks the date in the cycle closest to "today" (e.g. within −3000/+5500 days). This is common practice in libraries, but I found no FEBRABAN-specified disambiguation rule in the sources above.
- The per-bank campo livre (positions 20–44) differs by bank: BB has convênio/nosso número/carteira, Itaú carteira/nosso número/agência/conta, and so on. Jupiter needs a per-bank strategy interface.

### Gaps
- I did not obtain the FEBRABAN *cobrança* barcode spec text (the BB Doc5175 PDF returned 403). I also did not obtain per-bank campo livre layouts. The DV/linha algorithms above are verified empirically, not quoted.
- These NPC details were not researched: rules on partial payment, min/max amounts, payment after due date, and the CMP project's D+0 go-live date.

---

## 6. CNAB 240 / CNAB 400: remessa/retorno structure, segments, occurrence codes, bank differences

### Takeaway
CNAB 240 (FEBRABAN v10.11, 31/07/2023) is organized as follows:
- **File:** header de arquivo (type 0) → batches (header de lote type 1, detail type 3 by segment, trailer de lote type 5) → trailer de arquivo (type 9).
- **Cobrança batch (layout de lote `060`, file layout `103`):**
  - Remessa uses segments **P+Q** (mandatory) and **R/S/Y** (optional).
  - Retorno uses **T+U** (mandatory) and **Y** (optional).
- **Movement codes:** C004 for remessa, C044 for retorno, with C047 reasons.

CNAB 400 is a legacy single-record-per-title format that is **bank-specific**; FEBRABAN has no unified standard for it.

### Cited Findings
- **Header de Arquivo (240)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 1–3 | banco |
  | 4–7 | lote `0000` |
  | 8 | registro `0` |
  | 18 | tipo inscrição |
  | 19–32 | CNPJ/CPF |
  | 33–52 | convênio |
  | 53–57 | agência |
  | 58 | DV agência |
  | 59–70 | conta |
  | 71 | DV conta |
  | 72 | DV ag/conta |
  | 73–102 | nome empresa |
  | 103–132 | nome banco |
  | 143 | código remessa(1)/retorno(2) |
  | 144–151 | data geração DDMMAAAA |
  | 152–157 | hora HHMMSS |
  | 158–163 | NSA |
  | 164–166 | layout do arquivo `103` |
  | 167–171 | densidade |
  | 172–191 | reservado banco |
  | 192–211 | reservado empresa |
  | 212–240 | brancos |

- **Header de Lote, Cobrança** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 1–3 | banco |
  | 4–7 | lote |
  | 8 | `1` |
  | 9 | operação (R/T) |
  | 10–11 | serviço `01` |
  | 14–16 | layout do lote `060` |
  | 18 | tipo inscrição |
  | 19–33 | inscrição (15) |
  | 34–53 | convênio |
  | 54–58 | agência |
  | 59 | DV |
  | 60–71 | conta |
  | 72 | DV |
  | 73 | DV ag/conta |
  | 74–103 | nome |
  | 104–143 | mensagem 1 |
  | 144–183 | mensagem 2 |
  | 184–191 | nº remessa/retorno |
  | 192–199 | data gravação |
  | 200–207 | data crédito |
  | 208–240 | brancos |

- **Segmento P (remessa)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 9–13 | seq |
  | 14 | `P` |
  | 16–17 | cód. movimento (C004) |
  | 18–22 | agência |
  | 23 | DV agência |
  | 24–35 | conta |
  | 36 | DV conta |
  | 37 | DV ag/conta |
  | 38–57 | nosso número (20) |
  | 58 | carteira (1 simples, 2 vinculada, 3 caucionada, 4 descontada, 5 vendor, 6 cessão) |
  | 59 | cadastramento |
  | 60 | tipo documento |
  | 61 | emissão boleto |
  | 62 | distribuição (incl. `P`/`Q` for Pix) |
  | 63–77 | nº documento |
  | 78–85 | vencimento DDMMAAAA |
  | 86–100 | valor nominal (13,2) |
  | 101–105 | agência cobradora |
  | 106 | DV |
  | 107–108 | espécie |
  | 109 | aceite |
  | 110–117 | emissão |
  | 118 | cód. juros |
  | 119–126 | data juros |
  | 127–141 | juros/dia ou taxa |
  | 142 | cód. desconto 1 |
  | 143–150 | data desconto 1 |
  | 151–165 | valor/% desconto 1 |
  | 166–180 | IOF |
  | 181–195 | abatimento |
  | 196–220 | uso da empresa (seu número, 25) |
  | 221 | cód. protesto |
  | 222–223 | prazo protesto |
  | 224 | cód. baixa/devolução |
  | 225–227 | prazo baixa |
  | 228–229 | moeda |
  | 230–239 | contrato |
  | 240 | uso livre / autorização pagamento parcial |

- **Segmento Q (remessa, pagador)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 16–17 | movimento |
  | 18 | tipo inscrição |
  | 19–33 | inscrição |
  | 34–73 | nome |
  | 74–113 | endereço |
  | 114–128 | bairro |
  | 129–133 | CEP |
  | 134–136 | sufixo CEP |
  | 137–151 | cidade |
  | 152–153 | UF |
  | 154–209 | sacador/avalista (tipo, inscrição, nome) |
  | 210–212 | banco correspondente |
  | 213–232 | nosso nº no correspondente |
  | 233–240 | brancos |

- **Segmento T (retorno)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 16–17 | cód. movimento retorno (C044) |
  | 18–37 | agência/conta/DVs |
  | 38–57 | nosso número |
  | 58 | carteira |
  | 59–73 | nº documento |
  | 74–81 | vencimento |
  | 82–96 | valor nominal |
  | 97–99 | banco cobrador/recebedor |
  | 100–104 | agência cobradora |
  | 105 | DV |
  | 106–130 | uso da empresa |
  | 131–132 | moeda |
  | 133–188 | pagador (tipo, inscrição, nome) |
  | 189–198 | contrato |
  | 199–213 | tarifa/custas |
  | 214–223 | motivo da ocorrência (up to 5 × 2-char codes, C047) |
  | 224–240 | brancos |

  DDA specifics: 02 plus motivo `A4` means pagador DDA; `51`/`52` mean recognized or not recognized by the pagador; `53` means recusado pela CIP.
- **Segmento U (retorno, amounts)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 18–32 | juros/multa/encargos |
  | 33–47 | desconto |
  | 48–62 | abatimento |
  | 63–77 | IOF |
  | 78–92 | **valor pago** |
  | 93–107 | **valor líquido creditado** |
  | 108–122 | outras despesas |
  | 123–137 | outros créditos |
  | 138–145 | data ocorrência |
  | 146–153 | **data do crédito** |
  | 154–157 | código ocorrência pagador |
  | 158–165 | data ocorrência pagador |
  | 166–180 | valor ocorrência |
  | 181–210 | complemento |
  | 211–213 | banco correspondente |
  | 214–233 | nosso nº correspondente |

- **Trailer de Lote (cobrança)** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):

  | Positions | Field |
  |---|---|
  | 8 | `5` |
  | 18–23 | qtd registros |
  | 24–46 | qtd/valor cobrança simples |
  | 47–69 | qtd/valor vinculada |
  | 70–92 | qtd/valor caucionada |
  | 93–115 | qtd/valor descontada |
  | 116–123 | nº aviso lançamento |

- **C004 remessa codes** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):
  - 01 Entrada de Títulos
  - 02 Pedido de Baixa
  - 03 Protesto p/ fins falimentares
  - 04 / 05 Concessão / cancelamento de abatimento
  - 06 Alteração de Vencimento
  - 07 / 08 Concessão / cancelamento de desconto
  - 09 Protestar
  - 10 / 11 Sustar protesto (and baixar / manter)
  - 12 Alteração de juros
  - 13 Dispensar juros
  - 14 Alteração de multa
  - 15 Dispensar multa
  - 16–24 other alterations
  - 30 Recusa da alegação do pagador
  - 31 Alteração de outros dados
  - 40–49 carteira / espécie / contrato / negativação / valor nominal / mín / máx
  - **61 Inclusão/manutenção de QR Code Pix**
- **C044 retorno codes** — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf):
  - **02 Entrada Confirmada**
  - **03 Entrada Rejeitada**
  - 04 / 05 Transferência de carteira
  - **06 Liquidação**
  - 07 / 08 Desconto
  - **09 Baixa**
  - 11 Em ser
  - 12 / 13 Abatimento
  - 14 Alteração de vencimento
  - 15 Franco de pagamento
  - **17 Liquidação após baixa / título não registrado**
  - 19 / 20 Protesto
  - 23 Remessa a cartório
  - 24 Retirada de cartório
  - 25 Protestado e baixado
  - **26 Instrução rejeitada**
  - 27 Confirmação alteração outros dados
  - **28 Débito de tarifas/custas**
  - 29 Ocorrências do pagador
  - 30 Alteração de dados rejeitada
  - 33–65 various confirmations (51 / 52 / 53 DDA)
  - 93 Intenção de pagamento
  - 94 Cancelamento de intenção de pagamento
- **C047 reason groups:** group A covers rejection codes 01–95 for movements 02/03/26/30. Group B covers movement 28, tarifas. Group C covers movements 06/09/17, liquidation channel and baixa type. Group C examples: 30 guichê cheque, 31 correspondente, 32 ATM, 33 internet banking, 34 office banking, 37 central telefone, **61 Liquidado via Pix**, 09 baixa comandada banco, 10 baixa comandada cliente arquivo, 11 baixa comandada cliente online — [FEBRABAN CNAB 240 v10.11](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf)
- **Segments by direction:** P and Q are mandatory in remessa, with R, S, Y-03 and Y-53 optional. T and U are mandatory in retorno, with Y-03 and Y-04 optional — [FEBRABAN CNAB 240 v10.11 (search summary)](https://cmsarquivos.febraban.org.br/Arquivos/documentos/PDF/Layout%20padrao%20CNAB240%20V%2010%2011%20-%2021_08_2023.pdf)
- **Bank variants:** banks publish their own "240 FEBRABAN" manuals with deviations, e.g. [Santander CNAB 240 v2.7 (2017)](https://cms.santander.com.br/sites/WPS/documentos/arq-layout-de-arquivos-2/17-10-26_172236_149-382-cobranca+layout+cnab+240+febraban+puro+versao+setembro+2017.pdf), [Unicred remessa 240](https://www.unicred.com.br/site/LayoutdeRemessa-de-Boletos-CNAB-240.pdf), [Bradesco 240](https://suporte.quarta.com.br/LayOuts/Bancos/28-Bradesco(CNAB240-FEBRABAN).pdf). An open-source multi-bank parser exists: [cnab-lens](https://github.com/BrunodosSantosVaz/cnab-lens/issues/7).

### Inferences
- A minimum viable CNAB 240 cobrança implementation for Jupiter:
  - **Remessa writer:** header 0, lote 1 (`060`), then P+Q per title (Y-03 when hybrid with Pix), trailer 5, trailer 9. All lines are exactly 240 chars. Numeric fields are zero-padded on the left, alpha fields space-padded on the right, dates are DDMMAAAA, and money is in cents with implied decimals.
  - **Retorno parser:** pair T+U by sequence and map C044 to domain events: 02 → registered, 03 → rejected (with C047 reasons), 06/17 → paid (valor pago U78–92, data crédito U146–153, channel from motivo, e.g. 61 = Pix), 09 → cancelled, 28 → fee.
  - **Simulator:** it plays the bank. It consumes remessa, emits retorno with 02/03, and later 06 on a simulated payment. This mirrors the Pix simulator's webhook.
- Per-bank differences mostly sit in the convênio/agência/conta fields, the nosso número format and DV, the carteira codes, and which optional segments are accepted. That suggests a `BankProfile` strategy.

### Gaps
- **CNAB 400** was not researched from primary sources. It is bank-specific: each bank publishes its own layout (Itaú, Bradesco, BB, Santander, Sicoob and others) with 400-char header "0", detail "1" and trailer "9", and occurrence codes that are also bank-specific. Treat CNAB 400 as a per-bank adapter and fetch the specific bank manual if needed.
- These were not reviewed: bank REST APIs (BB API Cobrança v2, Itaú Cash Management/Boleto API, Inter API Banking with mTLS, Sicoob, Efí), and their mapping of boleto registration and boleto híbrido (Efí/Inter return `pixCopiaECola` alongside `linhaDigitavel`).
- Segmento R (second and third discount, multa) and S (messages) positions were not extracted, but they are in the same FEBRABAN PDF.
- There may be a CNAB 240 version newer than 10.11 (Aug 2023). I did not confirm one as of 2026.
