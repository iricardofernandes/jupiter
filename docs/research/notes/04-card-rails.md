# Card Rails: Implementation-Level Specs for Jupiter (Go payments backend), as of 2026

> Scope: ISO 8583 simulator, auth/capture semantics, 3DS2 simulation, tokenization vault + network tokens, PCI DSS v4.0.1 scope, disputes, and risk engineering.
> Research date: 2026-09-29. Where a claim comes from background knowledge and was NOT verified against a fetched source in this session, it is labelled **[unverified]** and kept in Inferences/Gaps, not in Cited Findings.

---

## 1. ISO 8583: versions, MTI, bitmap, data elements, response codes, reversals, ISO 20022 status

### Takeaway
ISO 8583 is still the production card-auth protocol in 2026. ISO 20022 card messages (ATICA) exist, but there is no fixed migration deadline. A realistic simulator should implement 1987-style MTIs (0xxx), primary/secondary bitmaps, a scheme-like subset of DEs, and the request/response vs advice semantics: requests time out and get reversed, while advices are store-and-forward and repeated (0421) until acknowledged. moov-io/iso8583 (Go) is the natural library for this.

### Cited Findings
- ISO 8583 has three parts: the MTI (4 digits encoding version, class, function and origin), one or more bitmaps (primary, secondary, optional tertiary) that flag which data elements are present, and the data elements themselves. — [Wikipedia: ISO 8583](https://en.wikipedia.org/wiki/ISO_8583)
- MTI digit 1 is the version: 0xxx = 1987, 1xxx = 1993, 2xxx = 2003, 8xxx/9xxx = national/private use. Digit 2 = class (authorization, financial, reversal, reconciliation, administrative, network management). Digit 3 = function (request, response, advice…). Digit 4 = origin (acquirer/issuer, repeat). — [Wikipedia: ISO 8583](https://en.wikipedia.org/wiki/ISO_8583)
- Key DEs: 2 PAN, 3 processing code, 4 transaction amount, 7 transmission date/time, 11 STAN, 39 response code, 55 ICC/EMV data. — [Wikipedia: ISO 8583](https://en.wikipedia.org/wiki/ISO_8583)
- Response code examples: "00" approved, "51" insufficient funds, "54" expired card, "55" incorrect PIN. The Wikipedia summary also lists a partial-approval code (the 1993/2003 3-digit style "010"; 1987-based scheme specs commonly use "10" [unverified]). — [Wikipedia: ISO 8583](https://en.wikipedia.org/wiki/ISO_8583)
- Reversal/advice MTIs: 0120 auth advice, 0121 auth advice repeat, 0400 reversal request, 0420 acquirer reversal advice, 0421 reversal advice repeat, 0430 reversal response. — [LinkedIn: ISO 8583 message flow part two](https://www.linkedin.com/pulse/whats-iso-8583-message-flow-part-two-ahmed-el-nazer)
- Timeout rules: if the network times out waiting for the issuer, the switch generates a 0420 to the issuer to cancel the original 0100/0200. If the acquirer gets no 0110/0210 in time, the acquirer sends a 0420 and the network answers with 0430. If a 0420 is not acknowledged, the originator keeps sending 0421 repeats until it gets a response. — [LinkedIn: ISO 8583 message flow](https://www.linkedin.com/pulse/whats-iso-8583-message-flow-part-two-ahmed-el-nazer)
- "Requests are end-to-end messages with time-outs and automatic reversals in place, while advices are point-to-point messages with transmission guaranteed over each link, but not necessarily immediately." — [LinkedIn: ISO 8583 message flow](https://www.linkedin.com/pulse/whats-iso-8583-message-flow-part-two-ahmed-el-nazer)
- A processor's full production spec (useful as a real-world DE/MTI reference): Worldpay ISO 8583 Reference Guide v2.46. — [Worldpay/FIS ISO 8583 Reference Guide (PDF)](https://developerengine.fisglobal.com/assets/pdf/Worldpay_ISO_8583_Reference_Guide_V2.46.pdf)
- A public Go/"mini card network" project shows the same patterns: 0420 reversals end to end, unique RRNs, and a resilient store-and-forward (SAF) queue. — [pcaokhai/mini-card-network PR #88](https://github.com/pcaokhai/mini-card-network/pull/88), [PR #85](https://github.com/pcaokhai/mini-card-network/pull/85)
- **moov-io/iso8583 (Go):**
  - Messages are defined by a `MessageSpec` of `field.Spec`s. Field 0 is the MTI and field 1 the bitmap.
  - Field types: String, Numeric, Binary, Composite (TLV/BER-TLV or positional subfields).
  - Encodings: ASCII, Binary, BCD, LBCD, EBCDIC. Length prefixes: Fixed, L, LL, LLL, LLLL. Padding is configurable.
  - Built-in `Spec87ASCII`. Struct-based marshaling is recommended.
  - The `network` package provides length headers (Binary2Bytes, ASCII4Bytes, BCD2Bytes, VMLH).
  - The companion `moov-io/iso8583-connection` package handles client/server, request/response matching and connection management.
  - A `describe` CLI inspects messages.

  — [github.com/moov-io/iso8583](https://github.com/moov-io/iso8583)
- **ISO 20022 for cards:**
  - ATICA (Acquirer to Issuer Card Messages) covers authorization, verification, clearing, settlement and reporting. It is meant to reproduce ISO 8583 semantics with structured, named fields.
  - Coexistence with 8583 is expected. Tens of thousands of 8583 implementations would need to migrate.
  - Unlike wholesale payments (November 2025 milestone), card migration has **no fixed timeline**.

  — [Tapix: ISO 8583 vs ISO 20022](https://www.tapix.io/resources/post/iso-8583-vs-iso-20022); background: [ISO 20022 ATICA business justification](https://www.iso20022.org/submission-status/771/download), [Cognizant whitepaper](https://www.cognizant.com/nl/en/documents/cards-payment-messaging-standardization-iso20022.pdf)

### Inferences
- **Simulator MTI set:** 0100/0110 (auth, dual-message), 0120/0130 (auth advice, e.g. stand-in notification), 0200/0210 (financial, single-message/debit), 0220 (completion advice), 0400/0410 and 0420/0430 (+0421 repeat), and 0800/0810 (network management).
- **Standard [unverified] DE meanings for the core set:**
  - DE12 local time; DE22 POS entry mode (e.g. 01x manual/keyed, 05x chip, 07x contactless, 81x e-commerce/credential-on-file); DE37 RRN (an12); DE38 auth ID response (an6); DE41 terminal ID (ans8); DE42 merchant/card acceptor ID (ans15); DE49 currency code (n3, ISO 4217, e.g. 986 BRL); DE90 original data elements (used in reversals to point at the original MTI+STAN+date); DE70 network management code in 0800 (001 sign-on, 002 sign-off, 301 echo/heartbeat, 161 key change).
  - Response codes to model: 00, 05 do not honour, 12 invalid transaction, 13 invalid amount, 14 invalid card number, 51, 54, 55, 57 not permitted to cardholder, 61 exceeds withdrawal limit, 62 restricted card, 65 activity count exceeded (often used as an SCA-required soft decline in the EU), 91 issuer unavailable, 96 system malfunction.
- **Stand-in processing (STIP) [unverified]:** when the issuer is unavailable (91 / timeout), the network approves or declines on the issuer's behalf using issuer-set limits. It then sends 0120 advices to the issuer, which the issuer must post. Jupiter's "network" component can implement STIP with per-issuer limits plus a SAF queue of 0120/0420s retried as 0121/0421.
- **Idempotency:**
  - The key for matching a response is roughly (DE7, DE11, DE32/DE33, DE41/42). DE37 RRN is the handle for later clearing and disputes.
  - A late 0110 arriving after the acquirer has already sent 0420 must be ignored by the acquirer, and the issuer must process the reversal. Build this "late response after timeout" race explicitly into tests.
- **Wire format for the portfolio:** moov-io's `Spec87ASCII` plus a 2-byte binary length header is enough. Keep DE55 as a BER-TLV composite (tags 9F26 ARQC, 9F27, 9F10, 9F37, 9F36 ATC, 95 TVR, 9A, 9C, 5F2A, 82, 9F1A, 9F02) so EMV chip data is realistic.

### Gaps
- Visa BASE I / Mastercard CIS specific private fields (Visa F62/F63/F126, Mastercard DE48 subelements, DE61, DE112 installment data) are proprietary and I could not source them publicly. They are mapped only roughly under [unverified].
- I did not find a public authoritative full response-code table (scheme manuals are gated).

---

## 2. Auth/capture semantics: pre-auth, incremental, partial approval, auth expiry, MIT/CIT, stored credentials, recurring, installments (parcelado)

### Takeaway
Since April 2024, Visa's auth validity windows are:
- 5 days for card-present,
- 10 days for CNP customer-initiated,
- 5 days for MITs,
- 30 days for lodging, cruise and car rental estimated auths,
- up to 30 days for other CNP via the paid Extended Authorization Service.

Incremental auths add amount but do not extend Visa's validity. Jupiter should model the auth as a state machine with an `expires_at` derived from scheme × auth-type × MCC.

### Cited Findings
- **Visa auth-to-clearing windows (from April 2024):**
  - Card-present: 5 calendar days.
  - CNP cardholder-initiated: 10 days.
  - MIT (installments, recurring, advance payments, unscheduled credential-on-file): 5 days.
  - Lodging, cruise and vehicle rental with estimated auth: 30 days.
  - Aircraft, bicycle, boat and equipment rental estimated auth: 10 days.
  - Regional exceptions: Europe intraregional 2 days, India domestic 4 days, Malaysia 2 days (AFDs).
  - The optional Visa Extended Authorization Service gives up to 30 days for CNP, at a 0.08% fee on approved auths.

  — [Chargebacks911: Visa authorization rules](https://chargebacks911.com/visa-authorization-rules/); primary: [Visa: Authorization framework will be updated to simplify time frames (PDF)](https://corporate.visa.com/content/dam/VCOM/regional/na/us/support-legal/documents/authorization-framework-will-be-updated-to-simplify-authorization-processing-time-frames.pdf)
- For Visa estimated authorizations the expiry period cannot be extended, unlike Mastercard (and Mada), where an incremental can extend it. Incremental auths do not extend the validity of the initial pre-auth amount on Visa. — [Checkout.com docs: adjust an estimated authorization](https://www.checkout.com/docs/payments/manage-payments/authorize-a-payment/adjust-an-estimated-authorization)
- Adyen implements authorization adjustment (incremental/decremental) as a separate API operation on the original auth reference. — [Adyen: Authorization adjustment](https://docs.adyen.com/online-payments/adjust-authorisation)
- Visa's merchant best practices on estimated/incremental auths and reversals (release unused auth promptly). — [Visa: Authorization and reversal processing best practices (PDF)](https://usa.visa.com/content/dam/VCOM/regional/na/us/support-legal/documents/authorization-and-reversal-processing-best-practices-for-merchants.pdf)
- Explainer on incremental authorizations (hotel/rental use cases). — [Checkout.com blog](https://www.checkout.com/blog/what-are-incremental-authorizations)

### Inferences
- **State machine:** `AUTHORIZED -> (INCREMENTED)* -> PARTIALLY_CAPTURED/CAPTURED -> SETTLED`. Also `AUTHORIZED -> REVERSED` (full or partial 0400) and `AUTHORIZED -> EXPIRED` (auto-release job at `expires_at`). Compute `expires_at` = f(scheme, CP/CNP, CIT/MIT, estimated flag, MCC).
- **Stored Credential Framework [unverified; well-known Visa/Mastercard rules since 2017–2018]:**
  - The first CIT that stores the credential must be flagged and needs the cardholder's consent.
  - Later MITs (recurring, installment, unscheduled COF, plus industry-practice types: incremental, resubmission, delayed charge, no-show, reauthorization) must carry the original transaction's network ID: Visa Transaction ID (F62.2) / Mastercard Trace ID (DE63/DE15). This chaining is what gets MITs out of SCA scope and improves approval rates.
  - Jupiter should store `network_transaction_id` from the first CIT response and send it on every MIT.
- **Partial approval [unverified]:** needs a merchant capability indicator in the request; the issuer answers with RC 10 and a lower approved amount. The merchant must then collect the remainder by another tender or reverse.
- **Brazil parcelado [unverified]:**
  - *Parcelado lojista* (merchant-financed, no interest; merchant receives each installment monthly or anticipates receivables) vs *parcelado emissor* (issuer-financed, with interest to the cardholder).
  - The auth goes through once for the full amount, with installment count/type carried in scheme/local fields (e.g. Mastercard DE112 / Visa and Elo domestic fields).
  - Issuers bill the cardholder monthly. Acquirers settle to the merchant per installment (D+30, D+60…), which is why *antecipação de recebíveis* and the registradoras (CERC, CIP/Núclea, TAG) exist.
  - For Jupiter: model `installments{count, type: MERCHANT|ISSUER}` on the auth, plus a settlement schedule generator producing N receivables.

### Gaps
- Mastercard's exact auth validity table (commonly cited: 7 days for final/undefined, 30 days for pre-auth) was not verified in this session.
- There is no public primary source for the exact ISO fields carrying Brazilian installment data (ABECS/Elo specs are gated).

---

## 3. 3DS2 (EMVCo): actors, flows, ECI/CAVV, liability shift, exemptions, data-only

### Takeaway
Visa and major schemes sunset EMV 3DS 2.1.0 in September 2024. The 2026 baseline is 2.2.0, with 2.3.1 (August 2022) the latest spec. A Jupiter simulation should implement:
- 3DS Server → DS → ACS routing: AReq/ARes, then CReq/CRes for a challenge, then RReq/RRes as the result callback.
- A frictionless vs challenge decision in the ACS.
- An authentication value (CAVV/AAV) plus ECI, carried into the ISO 8583 auth.

### Cited Findings
- EMV 3DS 2.1.0 is no longer supported as of September 25, 2024. Merchants must support 2.2.0. — [Cybersource: EMV 3DS 2.1.0 support ending](https://developer.cybersource.com/docs/cybs/en-us/mandates/relnote/all/na/mandates-april-2024/mandates-payments-intro-april-2024/3ds-2-1-0-sunset.html); [Visa Acceptance KA-04311](https://support.visaacceptance.com/knowledgebase/knowledgearticle/?code=KA-04311)
- Visa mandated issuers to use 3DS 2.2 from 14 September 2024. — [Netcetera: 3DS 2.1 decommission](https://www.netcetera.com/stories/news/3DS-2.1-decommission.html)
- EMV 3DS 2.3.1 is the latest version (released August 2022). It adds new data elements and channels/devices. — [GPayments: 3DS 2.3 enhancements](https://www.gpayments.com/blog/article/evolving-security-an-overview-of-3d-secure-2-3-enhancements/)
- Regional mandates continue, e.g. the Japan Credit Association EMV 3DS mandate for all e-commerce merchants (effective end of March 2025). — [MRC: JCA EMV 3DS mandate](https://merchantriskcouncil.org/learning/resource-center/member-news/blog/2025/mrc-japan-credit-association-emv-3ds-mandate)

### Inferences
**Protocol and actors [unverified detail; standard EMVCo architecture]:**
- 3DS Requestor/3DS Server (merchant/PSP side) sends AReq → Directory Server (scheme: Visa Secure, Mastercard Identity Check, Elo) → ACS (issuer).
- ACS returns ARes with transStatus:
  - Y (authenticated, frictionless)
  - A (attempted)
  - N (not authenticated)
  - U (unavailable)
  - R (rejected)
  - C (challenge required)
  - D (decoupled)
  - I (informational only / data-only)
- Challenge: CReq/CRes between browser/SDK and ACS. The ACS then sends RReq to the 3DS Server via the DS with the final result.
- Pre-step: the 3DS Method (hidden iframe device fingerprint) and PReq/PRes for card range/version caching.

**ECI [unverified]:**
- Visa/Amex/Discover: 05 fully authenticated, 06 attempted, 07 non-authenticated.
- Mastercard: 02 fully authenticated, 01 attempted, 00 not authenticated; 06/07 for data-only / TRA-exempted in newer programs.
- CAVV (Visa) / AAV (Mastercard UCAF) is a 20-byte cryptogram, usually base64, carried in the auth (Visa F126.9; Mastercard DE48 SE43 [unverified]).

**Liability shift [unverified]:**
- Authenticated (Y) or attempted (A) moves fraud-chargeback liability to the issuer. Visa 10.4 / Mastercard 4837 fraud disputes are then blocked.
- Exemptions (TRA, low value <€30, MIT, whitelisting) or data-only flows generally leave liability with the merchant.

**Simulator design:**
- The ACS mock uses a rule/score function (amount, device, card history) to return Y/C/N.
- Generate a CAVV as HMAC(issuer key, PAN‖amount‖timestamp‖ATN).
- Jupiter's issuer simulator verifies the CAVV on the 0100 and maps the result into approval and liability metadata.

### Gaps
- EMVCo spec PDFs were not fetched this session. The message field lists (e.g. threeDSServerTransID, acsTransID, dsTransID, messageVersion) come from background knowledge.
- The exact ECI codes for Mastercard data-only (Identity Check Insights) need verification against current Mastercard docs.

---

## 4. Network tokenization (VTS, MDES), PAR, cryptograms, PSP tokens vs network tokens, account updater

### Takeaway
Network tokens (Visa Token Service, Mastercard MDES) are now mainstream:
- VTS has more than 12.6B tokens issued, and about 50% of Visa digital transactions are tokenized.
- MDES covers 30–40% of Mastercard transactions.
- Mastercard has committed to 100% European e-commerce tokenization, with no manual PAN entry, by 2030.

Jupiter should run two layers: its own PSP vault token (PAN → opaque token, used internally), plus optional network token provisioning, where the token and a per-transaction cryptogram replace the PAN in the auth.

### Cited Findings
- Visa Token Service crossed 12.6 billion tokens issued, with 50% of digital transactions tokenized. — [Glenbrook: tokenization 2025 update](https://glenbrook.com/payments_views/we-really-cant-stop-talking-about-tokenization-a-2025-update/) (via search summary)
- MDES tokenizes more than 30% of Mastercard transactions worldwide, estimated at 35–40% by late 2025. Mastercard committed to 100% European e-commerce tokenization by 2030 and to phasing out manual card entry in Europe. — [PYMNTS: Mastercard prepping for 2030 e-commerce tokenization](https://www.pymnts.com/mastercard/2024/mastercard-new-use-cases-prep-for-total-ecommerce-tokenization-by-2030/); [Payments Dive: Visa, Mastercard push more tokenization](https://www.paymentsdive.com/news/visa-mastercard-push-more-tokenization/759762/)
- Comparison of Visa, Mastercard and Apple token designs, and gateway tokens vs network tokens. — [Spark research](https://www.spark.money/research/payment-network-tokenization-card-comparison); [Inyo: network vs gateway tokens](https://www.inyoglobal.com/news/payment-tokenization-guide); [Solidgate: card scheme tokenization](https://docs.solidgate.com/payments/card-payments/card-payments-insights/card-schemes-tokenization/)
- The tokenized-transaction forecast (283B in 2025 → 574B by 2029) is an industry projection, not a confirmed figure. — [Payments Dive](https://www.paymentsdive.com/news/visa-mastercard-push-more-tokenization/759762/) (via search summary)

### Inferences
- **Network token flow [unverified detail]:**
  1. The Token Requestor (PSP with a TRID) calls VTS/MDES provision with PAN, expiry and CVV/3DS.
  2. The network returns a token (a 16-digit, BIN-like DPAN from a token BIN range), token expiry, and the PAR (Payment Account Reference, 29 chars, stable across all tokens and the PAN for the same account).
  3. For each transaction, the PSP requests a cryptogram (TAVV for Visa, DSRP/UCAF for Mastercard) and sends token + cryptogram + ECI in the auth.
  4. The network de-tokenizes before the issuer.
  5. Lifecycle webhooks (token updated/suspended/deleted) replace most account-updater traffic: the token survives card reissue.
- **PSP vault tokens vs network tokens:**
  - Vault tokens reduce the merchant's PCI scope but are useless outside the PSP.
  - Network tokens are portable across PSPs only if the merchant owns the TRID, and they raise approval rates.
  - PAR lets Jupiter link a token to a PAN for fraud velocity and loyalty without handling the PAN.
- **Account updater (Visa Account Updater / Mastercard Automatic Billing Updater) [unverified]:** batch or real-time lookup returns new PAN/expiry or a "closed account" status. Jupiter can simulate it with a nightly job over non-tokenized stored credentials.
- **Vault design for Jupiter:**
  - Store the PAN encrypted with a per-record DEK (AES-256-GCM), with the DEK wrapped by a KEK in KMS/HSM (envelope encryption).
  - Store the token as a random (non-derivable) ID.
  - Keep a separate keyed HMAC(PAN) fingerprint for dedup/velocity.
  - Keep BIN8 + last4 in cleartext for display and routing.
  - Put the vault in its own network segment/service with mTLS and a minimal API: tokenize, detokenize-to-acquirer-only, delete.

### Gaps
- There is no public primary Visa/Mastercard developer spec for token provisioning APIs (VTS/MDES developer portals need onboarding). PAR format details are from background knowledge.
- The Brazilian network-token status (Elo tokenization, bank-level adoption) was not researched.

---

## 5. PCI DSS v4.0.1: scope, SAQs, scope reduction, keys, PAN storage, truncation, 8-digit BINs

### Takeaway
PCI DSS v4.0.1 has been in force since v4.0 retired at end-2024, and the future-dated requirements became mandatory on 31 March 2025. For iframe/hosted-fields merchants, the January 2025 SAQ A revision removed payment-page script requirements 6.4.3/11.6.1 but added an eligibility criterion that the site is "not susceptible to attacks from scripts". Truncation may show at most first 8 + any other 4 for 16-digit PANs (per the PCI FAQ 1091 formats), but first 6 / last 4 is the only format every brand accepts.

### Cited Findings
- SAQ A (January 2025) removed requirements 6.4.3 and 11.6.1 (payment-page script security) and 12.3.1 (the TRA supporting 11.6.1). It added the eligibility criterion that the merchant "confirm their site is not susceptible to attacks from scripts that could affect the merchant's e-commerce system(s)." The October 2024 SAQ A retired and the January 2025 version took effect on 31 March 2025. The same date made 6.4.3, 11.6.1 and 12.3.1 effective in v4.0.1. — [PCI SSC blog](https://blog.pcisecuritystandards.org/important-updates-announced-for-merchants-validating-to-self-assessment-questionnaire-a)
- Those requirements still apply in SAQ A-EP, SAQ D (merchant and service provider) and the ROC. Merchants who cannot meet the new SAQ A eligibility criterion must use SAQ A-EP. — [SecurityMetrics](https://www.securitymetrics.com/blog/big-changes-for-saq-a); [TrustedSec: the hidden trap](https://trustedsec.com/blog/the-hidden-trap-in-the-pci-dss-saq-a-changes); [Adyen: SAQ A eligibility](https://docs.adyen.com/development-resources/pci-dss-compliance-guide/saq-a-eligibility)
- PCI FAQ 1331 was revised (2026), narrowing the path to mark 6.4.3/11.6.1 "N/A". This is a secondary vendor source and should be verified against the PCI FAQ itself. — [Reflectiz: FAQ 1331 revision 2026](https://www.reflectiz.com/blog/pci-faq-1331-revision-2026/); related FAQ 1588 discussion: [HUMAN Security](https://www.humansecurity.com/learn/blog/pci-dss-4-update-unpacking-the-changes-to-saq-a/)
- Requirement 3.3/3.4 masking: at most first 6 and last 4 displayed without a business need. FAQ #1091 lists the brand truncation formats, including "first 8, any other 4" for 16-digit PANs (8-digit BIN formats have been in the FAQ since 2017). First 6 / last 4 remains the only format accepted by all brands. These are maximums, to use only with a business need. — [PCI SSC: 8-digit BINs and PCI DSS](https://blog.pcisecuritystandards.org/8-digit-bins-and-pci-dss-what-you-need-to-know); [SRC GmbH](https://src-gmbh.de/en/blog/8-digit-bins-and-pci-dss/); [Visa 8-digit BIN PCI position paper](https://usa.visa.com/content/dam/VCOM/regional/na/us/partner-with-us/documents/8-digit-bin-pci-security-requirements-visa-position-paper.pdf)
- A risk to watch: if truncated forms (e.g. first-8/last-4) and hashes of the same PAN are both stored, they can be correlated to rebuild the PAN. PCI requires controls so that truncated and hashed versions cannot be correlated. — [Control Gap: 8-digit BIN ranges and PCI truncation](https://www.controlgap.com/blog/new-bin-ranges-and-pci-truncation); [Sikich](https://www.sikich.com/insight/how-the-eight-digit-bin-mandate-impacts-pan-pci-dss-compliance/)

### Inferences
- **Scope picture for Jupiter [unverified details of v4.0.1 numbering]:**
  - A merchant using Jupiter hosted fields (cross-origin iframe) → SAQ A, if script-attack eligibility is met.
  - Merchant JS that posts the PAN directly to Jupiter → SAQ A-EP.
  - Merchant that touches the PAN server-side → SAQ D.
  - Jupiter itself, as a service provider storing PANs, is a Level 1/2 service provider (SAQ D-SP or ROC).
- **Key requirements for the vault:**
  - 3.5.1: render PAN unreadable using strong crypto, keyed hashes, truncation or tokens.
  - 3.5.1.1: hashes must be keyed (HMAC), and v4.0 made this explicit.
  - 3.5.1.2: disk-level encryption alone is not enough for non-removable media.
  - 3.6/3.7: key management (split knowledge/dual control, rotation/cryptoperiod, KEK stronger than DEK).
  - 3.2.1: SAD (CVV2, full track, PIN block) must never be stored after authorization. Hold CVV only in memory until the auth completes.
- **HSM vs KMS:** a cloud KMS (AWS KMS/GCP Cloud KMS, FIPS 140-2/3 L3 HSM-backed) is acceptable for envelope encryption. Payment HSMs (Thales payShield, AWS Payment Cryptography) are needed for PIN translation, CVV/ARQC verification and DUKPT. For a portfolio, simulate an issuer HSM interface (VerifyARQC, GenerateCAVV) behind a Go interface.
- **BIN table:** use an 8-digit BIN lookup (brand, issuer, country, product, debit/credit/prepaid) as a pure-data service. It is not PAN, so it is out of scope.

### Gaps
- The full PCI DSS v4.0.1 PDF was not fetched. Requirement numbers above (3.5.1.x, 3.6, 3.7, 3.2.1) are from background knowledge [unverified].
- The FAQ 1331 2026 revision is reported only by a vendor blog. Verify it on the PCI SSC FAQ site.

---

## 6. Disputes/chargebacks: lifecycle, reason codes, deadlines, fees, fraud reporting, RDR, Brazil

### Takeaway
The lifecycle is:
1. (Retrieval request / pre-dispute alerts)
2. Chargeback
3. Representment/second presentment
4. Pre-arbitration
5. Arbitration

Mastercard gives about 120 days to file, 45 days for the merchant to represent, and 30 + 30 days for pre-arbitration. Visa (VCR) uses four categories:
- 10.x fraud
- 11.x authorization
- 12.x processing errors
- 13.x consumer disputes

Compelling Evidence 3.0 lets merchants defeat 10.4 fraud claims using two prior undisputed transactions older than 120 days.

Since June 2025, Visa's VAMP ratio counts fraud reports (TC40) plus disputes (TC15) over settled CNP transactions:
- ≥50 bps: acquirer "Above Standard"
- ≥70 bps: acquirer "Excessive"
- merchant excessive: 220 bps, dropping to 150 bps in AP/CA/EU/US from 1 April 2026; LAC is already at 150 bps

Disputes resolved via pre-dispute tools (RDR/CDRN-type) and CE3.0-qualified fraud are excluded.

In Brazil, BCB Resolution 522/2025 caps participant liability at 180 days after authorization and makes the card schemes directly guarantee settlement to merchants.

### Cited Findings
- **Visa VAMP (official fact sheet):**
  - VAMP Ratio = count of [Fraud (TC40) + Disputes (TC15)] ÷ count of settled transactions (TC05), CNP only, effective 1 June 2025.
  - Excludes disputes resolved through pre-dispute solutions and TC40s qualified for CE3.0.
  - Acquirer thresholds: Above Standard ≥50 bps, Excessive ≥70 bps.
  - Merchant Excessive thresholds: AP/Canada/EU/US ≥220 bps with ≥1,500 monthly fraud+disputes, reduced to ≥150 bps on 1 April 2026; LAC ≥150 bps with ≥1,500; CEMEA ≥220 bps with ≥150 count and ≥USD 75k.
  - Enumeration ratio = enumerated auths (approved+declined) ÷ all auths, ≥2,000 bps (20%), together with ≥300,000 enumerated transactions.
  - Advisory period ended 30 September 2025.
  - **Programs for Brazil, Chile and India "will be announced later."**

  — [Visa VAMP fact sheet 2025 (PDF)](https://corporate.visa.com/content/dam/VCOM/corporate/visa-perspectives/security-and-trust/documents/visa-acquirer-monitoring-program-fact-sheet-2025.pdf); [Visa: Introducing VAMP](https://corporate.visa.com/en/sites/visa-perspectives/security-trust/introducing-visa-acquirer-monitoring-program.html)
- Secondary sources say an "early warning" band (0.4–0.5%) was later added for acquirers. This is not in the fact sheet above, so treat it as unverified. — [Chargeflow: VAMP 2026](https://www.chargeflow.io/blog/vamp-visa-acquirer-monitoring-program)
- **CE3.0** (effective April 2023) applies only to Visa 10.4 "Other Fraud – Card-Absent Environment". It needs at least two prior transactions on the same card that settled 120+ days before the dispute and were undisputed, with matching data elements (IP/device ID/shipping address/account). It can be submitted pre-dispute via Order Insight or post-dispute via VROL. Order Insight auto-identifies up to five prior transactions. — [Checkout.com: Visa CE3.0](https://www.checkout.com/blog/visa-compelling-evidence-3-0); [Visa CE3.0 merchant readiness (PDF)](https://usa.visa.com/content/dam/VCOM/regional/na/us/support-legal/documents/compelling-evidence-3.0-merchant-readiness-mar2023.pdf)
- **Pre-dispute tools:** Order Insight shares merchant order data with the issuer at the moment of the cardholder's inquiry. Rapid Dispute Resolution (RDR) auto-refunds, based on rules, at the pre-dispute stage to avoid a chargeback. — [Verifi: CE3.0 in pre-dispute](https://www.verifi.com/in-the-news/compelling-evidence-3-0-ce3-0-in-the-pre-dispute-and-pre-arbitration-environments.html); [chargeback.io: VCR](https://www.chargeback.io/blog/what-is-visa-claims-resolution)
- **Mastercard timeframes:**
  - Cardholder/issuer has about 120 days to file most chargebacks.
  - Merchant/acquirer has 45 days for second presentment.
  - Issuer has 30 days after representment to open pre-arbitration, and the merchant has 30 days to accept or reject.
  - Pre-compliance/compliance filing was reduced to 120 days (from 180).

  — [Chargebacks911: Mastercard time limits](https://chargebacks911.com/chargeback-rules/chargeback-time-limits/mastercard-chargeback-time-limit/); [chargeback.io: Mastercard guide](https://www.chargeback.io/blog/mastercard-chargeback-guide)
- **Brazil, BCB Resolution 522 (10 Nov 2025):**
  - Amends 2021 arrangement risk rules (Res. BCB 150/2021).
  - Participant financial liability is limited to 180 days after authorization. Beyond that, the scheme (instituidor do arranjo) bears it if its rules allow.
  - Schemes (Visa, Mastercard, Elo) must directly guarantee payment to merchants (recebedores) even if issuers or acquirers fail.
  - Institutions have 180 days to adapt and file for authorization.

  — [Agência Brasil](https://agenciabrasil.ebc.com.br/economia/noticia/2025-11/bc-bandeira-de-cartao-tera-de-pagar-transacoes-em-caso-de-falhas)
- The earlier BCB consultation (CP 104, 2024) proposed a 120-day cutoff. Nubank, Cielo and Abipag opposed it (airlines and deferred delivery), Abecs asked for delivery-date data, and Febraban wanted uniform rules for insolvency cases. The final rule set 180 days. — [Finsiders Brasil](https://finsidersbrasil.com.br/reportagem-exclusiva-fintechs/novas-regras-de-chargebacks-de-cartoes-de-credito-dividem-bancos-e-bandeiras/)
- **Conflict:** a consumer-facing Brazilian source says cardholders can contest within 180 days, but "até 90 dias" for the administrative dispute depending on the scheme. This is inconsistent and low quality, so do not rely on it. — [meutudo.com.br](https://meutudo.com.br/blog/noticias/2025/11/11/bandeiras-de-cartao-devem-cobrir-falhas-em-transacoes-diz-banco-central/)

### Inferences
- **Dispute state machine for Jupiter:** `INQUIRY(retrieval / Order Insight) -> [RDR auto-accept] -> CHARGEBACK_OPEN(reason_code, due_at) -> {ACCEPTED | REPRESENTED(evidence)} -> PRE_ARB_OPEN -> {PRE_ARB_ACCEPTED | PRE_ARB_DECLINED} -> ARBITRATION -> {WON | LOST}`.
  - Each transition carries a deadline computed from the scheme table: Mastercard 120/45/30/30.
  - For Visa VCR [unverified]: 120 days for most codes; 30 days to respond to a dispute; 30 days for pre-arbitration response.
  - Ledger impact: debit merchant plus fee at CHARGEBACK_OPEN, credit on WON.
- **Reason code taxonomy to seed [unverified]:**
  - Visa: 10.1–10.5 fraud (10.4 CNP fraud), 11.1–11.3 authorization, 12.1–12.7 processing, 13.1 not received, 13.2 cancelled recurring, 13.3 not as described, 13.5 misrepresentation, 13.6 credit not processed, 13.7 cancelled merchandise.
  - Mastercard: 4837 no cardholder authorization, 4853 cardholder dispute, 4834 point-of-interaction error, 4808 authorization-related, 4863 cardholder does not recognize, 4870/4871 chip liability.
- **Fraud reporting:** TC40 (Visa) / SAFE, now FRAUD in Mastercom (Mastercard), are issuer fraud reports independent of chargebacks. They count toward VAMP even without a chargeback, so Jupiter's risk data model should ingest them as labels separate from disputes.
- **Brazil:** because of parcelado, a chargeback on a 10x installment purchase can hit receivables that were already anticipated or registered at the registradoras. Model chargeback debits against the future receivable schedule, not only the balance.

### Gaps
- Chargeback fee amounts (acquirer fees of about USD 15–100, scheme pre-arb/arbitration filing fees) were not verified from primary sources.
- The Visa/Elo detailed timeframe tables for Brazil domestic transactions and the Elo reason codes are not public.
- The official Visa Core Rules (VCR timeframes) were not fetched.

---

## 7. Risk/fraud engineering: velocity, device fingerprinting, rules vs ML, 3DS decisioning

### Takeaway
The production pattern (Stripe Radar) is a synchronous ML score in the auth path (<100 ms) over 1,000+ features, many of them network-wide velocity and graph signals. User-editable rules sit on top and can reference the ML score and issuer CVC/AVS responses. Labels come from disputes and fraud reports, which arrive with a delay. Jupiter should implement: feature extraction → score → rules engine (allow/block/review/3DS) → decision, with a label pipeline fed by TC40-like and chargeback events.

### Cited Findings
- **Stripe Radar:**
  - Decisions in under 100 ms; about 0.1% false-positive rate on legitimate payments.
  - Evolved from logistic regression → Wide & Deep (XGBoost + DNN) → a pure DNN in mid-2022, inspired by ResNeXt, with training time down more than 85% to under 2 hours. XGBoost was dropped for poor parallelizability and incompatibility with embeddings and transfer learning.
  - Uses more than 1,000 characteristics. Examples: payment velocity, name–email match, cards previously seen per IP, throwaway-email patterns, billing–shipping correlation.
  - Base fraud rate is about 1 in 1,000 payments. Labels come from disputes and fraud investigations; 10× more data brought significant gains.
  - Explainability via Risk Insights (feature contributions, related-transaction clustering via Elasticsearch).

  — [Stripe: How we built it — Stripe Radar](https://stripe.dev/blog/how-we-built-it-stripe-radar)
- Since 27 March 2025 Stripe lets Radar rules combine ML model outputs with the issuer's real-time CVC and postal-code check responses. — [Stripe blog: dynamic risk-based Radar rules](https://stripe.com/blog/using-ai-dynamic-radar-rules)
- Stripe's ML fraud primer (precision/recall trade-offs, label delay). — [Stripe guide: primer on ML for fraud](https://stripe.com/guides/primer-on-machine-learning-for-fraud-protection)
- Card-testing (enumeration) is now a scheme-monitored metric (VAMP enumeration ratio ≥20% with ≥300k enumerated transactions). Velocity/BIN-attack detection is therefore a compliance issue, not just a loss issue. — [Visa VAMP fact sheet](https://corporate.visa.com/content/dam/VCOM/corporate/visa-perspectives/security-and-trust/documents/visa-acquirer-monitoring-program-fact-sheet-2025.pdf)

### Inferences
- **Jupiter risk design:**
  - Features: sliding-window velocity counters in Redis (attempts per card fingerprint / IP / device / BIN / merchant over 1 min, 1 h, 24 h; decline ratio per BIN for card-testing detection).
  - Device fingerprint: the 3DS Method data plus a JS SDK hash.
  - A simple logistic regression or GBDT score served in-process in Go (ONNX), with a deterministic rules DSL (e.g. CEL-go, or expr-lang/expr) evaluated after scoring.
  - Actions: ALLOW / BLOCK / REVIEW / REQUEST_3DS / SCA_EXEMPT.
- **3DS decisioning:** request a challenge when score > T1. Take a TRA exemption when score < T0 and amount is under the PSD2 TRA band (for EU context). Otherwise go frictionless. Remember the liability trade-off: exemptions keep liability with the merchant/PSP.
- **Label pipeline:** join TC40-like fraud reports and 10.4/4837 chargebacks back to auths via RRN / network transaction ID. Train only on matured data (e.g. transactions older than 90–120 days) to handle label delay.

### Gaps
- Adyen RevenueProtect and Checkout.com engineering writeups were not reviewed.
- There is no public benchmark data for rules-only vs ML approval-rate lift beyond Stripe's own claims (vendor source, possible bias).
