# PSP public API design: a comparison of Stripe, Adyen and Brazilian PSPs (as of Sep 2026)

Scope: API resources, object lifecycles and developer-experience conventions, to inform the "Jupiter" Go payments backend (Stripe-grade API plus Brazilian methods). Research date: 2026-09-29. All claims were fetched or searched this session unless they are marked as inference.

## 1. Stripe: resources, PaymentIntent state machine, idempotency, versioning, pagination, expand, webhooks, errors, metadata, Pix and boleto

### Takeaway
Stripe's API is still the reference design. It uses one intent object with a status machine that is shared by PaymentIntent and SetupIntent, an append-only BalanceTransaction ledger, a client-supplied `Idempotency-Key`, date-named versions pinned per account, cursor pagination, `expand[]`, HMAC-signed webhooks with a timestamp, and a typed error object. Since 2024 Stripe has run a second namespace (`/v2`) that changes several of these conventions: JSON bodies, page-token pagination, thin events, 30-day retry-safe idempotency and `include` in place of `expand`. Jupiter should copy these v2 choices. Stripe's own Brazilian support is thin. Brazilian accounts get only one-time Pix (no Pix Automático), and boleto has no refunds.

### Cited Findings

**PaymentIntent / SetupIntent lifecycle**
- PaymentIntent and SetupIntent go through the same states: `requires_payment_method` → `requires_confirmation` (most integrations skip it by sending the payment method at confirm) → `requires_action` (for example 3DS) → `processing` (async methods such as bank debits can take days) → `succeeded` | `canceled`. `requires_capture` appears when authorisation and capture are separate, and capturing moves the intent to `processing` or `succeeded` — [Stripe lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- A failed attempt (for example a decline) sends the PaymentIntent back to `requires_payment_method` so it can be retried. The intent is the container for many attempts, and Stripe recommends creating it "as soon as you know the amount" so that every attempt is recorded — [Stripe lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- Cancel is allowed only before `processing`/`succeeded`, except that some bank debits (ACH, SEPA, BACS and others) can be cancelled while `processing` on a best-effort basis. Cancelling cannot be undone and releases held funds. A PaymentIntent can auto-cancel if it is confirmed too many times — [Stripe lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- Status names changed on 2019-02-11: `requires_source` → `requires_payment_method` and `requires_source_action` → `requires_action`. This is an example of a renaming handled by versioning — [Stripe lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)
- In 2026 Stripe's docs point new integrations to the Checkout Sessions API (with Payment Element) instead of raw PaymentIntents. Some features, such as Adaptive Pricing, exist only on Checkout Sessions — [Stripe lifecycle](https://docs.stripe.com/payments/paymentintents/lifecycle)

**Balance ledger**
- BalanceTransaction fields: `amount` (gross, in minor units; positive = in, negative = out), `fee`, `fee_details[]`, `net` (= amount − fee), `currency`, `exchange_rate`, `available_on`, `status` (`pending`|`available`), `balance_type` (`payments`, `issuing`, `risk_reserved`, `refund_and_dispute_prefunding`), `reporting_category`, `source` (expandable link to the charge, refund, transfer or payout) and `type`. There are about 50 types, including `charge`, `payment`, `refund`, `payout`, `payout_failure`, `transfer`, `application_fee`, `reserve_hold`/`reserve_release`, `advance`/`advance_funding`/`anticipation_repayment` and `stripe_fee` — [BalanceTransaction object](https://docs.stripe.com/api/balance_transactions/object)
- Stripe tells accountants to classify by `reporting_category`, not `type` — [BalanceTransaction object](https://docs.stripe.com/api/balance_transactions/object)

**Connect (multi-party)**
- There are three charge types: direct charges, destination charges, and separate charges and transfers. The merchant of record (MoR) depends on the type. The connected account is MoR for direct charges or when `on_behalf_of` is set. The platform is MoR for destination charges or for separate charges and transfers without `on_behalf_of` — [Stripe Pix / Connect section](https://docs.stripe.com/payments/pix)
- Per-account payment-method enablement is modelled as "capabilities", for example `pix_payments` — [Stripe Pix](https://docs.stripe.com/payments/pix)
- Accounts v2 (`/v2/core/accounts`) is one Account object with `applied_configurations` such as `merchant`, `customer` and `recipient`. Related data comes back through `include[]=requirements|defaults|identity`. Payout methods live under `/v2/money_management/payout_methods` — [API v2 overview](https://docs.stripe.com/api-v2-overview)

**Idempotency**
- In v1, the `Idempotency-Key` header is accepted on every POST and ignored on GET/DELETE. Keys can be up to 255 characters, and Stripe recommends V4 UUIDs and warns against using PII in keys. Stripe saves the status code and body of the first request whether it succeeded or failed, 500s included, and replays it. Keys can be pruned after at least 24 hours. Parameters that differ from the original request return an error (`idempotency_error`). Nothing is saved if validation fails or if the request conflicts with a concurrent one, because the endpoint never started executing. Those requests are safe to retry — [Idempotent requests](https://docs.stripe.com/api/idempotent_requests)
- A 409 Conflict can mean a clash with another request, for example one using the same idempotency key — [Errors](https://docs.stripe.com/api/errors)
- In v2, idempotency covers POST and DELETE. A replay window is 30 days, scoped to the same account and API. If the first request failed or partly failed, a replay re-executes it without side effects instead of returning the cached error. If no key is sent, Stripe generates a UUID — [API v2 overview](https://docs.stripe.com/api-v2-overview)
- Brandur Leach, Oct 2017, describes the canonical server design. It uses an `idempotency_keys` table with a unique constraint on `(user_id, idempotency_key)`, plus `request_method`, `request_path`, `request_params`, `locked_at`, `recovery_point`, `response_code` and `response_body`. Work is split into "atomic phases" between "foreign state mutations", with named recovery points from `started` to `finished`. Concurrent use of the same key returns 409. A "reaper" deletes keys after about 72 hours and a "completer" pushes abandoned requests to completion — [brandur.org/idempotency-keys](https://brandur.org/idempotency-keys)

**Versioning**
- Brandur Leach (Aug 5, 2017) describes the original design. Versions are dates. An account is pinned automatically on its first call, and overrides go through the `Stripe-Version` header or the dashboard. Backwards-incompatible changes live in "version change modules" that transform responses backwards step by step to the target version. Safe changes are additive: new endpoints and new optional fields — [stripe.com/blog/api-versioning](https://stripe.com/blog/api-versioning)
- Since `2024-09-30.acacia`, Stripe ships a monthly version with no breaking changes and a major release twice a year that carries the breaking changes. Version names combine a date and a release name. The current version is `2026-08-26.dahlia`, after acacia, basil, clover and others. Webhook endpoints can pin their own API version, and otherwise use the account default. Strongly typed SDKs such as `stripe-go` are fixed to the API version that was current when that SDK version was released — [Versioning](https://docs.stripe.com/api/versioning)
- Every `/v2` request must send `Stripe-Version` explicitly — [API v2 overview](https://docs.stripe.com/api-v2-overview)
- An Event keeps the API version that applied when it was created. Upgrading later does not rewrite past events — [Webhooks](https://docs.stripe.com/webhooks)

**Pagination, expand and metadata**
- In v1, lists use `limit` (1–100, default 10) and object-ID cursors `starting_after`/`ending_before`, which are mutually exclusive, in reverse chronological order. The envelope is `{object:"list", data, has_more, url}` — [Pagination](https://docs.stripe.com/api/pagination)
- In v2, lists use a `page` token and return `next_page_url`/`previous_page_url`. Filters cannot change after the first request. v2 lists are eventually consistent and lower-latency, while v1 top-level lists are immediately consistent — [API v2 overview](https://docs.stripe.com/api-v2-overview)
- `expand[]` replaces an ID with the full object. It supports dot notation up to 4 levels and a `data.` prefix inside lists, and it has a performance cost. Webhook payloads are never expanded — [Expand](https://docs.stripe.com/expand)
- v2 drops `expand` and uses `include[]` for opt-in fields — [API v2 overview](https://docs.stripe.com/api-v2-overview)
- Metadata allows up to 50 keys, key names up to 40 characters and values up to 500 characters, all stored as strings, with no `[` or `]` in keys. Stripe never uses metadata for decisions, and docs say not to store sensitive data in it. To delete a key, v1 uses an empty string and v2 uses `null` — [Metadata](https://docs.stripe.com/api/metadata); [API v2 overview](https://docs.stripe.com/api-v2-overview)

**Test vs live**
- Objects carry `livemode`, and test and live webhook secrets differ for the same endpoint — [Pagination sample](https://docs.stripe.com/api/pagination); [Webhooks](https://docs.stripe.com/webhooks)
- Sandboxes are the isolated test environment for both namespaces. Not every `/v2` API supports the older test mode — [API v2 overview](https://docs.stripe.com/api-v2-overview)

**Webhooks and events**
- The header looks like `Stripe-Signature: t=<unix>,v1=<hex>[,v1=...][,v0=...]`. The signature is HMAC-SHA256 over `"{t}.{raw_body}"` with the endpoint secret (`whsec_`). Receivers should ignore any scheme other than `v1` to prevent downgrade attacks, and compare in constant time. SDKs allow a default tolerance of 5 minutes, and a tolerance of 0 is discouraged. Each retry is signed again with a new timestamp. When a secret is rolled, the old one can stay valid for up to 24 hours and each delivery carries one signature per secret. The raw body is required — [Webhooks](https://docs.stripe.com/webhooks); [Signature troubleshooting](https://docs.stripe.com/webhooks/signature)
- In live mode, delivery is retried with exponential backoff for up to 3 days. In sandbox it is retried 3 times over a few hours. Any 3xx counts as a failure. Manual resend is available for 15 days in the Dashboard and 30 days through the CLI. Order is not guaranteed, so receivers should deduplicate on event `id` and not use `created`, which has only second resolution. Receivers should reply 2xx quickly and work from a queue. Endpoints need TLS 1.2 or later, there is an IP allowlist, and an account can have up to 16 endpoints — [Webhooks](https://docs.stripe.com/webhooks)
- Snapshot events embed the object as it was at event time in `data.object`. Thin events carry a minimal, unversioned payload, and the handler calls `fetchRelatedObject()`/`fetchEvent()`. All `/v2` events are thin. Destinations are created through `/v2/core/event_destinations`, with `event_payload: snapshot|thin` and `events_from: ["@self"|"@accounts"]`, and can target EventBridge or Azure Event Grid — [Webhooks](https://docs.stripe.com/webhooks); [API v2 overview](https://docs.stripe.com/api-v2-overview)

**Error model**
- HTTP codes used: 400, 401, 402 (valid parameters but the request failed, for example a decline), 403, 404, 409, 424 (external dependency failed), 429 (use exponential backoff) and 5xx. Error `type` is one of `api_error`, `card_error`, `idempotency_error` or `invalid_request_error`. Other fields are `code`, `decline_code`, `advice_code`, `network_decline_code`, `network_advice_code`, `message` (safe to show to end users for card errors), `param`, `doc_url` and `request_log_url`, plus the related `payment_intent`, `setup_intent` or `payment_method` object — [Errors](https://docs.stripe.com/api/errors)

**Pix and boleto on Stripe**
- Pix is BRL-only. It is customer-initiated and real-time, with no manual capture. Full and partial refunds are allowed within 90 days, and disputes are allowed only in limited cases (MED-like) and cannot be contested. Amounts must be between R$0.50 and R$3,000. Pix Automático handles recurring payments through a mandate. Brazilian Stripe accounts can accept only one-time Pix settled in BRL, and "Pix Automático isn't available in Brazil". International accounts in the US, EU, UK and elsewhere accept Pix through the partner Ebanx and pay IOF of 3.5% (`amount_includes_iof: never|always`). The statement descriptor shows Ebanx — [Stripe Pix](https://docs.stripe.com/payments/pix)
- Boleto is for Brazilian accounts only. Amounts must be between R$5.00 and R$49,999.99. The voucher expires after 3 days by default, configurable from 0 to 60. Confirmation is async, usually by the next business day, and settlement is T+2. There is no manual capture, no refunds of any kind and no disputes. The customer-visible number is in `next_action.boleto_display_details.number` — [Stripe boleto](https://docs.stripe.com/payments/boleto)

### Inferences
- For Jupiter, model Pix and boleto as PaymentIntents that sit in `requires_action` (with a QR or voucher in `next_action`) and then move to `succeeded` or `canceled` on expiry. This follows Stripe's pattern and keeps one state machine.
- A Go service should use v2-style conventions: JSON bodies, opaque page tokens, `include`, thin events with fetch-latest, and idempotency that retries failures. These are simpler to implement correctly than form encoding plus snapshot versioning.
- Stripe's gaps in Brazil (no Pix Automático, no boleto refunds, no split or anticipation primitives) are where a Brazilian-focused API can add value.

### Gaps
- I did not fetch the full object schemas for Charge, Refund, Dispute, Payout or Transfer this session. Their status enums (for example, dispute statuses such as `warning_needs_response`, `needs_response`, `under_review`, `won` and `lost`) should be checked on docs.stripe.com/api before they are used.
- I did not verify the API key prefix conventions (`sk_test_`, `sk_live_`, `rk_`, `pk_`) or the details of restricted keys, apart from the `sk_test_` key that appears in examples.

## 2. Adyen: /payments, /payments/details, async modifications, webhooks, pspReference, platform split

### Takeaway
Adyen treats every modification as an asynchronous request. The synchronous response only says `received` and gives a new `pspReference`, and the real outcome arrives by webhook. Every operation gets its own pspReference, linked to the original through `originalReference`. For platforms, splits are an array of typed booking instructions: BalanceAccount, Commission, PaymentFee, Remainder and others. Splits can be sent at authorisation or at capture, or set up through configuration profiles.

### Cited Findings
- The modification types are capture, cancel (before capture), refund (after capture), reversal (when the capture state is unknown) and authorisation adjustment — [Adyen modify payments](https://docs.adyen.com/online-payments/modify-payments/)
- Capture is `POST /payments/{paymentPspReference}/captures` with `merchantAccount` and `amount`. The response is `status: "received"` with a new `pspReference`, and processing is asynchronous. The `CAPTURE` webhook carries `success` true/false, `originalReference` (the authorisation's pspReference) and `pspReference` (the capture's). `CAPTURE_FAILED` means the scheme or issuer rejected the capture later — [Adyen capture](https://docs.adyen.com/online-payments/capture/)
- Automatic capture is the default. Delayed automatic capture and manual capture are also available. A single partial capture cancels the remainder automatically, and multiple partial captures need to be enabled by Adyen support — [Adyen capture](https://docs.adyen.com/online-payments/capture/)
- Webhooks must be acknowledged with a 2xx or an "accepted" response. Authenticity is checked with an HMAC signature in `additionalData.hmacSignature`, and basic auth is also supported. Webhooks deliver status for async flows and for external events such as chargebacks, onboarding verification and reports — [Adyen webhooks](https://docs.adyen.com/development-resources/webhooks/)
- On platforms, split instructions sent in the authorisation request book the sale amount, the platform's commission, transaction fees and the FX leftover separately. Split types include `BalanceAccount` (needs `account`), `Commission`, `PaymentFee` (all fees, or broken down by fee kind to different balance accounts) and `Remainder` — [Split at authorization](https://docs.adyen.com/marketplaces/split-transactions/split-payments-at-authorization); [Hyperswitch summary of Adyen splits](https://docs.hyperswitch.io/explore-hyperswitch/payment-orchestration/split-payments/adyen-split-payments)
- Splits can also be sent at capture — [Split at capture](https://docs.adyen.com/platforms/online-payments/split-transactions/split-payments-at-capture)
- Split configuration profiles can be created through Management API v3 (`POST /merchants/{merchantId}/splitConfigurations`) — [Adyen API Explorer](https://docs.adyen.com/api-explorer/Management/3/post/merchants/(merchantId)/splitConfigurations)
- There is a separate transaction fees doc for platforms — [Adyen transaction fees](https://docs.adyen.com/platforms/online-payments/transaction-fees)

### Inferences
- Adyen's "every modification gets its own reference and settles by webhook" model fits Pix refunds (devoluções) and boleto cancellations, which are asynchronous at the rail level. Jupiter's Refund object should therefore carry `pending`, `succeeded` and `failed` states, and should not assume refunds are synchronous.
- A split as an array of `{type, account, amount, reference}` bookings, where fees and remainder are explicit line types, is cleaner than boolean flags (see Pagar.me, section 3).

### Gaps
- `/payments/details` (used for redirect, 3DS and action results) was not fetched. Its role, submitting `details` or `redirectResult` after an action, is known from general knowledge but was not verified this session.
- I did not confirm the current Checkout API version number (v71 or v72 or later), webhook retry timings, or whether Adyen guarantees delivery order.
- The Balance Platform objects (legal entities, account holders, balance accounts, transfers and payouts through the Transfers API) were not fetched.

## 3. Brazilian PSPs: how they model split, recipients, anticipation and payouts

### Takeaway
Brazilian PSPs share a common vocabulary. A recipient or sub-account (recebedor, subconta, walletId) receives an array of split rules, each a flat or percentage amount, attached to the order or charge. Around that sit an anticipation resource (advancing card receivables against a fee) and transfer or withdrawal settings on the recipient. They differ on who pays fees and chargebacks. Pagar.me uses per-rule boolean flags. Asaas splits the net value after fees. Mercado Pago uses OAuth seller tokens plus an `application_fee`. Anticipation is a first-class resource at Pagar.me and Asaas and has no equivalent in Stripe's Brazilian offering.

### Cited Findings

**Pagar.me v5 (Stone group)**
- Pagar.me separates Order (pedido) from Charge (cobrança). An Order bundles charges, items, shipping, antifraud, multiple payment methods, multiple buyers and split. A Charge represents one payment. `chargedback` exists only on the Charge, is terminal, and does not change Order status. An order created with `closed: false` stays open, and does not become `paid` until it is closed, even if all its charges are paid — [Pagar.me Pedidos](https://docs.pagar.me/reference/pedidos-1); [Chargeback status](https://docs.pagar.me/page/chargeback-novo-status-na-cobran%C3%A7a); [Cobranças](https://docs.pagar.me/reference/cobran%C3%A7as-1)
- A split rule has `amount`, `type` (`flat`|`percentage`), `recipient_id` (`rp_XXXXXXXXXXXXXXXX`) and `options` `{liable, charge_processing_fee, charge_remainder_fee}`. An order can have N recipients and N rules. At least one recipient must be responsible for chargebacks, fees and the remainder. Split is available only to PSP clients, not gateway clients — [Pagar.me split](https://docs.pagar.me/reference/split-1)
- A recipient carries `transfer_settings {transfer_enabled, transfer_interval (e.g. Daily), transfer_day}` and `automatic_anticipation_settings {enabled, type (e.g. full), volume_percentage, delay}` — [Automatic anticipation settings](https://docs.pagar.me/reference/informa%C3%A7%C3%B5es-de-antecipa%C3%A7%C3%A3o-autom%C3%A1tica-1); [v5 recipient contract changes](https://docs.pagar.me/page/novas-regras-para-cria%C3%A7%C3%A3o-de-sellers-de-marketplace-c-v5)
- The contract for creating marketplace sellers (recipients) changed in v5 as of 30 Nov 2024 (a KYC-related contract) — [Pagar.me v5 recipient changes](https://docs.pagar.me/page/novas-regras-para-cria%C3%A7%C3%A3o-de-sellers-de-marketplace-c-v5)
- Receivables (payables/recebíveis) have their own resource and status — [Obter recebíveis](https://docs.pagar.me/reference/retornando-receb%C3%ADveis)

**Asaas**
- A Cobrança (payment) carries a `split[]` array of `{walletId, fixedValue | percentualValue (up to 4 decimals) | totalFixedValue (for installments)}`. The split is computed on `netValue` (after Asaas fees) when the payment is received. Fixed and percentage rules can be combined, but their total cannot exceed the net value. The creator's own walletId must not be included — [Asaas split](https://docs.asaas.com/docs/split-de-pagamentos)
- Split status: `PENDING` → `AWAITING_CREDIT` → `DONE` | `CANCELLED` | `REFUSED` | `REFUNDED`. Webhooks are `PAYMENT_SPLIT_DONE`, one per split, and `PAYMENT_SPLIT_DIVERGENCE_BLOCK`, which fires when splits exceed the available value and gives 2 business days to fix it — [Asaas split](https://docs.asaas.com/docs/split-de-pagamentos)
- Creating a subconta returns its `apiKey` (shown only once) and its `walletId`. The walletId is used for splits and internal transfers. Sub-account API keys can be created and rotated through the API. New customers creating subcontas through the API go through a mandatory regulatory evaluation period with limits — [Criar subconta](https://docs.asaas.com/reference/criar-subconta); [Criação de subcontas](https://docs.asaas.com/docs/criacao-de-subcontas); [Chaves de subcontas](https://docs.asaas.com/docs/gerenciamento-de-chaves-de-api-de-subcontas)
- Anticipation endpoints are `POST /v3/anticipations/simulate`, then `POST /v3/anticipations` with `payment` or `installment` (plus `documents` when the simulation says they are required), and `GET /v3/anticipations/{id}`. Statuses include `PENDING`, `DENIED`, `CREDITED` and `CANCELLED`. Webhooks are `RECEIVABLE_ANTICIPATION_CREDITED`, `_DENIED` and `_CANCELLED` — [Asaas antecipações](https://docs.asaas.com/docs/antecipacoes)

**Mercado Pago**
- Split 1:1 (marketplace) uses the normal checkout flow with a per-seller access token obtained through OAuth. The integrator's `public_key` goes on the front end and the seller's `access_token` on the back end. With Checkout Pro you set `marketplace_fee`. With Checkout API you call `POST https://api.mercadopago.com/v1/payments` with `application_fee`. Mercado Pago's own fee is deducted from the seller's amount first, then the marketplace commission is taken from what remains — [MP Split 1:1 integrate marketplace](https://www.mercadopago.com.br/developers/pt/docs/split-payments/split-1-1/integration-configuration/integrate-marketplace); [MP Checkout Pro marketplace](https://www.mercadopago.com.br/developers/en/docs/checkout-pro/how-tos/integrate-marketplace)
- The payment is created on, and settles to, the seller's account. The platform does not hold the funds — [MP checkout API marketplace](https://www.mercadopago.com.br/developers/en/docs/checkout-api-payments/how-tos/integrate-marketplace.md)

**Iugu**
- The Invoice (fatura) is the central object. `splits[]` holds `{recipient_account_id, cents (fixed), percent}`, and the rules can vary by method (card, boleto, Pix) and by installment. Splits work only between Iugu accounts (subcontas). The dashboard (Alia) allows one recipient, while the API allows unlimited recipients. Docs warn that if the splits total 100% of the invoice value, the split is not processed and everything goes to the creator — [Iugu split](https://dev.iugu.com/docs/split-de-pagamentos)

**PagBank (PagSeguro)**
- The Orders API carries `splits {method: FIXED|PERCENTAGE, receivers[] {account, amount}}`. The split can cover the net value and also fees, tariffs and installment rates. Documented flows include "create then pay an order with split" and "pre-authorise then partially capture an order with split" — [PagBank Order object](https://developer.pagbank.com.br/reference/objeto-order); [Config split](https://developer.pagbank.com.br/docs/config-split); [Pre-auth partial capture with split](https://developer.pagbank.com.br/reference/pre-autorizar-e-capturar-parcialmente-um-pedido-com-divisao-do-pagamento)

**Efí (ex-Gerencianet) Pix API**
- Efí follows the Banco Central Pix API model: immediate charges (`cob`) keyed by `txid`, due charges (`cobv`), payload locations, and Pix Automático endpoints, plus Efí-exclusive endpoints. A webhook is registered per Pix key with `PUT /v2/webhook/:chave`, and callbacks go to `POST {url}/pix`. Callbacks are protected by mTLS, which can be skipped with the header `x-skip-mtls-checking: true`, but then the receiver must verify origin some other way. In sandbox, `cob`/`cobv` charges of R$0.01–10.00 are auto-confirmed and trigger webhooks, and charges above R$10 stay active — [Efí webhooks](https://dev.efipay.com.br/en/docs/api-pix/webhooks/); [Efí community on mTLS](https://comunidade.sejaefi.com.br/discussao/configuracao-webhook-chave-pix-mtls); [Due charges](https://dev.efipay.com.br/en/docs/api-pix/cobrancas-com-vencimento/); [Payload locations](https://dev.efipay.com.br/en/docs/api-pix/payload-locations/); [Pix Automático](https://dev.efipay.com.br/en/docs/api-pix/pix-automatico/); [Efí exclusive endpoints](https://dev.efipay.com.br/en/docs/api-pix/endpoints-exclusivos-efi/)

### Inferences
- The common Brazilian model is: Recipient/SubAccount (KYC, bank account, `transfer_settings`, `anticipation_settings`) → Payment/Charge with `split[]` of `{recipient, flat|percentage, liable, pays_fee, gets_remainder}` → Receivable (payable) per recipient per installment with `due_at` → Anticipation (simulate, then request, with a fee) → Transfer/Payout. Jupiter should model receivables (recebíveis) and anticipation explicitly, because neither Stripe nor Adyen has an equivalent.
- For split fee and liability semantics, Pagar.me's explicit `liable`/`charge_processing_fee`/`charge_remainder_fee` flags, with the rule that at least one recipient holds each, are a clear and testable invariant. Asaas's "split on net value" is simpler but hides fee allocation. Adyen's typed split lines (`Commission`, `PaymentFee`, `Remainder`) are the most ledger-friendly design and map directly to double-entry postings.
- The Efí/BCB Pix API (`txid`, `cob`/`cobv`, `e2eid`, webhook per key over mTLS) is the regulatory shape underneath. Jupiter should hide it behind a PaymentIntent, and expose `txid`/`end_to_end_id` in `payment_method_details.pix` for reconciliation.

### Gaps
- Stone/Openbank: I found no public API documentation this session and did not search it separately. Stone's merchant acquiring API is mostly reached through Pagar.me.
- Mercado Pago's newer Orders API (`/v1/orders`) and the 1:N "advanced payments" (disbursements) model could not be fetched: MP's docs returned 404 for several URLs. Details such as `money_release_days` and disbursement fields remain unverified.
- Full Pagar.me v5 status enums for Order and Charge (for example `pending`, `paid`, `failed`, `canceled`, `processing`, `chargedback`) were seen only as search snippets, so they need checking. The Pagar.me anticipation endpoint (`/recipients/{id}/anticipations`) and transfer endpoints returned 404 at the URL tried.
- Iugu's withdrawal (saque) and advance (antecipação) resources, and how it assigns fee responsibility in splits, were not covered by the page fetched.
- Asaas's complete cobrança status list and its automatic anticipation configuration were not fetched.

## 4. What is worth copying or avoiding, and what the design writeups say

### Takeaway
The patterns that recur across leading PSPs, and that Jupiter should adopt:
- one intent-style payment object with an explicit state machine
- every money movement as an immutable ledger entry with gross, fee and net plus `available_on`
- client idempotency keys backed by a server-side key table
- date-based API versions with response transformers
- cursor or token pagination
- signed webhooks with a timestamp and dedupe by event ID
- typed errors that carry a request log URL or ID

Things to avoid:
- modelling split through side effects that hide fees
- order and charge status that can silently diverge
- permanently caching 5xx responses under an idempotency key (Stripe's own v2 moved away from this)

### Cited Findings
- Stripe changed idempotency in v2 from "replay the saved response even if it was an error" to "retry the failed request without side effects", with a 30-day window. This is Stripe's own correction of the v1 design — [API v2 overview](https://docs.stripe.com/api-v2-overview); [v1 idempotency](https://docs.stripe.com/api/idempotent_requests)
- Stripe moved from continuous date versions (2017) to monthly non-breaking versions plus two breaking releases a year (2024 onward). This reduces how many breaking points an integrator has to track — [API versioning blog](https://stripe.com/blog/api-versioning); [Versioning docs](https://docs.stripe.com/api/versioning)
- Thin events (ID plus type, then fetch the latest) avoid versioned snapshot payloads and stale-state races. Stripe uses them for all of v2 — [Webhooks](https://docs.stripe.com/webhooks); [API v2 overview](https://docs.stripe.com/api-v2-overview)
- Adyen gives every modification its own reference and reports the outcome by webhook, which keeps async rails honest — [Adyen capture](https://docs.adyen.com/online-payments/capture/)
- Pagar.me lets Order `paid` and Charge `chargedback` diverge by design. The chargeback does not propagate to the order — [Pagar.me chargeback status](https://docs.pagar.me/page/chargeback-novo-status-na-cobran%C3%A7a)
- Iugu's rule that a 100% split is not processed is a surprising edge case, and documenting it is necessary — [Iugu split](https://dev.iugu.com/docs/split-de-pagamentos)
- Asaas returns an API key only once and has a split-divergence block with a 2-day correction window. Both are good DX and safety patterns to copy — [Asaas subcontas](https://docs.asaas.com/docs/criacao-de-subcontas); [Asaas split](https://docs.asaas.com/docs/split-de-pagamentos)

### Inferences
Suggested Jupiter conventions, synthesised from the findings above:
- Resource set: `payment_intents` (Pix, boleto, card, and Pix Automático via `mandates`), `payment_methods`, `customers`, `refunds` (async status), `disputes` (card chargebacks plus Pix MED), `recipients` (KYC plus `transfer_settings` and `anticipation_settings`), `splits` inline on the intent as typed lines (`recipient`, `commission`, `fee`, `remainder`) with `liable` and `fee_bearer` flags, `receivables`, `anticipations` (simulate, then create), `transfers`, `payouts`, `balance_transactions` (immutable ledger with gross, fee, net, `available_on` and `reporting_category`), `events` and `webhook_endpoints`.
- Conventions:
  - prefixed IDs (`pi_`, `re_`, `rcp_` and so on)
  - `livemode` and separate test/live keys and signing secrets
  - JSON bodies
  - `Idempotency-Key` on POST/DELETE, stored in a Brandur-style table with `locked_at`, `recovery_point`, a parameter-hash check (409 or 422 on mismatch) and 24h–30d retention
  - a date-named `Jupiter-Version` header, with the version pinned per account and in-code response transformers
  - opaque `page` token pagination with `next_page_url`
  - `include[]` for opt-in fields
  - metadata limited to 50 keys, 40-character keys and 500-character values
  - a webhook signature header in the form `t=...,v1=HMAC_SHA256(secret, t + "." + body)` with 5-minute tolerance and secret rotation that allows a 24h overlap
  - error envelope `{type, code, decline_code, message, param, doc_url, request_id}`
  - HTTP codes 400/401/402/403/404/409/422/429/5xx

### Gaps
- Stripe's "APIs as infrastructure" essay was not fetched this session. Its URL and exact content could not be verified, so no claims are drawn from it.
- Other Brandur posts, for example on API design, "Stripe-style" IDs or transactionally staged job drains, were not fetched. Only the idempotency-keys post was verified.
