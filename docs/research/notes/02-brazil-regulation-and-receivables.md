# Brazilian Payments Regulation and the Receivables (Recebíveis) Ecosystem, as of Sept 2026

Scope: the rules a Brazilian PSP, instituição de pagamento (IP) or subcredenciador must follow, and how the card-receivables registry works, so that "Jupiter" (Go backend) can model them with simulated adapters. Research date: 2026-09-29. Primary texts were read where I could get them (the consolidated text of Res. BCB 264 and Res. BCB 80 on LegisWeb, the BCB-published Convenção entre Entidades Registradoras PDF, and BCB Voto 159/2025 with its draft resolution). Everything else comes from secondary law-firm or industry summaries, and I say so where that applies.

---

## 1. Legal framework: Lei 12.865/2013, IP types, Res. BCB 80/2021, Res. BCB 150/2021, subcredenciadores, centralized settlement, segregation of funds, and 2024–2026 changes

### Takeaway
Lei 12.865/2013 is the statute. Res. BCB 80/2021 defines the IP types and their authorization. Res. BCB 150/2021 (Annex I) holds the arranjo rules and was substantially amended by **Res. BCB 522/2025**. The biggest changes in 2025–2026:
- **Res. BCB 494/2025 removed the volume thresholds that let IPs operate without authorization.** Every IP now needs prior BCB authorization. Incumbents had to file between **1 and 31 May 2026**.
- **Res. BCB 522/2025 requires every subcredenciador to join centralized settlement (SLC/Núclea) as a payer to merchants, regardless of volume, by 11 May 2026.** It also caps participant chargeback liability at 180 days.
- **Res. Conjunta 14/2025 (plus BCB 517/518) replaced flat capital minimums with activity-based capital.** It phases in from June 2026 to January 2028.
- **Res. Conjunta 16/2025 regulates BaaS and bans contas-bolsão.** The compliance deadline is 31 Dec 2026.

### Cited Findings

**IP modalities (Res. BCB 80/2021, art. 3º)** — [LegisWeb, Res. BCB 80 consolidated](https://www.legisweb.com.br/legislacao/?id=411674)
- There are four types:
  - (I) **emissor de moeda eletrônica**: manages a *prepaid* conta de pagamento.
  - (II) **emissor de instrumento de pagamento pós-pago**: manages a *post-paid* conta de pagamento for the payer.
  - (III) **credenciador**: does not manage a conta de pagamento; enables merchants to accept payment instruments and participates in settlement as a creditor to the issuer.
  - (IV) **iniciador de transação de pagamento (ITP)**: initiates payments and is barred from holding client funds (art. 4º).
- "Moeda eletrônica" means reais stored in an electronic device or system that let the user make payment transactions (art. 3º §1º).
- An IP must be a Ltda or S.A. whose main corporate purpose is one of the activities in Lei 12.865 art. 6º III (art. 5º). It must have a governance policy (art. 6º). It must give 90 days' notice before stopping a modality (art. 15).
- Legacy capital minimums, now being superseded (see Res. Conjunta 14 below):
  - R$ 2,000,000 per modality I–III;
  - R$ 1,000,000 for ITP;
  - R$ 2,000,000 for the "atividade especial" of art. 4º-A.
- Banks (comercial/múltiplo), cooperatives, SCDs, SEPs and others are exempt from separate IP authorization for listed modalities (art. 16). They must give 90 days' notice.

**Authorization regime change (Res. BCB 494, 5 Sep 2025)** — [LegisWeb Res. 80 consolidated](https://www.legisweb.com.br/legislacao/?id=411674); [LegisWeb Res. 494](https://www.legisweb.com.br/legislacao/?id=483156); [AML Reputacional](https://www.amlreputacional.com.br/2025/11/18/instituicoes-de-pagamento-reguladas-como-se-preparar-diante-da-resolucao-bcb-no-494/)
- New art. 9º: "A instituição de pagamento deve solicitar autorização ao Banco Central do Brasil para iniciar a prestação de serviço de pagamento." There is no longer any volume-based exemption.
- **SUPERSEDED — old thresholds (Res. 80 arts. 10–13, revoked by 494):**
  - Credenciadores and post-paid issuers needed authorization only above **R$ 500 million** in 12-month volume (art. 11).
  - E-money issuers faced a staggered schedule: R$ 500 mi in transactions or R$ 50 mi in balances, falling over time to R$ 100 mi or R$ 10 mi (art. 10).
- New art. 9º-A says these incumbents must request authorization **from 1 May 2026 to 31 May 2026**:
  - e-money issuers that started before 1 Mar 2021 and are not authorized;
  - post-paid issuers and credenciadores that started before 5 Sep 2025 and are not authorized.
- If an incumbent misses the window, it has about 30 days after BCB notice to wind down.
- Another source says the regularization deadline was moved forward "from December 2029 to May 2026". — [NDM Advogados](https://ndmadvogados.com.br/artigo/mudancas-no-capital-minimo-das-instituicoes-autorizadas/)

**Capital and account rules (Nov 2025)** — [Silveiro Advogados](https://silveiro.com.br/banco-central-reforma-regras-de-capital-e-determina-encerramento-compulsorio-de-contas-bolsao/); [NDM Advogados](https://ndmadvogados.com.br/artigo/mudancas-no-capital-minimo-das-instituicoes-autorizadas/)
- **Res. Conjunta 14/2025** (3 Nov 2025) bases minimum capital and PL on the activities an institution actually performs, not on its legal category.
  - Fixed "custo" component: R$ 2 mi per activity category, plus R$ 5–10 mi for technology-intensive services (BaaS, Open Finance, Pix).
  - Variable "atividades" component by risk level: services R$ 1 mi, custody R$ 3 mi, intermediation R$ 5 mi, credit R$ 7 mi.
  - The total is multiplied by a funding factor of 60–200%. Banks pay an extra R$ 30 mi.
  - Phase-in steps are 25%, 50%, 75% and 100%. The first calculation is on 1 Jun 2026, transition runs 1 Jul 2026 to 31 Dec 2027, and full compliance is due 1 Jan 2028.
- **Res. BCB 517/2025** creates a single activity taxonomy. Institutions must notify the BCB 90 days before launching a new activity. Activities had to be pre-declared by 30 Jul 2025 (per NDM).
- **Res. BCB 518/2025** (in force 1 Dec 2025) makes closure of a conta de pagamento mandatory in three cases:
  - serious registration irregularities;
  - third-party fund movement that hides the real owner;
  - use of the account to provide financial or payment services without authorization.
  - Records must be kept for 10 years.
- **Res. CMN 5.261/2025** applies the same closure rule to deposit accounts used as "contas-bolsão".
- Figures come from law-firm summaries; I did not read the primary text of Res. Conjunta 14.

**BaaS (Res. Conjunta 16, 28 Nov 2025)** — [Capital Aberto](https://capitalaberto.com.br/regulamentacao/bc-regulamenta-baas-e-instituicoes-terao-um-ano-para-adequacao/); [Demarest](https://www.demarest.com.br/bc-e-cmn-regulamentam-banking-as-a-service/); [Asaas](https://blog.asaas.com/regulamentacao-baas/); [LegisWeb](https://www.legisweb.com.br/legislacao/?id=487037)
- Defines a BaaS *prestadora* (a licensed institution) and a *tomadora*.
- Only three services can be offered via BaaS: (i) deposit or payment accounts, (ii) **credenciamento (acquiring)**, (iii) credit. Delivery must be electronic, and each account must be tied to the end client.
- Cooperatives and consórcio administrators cannot take part. Omnibus (bolsão) accounts are prohibited.
- The prestadora had 90 days to self-assess and report to the BCB. Full adaptation is due **31 Dec 2026**.

**Arranjo rules and subcredenciador (Res. BCB 150/2021 Annex I, amended by Res. BCB 522 of 10 Nov 2025)** — [BCB Voto 159/2025 + draft resolution](https://normativos.bcb.gov.br/Votos/BCB/2025159/Voto_do_BC_159_2025.pdf); [LegisWeb Res. 522](https://www.legisweb.com.br/legislacao/?id=486238); [NDM Advogados](https://ndmadvogados.com.br/artigo/liquidacao-centralizada-subadquirente/); [Núclea](https://www.nuclea.com.br/resolucao-bcb-522-2025-subcredenciadoras-slc-nuclea/); [Barcellos Tucunduva](https://barcellostucunduva.com.br/2025/11/13/bcb-publica-nova-regra-sobre-arranjos-de-pagamento/)
- **Subcredenciador definition** (Annex I art. 2º IX): a participant of the arranjo that exclusively enables merchants to accept an instrument but does *not* take part in settlement as a creditor to the issuer. It is an arranjo participant, not an IP modality under Res. 80. A company with enough activity could still be a credenciador needing authorization; see Gaps.
- **SUPERSEDED (Res. 150 original):** subcredenciadores had to join centralized settlement as *receivers*, but only had to join as *payers* to merchants above **R$ 500 mi/year**.
- **Res. 522 (new §5º-A):** participation in centralized settlement is mandatory for *all* subcredenciadores, as receivers and as payers, in arranjos subject to centralized settlement, whatever their volume.
  - Instituidores (card schemes) had 180 days from publication to implement this, a deadline of **11 May 2026**.
  - After that date, schemes must suspend new-transaction capture by non-compliant subcredenciadores.
- **Other Res. 522 content (from Voto 159/2025 and its draft):**
  - The instituidor must guarantee settlement of *all* authorized transactions, covering any residual gap with its own funds.
  - The instituidor may not make the credenciador responsible for monitoring, risk or default of subcredenciadores.
  - Participants must report every obligation settled outside centralized settlement to the settlement system (câmara).
  - The câmara must share settlement data with registradoras and the instituidor for conciliation.
  - Credenciadores and subcredenciadores may not discriminate against properly enabled issuers (art. 35-F).
  - Arranjos had 180 days to file amended rulebooks.
- **Chargeback cap (new art. 35-G, draft text in Voto 159):**
  - Participants' financial liability for chargebacks is limited to disputes started within **180 days of authorization**. After that, the instituidor bears the liability.
  - Chargeback is barred for commercial disputes arising from the merchant's bankruptcy or insolvency.
  - The rule does not affect consumers' legal rights to contest charges.
- **Chargeback definition (art. 2º XXVII):** "reversão ou cancelamento de transação de pagamento a pedido do usuário pagador ou do emissor ... decorrente de fraude, golpe, falha de processamento, falha de autorização ou desacordo comercial".
- **Context figures (Voto 159):**
  - In post-paid arranjos, the average time from transaction to merchant payout is about 2 months, because of parcelado.
  - About **R$ 480 bi (3.9% of GDP)** is outstanding to settle in credit-card arranjos at any moment.
  - The payer owes the issuer on average 26 days after an à-vista purchase.
- **SLC mechanics for subcredenciadores (Núclea):**
  - Under the "informativo via antecipação" model, a subcredenciador that pays merchants early with its own funds must inform Núclea the same day or D+1.
  - Messages use JWS signatures with ICP-Brasil certificates (SLC0912).
  - Returns arrive by HTTPS webhooks, with at most 1,000 PDV records per payload.
  - These details come from NDM's summary of Núclea specs.

**Lei do Repasse (Lei 14.031/2020, amending Lei 12.865)** — [BCB Voto 159/2025](https://normativos.bcb.gov.br/Votos/BCB/2025159/Voto_do_BC_159_2025.pdf)
- Funds received from the payer must be passed through (repassados) to the merchant, or to whoever is sub-rogated in the right to receive (e.g., a cessionário).
- The payment flow is protected from arresto, sequestro and bloqueio of participants (issuer, credenciador, subcredenciador).
- It is not affected by special regimes, recuperação judicial, falência or liquidação of a participant.

**Segregation of client funds (Lei 12.865/2013, art. 12)** — [Planalto, Lei 12.865](https://www.planalto.gov.br/ccivil_03/_ato2011-2014/2013/lei/l12865.htm)
- Funds held in contas de pagamento form a *patrimônio separado*. They do not mix with the IP's own assets and cannot answer for the IP's obligations or be seized for its debts. This is statute text; I did not re-fetch it this session.

### Inferences
- For Jupiter, the realistic 2026 posture is a **subcredenciador that participates in SLC as both receiver and payer**. It reports the merchant payout schedule to Núclea and registers URs with a registradora.
  - The main alternative is an authorized credenciador IP. It would need BCB authorization and about R$ 2 mi in legacy capital, moving to activity-based capital by 2028.
  - Modeling "exempt below R$ 500 mi" would be out of date.
- The chargeback window should be a configurable 180-day scheme liability cap. Scheme-specific reason codes and deadlines should sit behind a per-scheme adapter.
- A "conta de pagamento" ledger should enforce ring-fenced client balances: a separate ledger book from the PSP's own funds. Any omnibus design ("conta-bolsão") is now explicitly non-compliant.

### Gaps
- I did not read the final published text of Res. 522. Chargeback and SLC details come from the Voto 159 *draft* annex (6 Nov 2025) plus secondary summaries, and the final wording may differ slightly.
- I found no primary text of Res. Conjunta 14/2025. The capital figures come from law-firm summaries.
- Whether a large subcredenciador must now be authorized as a credenciador was not confirmed. Under Res. 80 as amended, anyone acting as a credenciador (a creditor to the issuer in settlement) needs authorization; a true subcredenciador is not an IP modality.
- Rules for investing e-money balances (believed to be Res. BCB 81/2021: 100% in reserves at the BCB or federal government bonds) were **not verified** this session.
- I did not read the 2026 BCB normative flow after April 2026 (e.g., 562/2026) beyond the receivables item below.

---

## 2. Recebíveis: Res. CMN 4.734/2019, Res. BCB 264/2022 (as amended through 2026), registradoras, agenda and UR, efeitos de contrato, conciliation, interoperability

### Takeaway
Two rules govern this area:
- **Res. CMN 4.734/2019** sets the rules for discounting receivables and for credit guaranteed by them. It is still in force as amended by Res. CMN 5.045/2022.
- **Res. BCB 264/2022** sets the rules for registering receivables. It replaced Circular 3.952/2019 and was amended by Res. BCB 349/2023, 373/2024, 456/2025, 472/2025, 514/2025, 530/2025 and 562/2026.

The credenciador or subcredenciador registers each **UR** and keeps it updated by D+1. Financiers place **effects** on URs (troca de titularidade or ônus/gravame) through any registradora. The multilateral **Convenção** handles interoperability, first-come priority, and the tariff caps set by Res. BCB 472/2025.

### Cited Findings

**Definitions (Res. 264 art. 2º)** — [LegisWeb Res. 264 consolidated](https://www.legisweb.com.br/legislacao/?id=438920)
- **Instituições credenciadoras** include credenciador IPs, financial institutions that do credenciamento, and e-money issuers that interoperate with the payer's arranjo.
- **Unidade de recebíveis (UR)**: a financial asset made of arranjo receivables, including pre-contracted anticipations. It is keyed by the same:
  - (a) CNPJ/CPF of the merchant (usuário final recebedor);
  - (b) arranjo;
  - (c) credenciador or subcredenciador;
  - (d) settlement date.
- **Agenda de recebíveis**: the set of URs sharing (a)–(c).
- **Negociação** means discount operations, credit guaranteed by receivables (Res. 4.734 art. 2º V and VI), or any operation that changes possession or effective or fiduciary ownership.

**Credenciador duties (Res. 264)** — [LegisWeb Res. 264](https://www.legisweb.com.br/legislacao/?id=438920)
- **Registration and update (art. 3º):**
  - Register each UR in a registration system with its constituted amount.
  - Update it with newly constituted amounts **by the business day after the underlying sale** (§1º).
  - The UR amount is gross, minus *only* (§3º): (I) MDR and fees of the credenciador or subcredenciador; (II) reversals from cancellation, contestation or fraud; (III) settlements already made, including post-contracted anticipations; (IV) blocks under art. 8º.
  - Pre-contracted anticipated amounts are registered on the UR for the anticipated settlement date (§4º).
- **Negotiation (arts. 4º–5º):**
  - A negotiation moves ownership of amounts constituted *now and in the future* on that UR to the beneficiary.
  - The registry must support dividing a UR by **fixed value** or **percentage**.
- **Blocks (art. 8º):**
  - The credenciador may block amounts for a risk reserve, or to offset merchant debts such as fines, chargebacks on transactions already settled, and contractual offsets.
  - Blocked amounts are not part of the constituted UR amount.
  - A block cannot reduce amounts already allocated to contracts.
  - Blocked amounts cannot be negotiated or post-anticipated.
  - The reserve must match actual risk and be set out in the merchant contract.
- **Contract information (art. 9º):**
  - The credenciador must send information on contracts that merchants sign with non-financial entities.
  - When onboarding a merchant, it must check the registries for existing contracts that reach the new agenda.
  - If any exist, it cannot sign promessa-de-cessão (trava) contracts until those effects end.
- **Settlement (art. 10):** settle each UR according to the ownership and domicile information from the registry.
- **Conciliation (art. 11):**
  - Daily: constituted UR amounts and contract-effect amounts.
  - Weekly: settlement amounts and domiciles.
  - Fortnightly: the list of merchants with an active contract.
  - Inconsistencies must be fixed within **2 business days**.
  - From 11 May 2026 (Res. 514 wording), settlement conciliation must use data that the centralized settlement system sends directly to the registry.
- **Other duties:**
  - Answer registry contestations within **3 business days** (art. 12).
  - Give merchants their agenda detail: deductions, free value, value per effect, and contract specs (art. 13).
- **Subcredenciadores (art. 14):** credenciadores must bind subcredenciadores contractually to Res. 264, with penalties up to suspension. Registradoras send monthly compliance reports on subcredenciadores to their credenciador (art. 15 §8º).
- **Revoking a trava or cancelling a pre-contracted anticipation (art. 7º):**
  - The Res. 514/2025 wording (effective 11 May 2026) required the credenciador to ask the registry to lift gravames and ônus within **2 business days** of the merchant's resilição.
  - It also required cancelling a pre-contracted anticipation within **2 business days** of the request, which may come through another registry participant.
  - If the credenciador missed a deadline, the registry would automatically re-prioritize contracts and report the credenciador to the BCB.
  - Cancellation applies only to transactions made after it.
  - **CAVEAT:** Res. BCB 562 of 30 Apr 2026 revoked Res. 514's rewrite of art. 7º and re-regulated "procedimentos relativos ao cancelamento de antecipação pré-contratada", effective 11 May 2026. I could not read 562's final wording. — [LegisWeb Res. 562](https://www.legisweb.com.br/legislacao/?id=495271); [LegisWeb Res. 514](https://www.legisweb.com.br/legislacao/?id=485366)

**Registradora duties (Res. 264 art. 15–17)** — [LegisWeb Res. 264](https://www.legisweb.com.br/legislacao/?id=438920)
- Receive agendas and contracts.
- Show agendas to participants **only with the merchant's authorization (opt-in)**.
- Accept gravame and ônus commands.
- Give credenciadores UR ownership and domicile data for settlement.
- Automatically extend contract effects to URs registered later (§3º), in the chronological order in which contracts were sent (§4º).
- Must *not* automatically create extra gravames beyond the initial request (§1º).
- Handle participant contestations within **5 business days** (§7º).
- Complete portability between registries within **30 days, extendable by 15** (art. 16 §1º).
- **Registration and agenda-disclosure services are free for credenciadores and subcredenciadores** (art. 17 §2º).
- Tariff-table changes need 30 days' notice (from 5 Jan 2026).

**The Convenção entre Entidades Registradoras (Res. 264 art. 18)** — [BCB-hosted Convenção PDF](https://www.bcb.gov.br/content/estabilidadefinanceira/spb_docs/convencoes/Conven%C3%A7%C3%A3o%20entre%20Entidades%20Registradoras%20-%20Receb%C3%ADveis%20de%20Arranjos%20de%20Pagamento.pdf); also [CERC copy](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf)
- **Participant roles:**
  - Participants are Credenciadoras, Subcredenciadoras, Financiadores (financial institutions) and Não Financeiras (e.g., credenciador IPs acting as financiers).
  - A credenciador or subcredenciador connects to **one registradora at a time** for its registrations, to keep them unique.
  - Financiers may connect to one or more.
- **Effect types:** "1 – Troca de titularidade; 2 – Ônus". Ônus covers credit guaranteed by URs, promessa de cessão (trava) and judicial penhora.
- **Minimum registration payload:**
  - CNPJ of the credenciador or subcredenciador;
  - CNPJ/CPF of the merchant;
  - arranjo code, from the SPB arranjo table (e.g., VCP = Visa pré-pago, MCP = Mastercard pré-pago);
  - settlement date (DD/MM/AAAA);
  - total constituted value (net);
  - pre-contracted constituted value;
  - payment domicile (account type, bank, branch/account, or payment account).
- **Per-effect agenda response:**
  - effect type;
  - beneficiary or holder CNPJ;
  - committed value;
  - domicile (ISPB + account type + branch/account);
  - free value.
- **Settlement order (item 3.13):** first accepted contract first, then later contracts chronologically, then the free portion.
- **Reduction order (item 3.14):** when a UR is reduced (e.g., a chargeback), take from the free portion first, then from the **most recent** contract backwards (LIFO on reductions).
- **Splitting a contract across credenciadores (Regra de Repartição, items 3.10–3.11):** a contract names specific credenciadores or covers the holder's whole agenda. Allocation is proportional over the whole agenda, not per date.
- **Other rules:**
  - Settlement must be reported to the registry by the next business day after it happens (5.3.6).
  - Post-contracted anticipation must be reported and follows contract priority (5.4).
  - Judicial constrictions take priority over other effects, *except* ownership transfers and cessão fiduciária (5.9.16).
  - Renegotiation keeps the original priority (5.10.2.1).
  - Processing is FIFO by confirmation time across registries (6.1–6.2).
- **Interoperability tariff price caps (Res. BCB 472/2025, R$ per event)**, adjusted annually by IPCA:

| Event | Jul 2025 | Jul 2026 | Jul 2029 |
|---|---|---|---|
| Agenda | 0.037295 | 0.030055 | 0.008335 |
| Efeito de contrato | 0.004190 | 0.003562 | 0.001680 |
| Atualização de contrato | 0.000034 | 0.000028 | 0.000009 |

**Registradoras**
- The convention signatories for card receivables are the registradoras authorized under Circular 3.743. The Brazilian market knows them as CERC, CIP/Núclea, TAG and B3. [Convenção](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf)
- A separate convention for *duplicatas escriturais* (signed 29 Nov 2024) has seven registradoras: CERC, Núclea, B3, TAG, CRDC, Grafeno, Quicksoft. That is a different asset class. — [antecipafacil (secondary)](https://antecipafacil.com.br/registradoras-de-duplicata-escritural)

**Res. 264 practice changes from April 2024** — [Fiserv](https://www.fiserv.com.br/insights/entenda-a-resolucao-264/); [Mattos Filho](https://www.mattosfilho.com.br/unico/bcb-regulamentacao-recebiveis-cartao/)
- New sale-cancellation rules.
- Contestation of gravames and cessões.
- Rules on netting POS rental and chargeback debits.
- Agenda queries by UR.

### Inferences
Suggested model for Jupiter:
- `ReceivableUnit{merchantDoc, arrangementCode, acquirerCNPJ, settlementDate, constitutedTotal, constitutedPreContracted, effects[]}` with an event-sourced history.
- `ContractEffect{type: OWNERSHIP_TRANSFER|LIEN, beneficiary, rule: FIXED|PERCENT, value, domicile, acceptedAt}`.
- Waterfall settlement: FIFO over effects, then the free portion.
- Reductions consume the free portion first, then LIFO over effects.
- A simulated `RegistryAdapter` exposing:
  - `RegisterUR`, `UpdateUR` (D+1 SLA), `NotifySettlement` (D+1);
  - `GetAgenda` (opt-in gated), `ApplyEffect`, `Contest` (3 business days to answer);
  - `DailyReconcile` (daily, weekly and fortnightly jobs, 2 business days to fix).

### Gaps
- The final text of Res. BCB 562/2026 (the new art. 7º) and of Res. BCB 530/2025 was not read.
- Registradora market shares for card receivables were not found.
- I did not find whether prepaid arranjos now fall under mandatory registration. The Convenção says prepaid is "facultativo", and Res. 472's UR definition hints at an expansion.
- CERC, Núclea and TAG technical layouts (API specs) were not fetched.

---

## 3. Antecipação de recebíveis: economics, operations, who can offer it

### Takeaway
There are two kinds of antecipação:
- **Pre-contracted:** the settlement date is moved forward contractually, and the UR is registered on the new date.
- **Post-contracted:** amounts already constituted are paid out early by the credenciador or subcredenciador.

Financial institutions and non-financial entities (including IPs and FIDCs) can also buy or lien receivables through the registries (discount/cessão or collateralized credit). Since 4.734, merchants are no longer tied to their acquirer for this.

### Cited Findings
- Res. CMN 4.734/2019 defines:
  - **desconto** (art. 2º V);
  - **credit guaranteed by receivables** (art. 2º VI);
  - **antecipação**, split into **pré-contratada** (VII a) and **pós-contratada** (VII b).
  - It was amended by Res. CMN 5.045/2022, effective 1 Dec 2022.
  - [Res 4.734 (BCB PDF)](https://normativos.bcb.gov.br/Lists/Normativos/Attachments/50795/Res_4734_v6_L.pdf); [Convenção definitions](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf); search summary
- The Convenção treats "Não Financeira" as any contract-reporting entity that is not a financial institution, "incluindo ... instituições de pagamento credenciadoras ... ou às Subcredenciadoras". Non-FIs therefore do take part as financiers and contract counterparties. — [Convenção](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf)
- Before 4.734, antecipação was in practice limited to the processing acquirer. Now merchants can choose among several institutions, including non-financial ones. — [Equals](https://equals.com.br/en/blog/resolucao-4734-2019-e-circular-bacen-3952-2019-na-antecipacao-de-recebiveis/); [subadquirente.com](https://www.subadquirente.com/post/antecipacao-de-recebiveis-novas-regras-descentralizam-o-processo-de-recebimentos-das-adquirentes)
- The market prices antecipação as a monthly rate × months anticipated, applied per installment. IOF may apply when the operation is characterized as credit. — [sejaefi](https://sejaefi.com.br/blog/taxa-de-antecipacao); [Migalhas on tax reform impact](https://www.migalhas.com.br/depeso/418345/impacto-da-reforma-tributaria-na-operacao-de-antecipacao-de-recebiveis). These are industry blogs, not authoritative sources.
- A post-contracted anticipation must honor existing contract effects on the UR (FIFO priority) and must be reported to the registry. It cannot touch blocked amounts. — [Convenção 5.4](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf); [Res. 264 art. 8º §3º](https://www.legisweb.com.br/legislacao/?id=438920)
- Operationally, a subcredenciador that pre-funds merchants reports this to SLC/Núclea in the "informativo via antecipação" modality (same day or D+1). — [NDM](https://ndmadvogados.com.br/artigo/liquidacao-centralizada-subadquirente/)

### Inferences
- **Cessão/desconto (true sale):** the merchant sells the UR. The effect is troca de titularidade, and the registry points settlement to the buyer's domicile.
- **Credit with collateral:** an ônus/gravame is placed. The FI lends and the UR settles to the FI's domicile up to the committed value.
- **Credenciador post-contracted anticipation:** the credenciador pays early from its own balance sheet and nets at the settlement date.
- Pricing formula: PV = Σ installment_i / (1 + r_monthly)^(days_i/30). Some players quote simple interest (value × r × months).
- Before quoting, run a simulated "who can offer" check: query the agenda (with opt-in), compute the free value, and apply the effect.

### Gaps
- I found no authoritative (BCB or law-firm) source on the tax classification (IOF vs none) of IP-provided antecipação, or on typical 2026 market rates.

---

## 4. Card chargebacks and consumer-law (CDC) points

### Takeaway
Chargeback rules sit in each arranjo's rulebook. Res. 522/2025 now requires them to be explicit and caps participant liability at 180 days from authorization. On the consumer side:
- **CDC art. 49** gives a 7-day right of withdrawal for purchases made outside the store (e-commerce).
- Case law treats issuers, card companies and often credenciadores as **objectively and jointly liable** to consumers for fraud or service failure.

### Cited Findings
- Chargeback is defined in Res. 150 Annex I art. 2º XXVII (draft). Participant liability covers disputes started within 180 days of authorization; after that the instituidor is liable. There is no chargeback for disputes caused by the merchant's bankruptcy. Consumers' legal rights are preserved. — [BCB Voto 159/2025 draft](https://normativos.bcb.gov.br/Votos/BCB/2025159/Voto_do_BC_159_2025.pdf)
- CDC art. 49: 7 days from signing or receipt for off-premises contracting, with a full refund and returns at no cost to the consumer. — [advocaciareis (secondary)](https://advocaciareis.adv.br/blog/chargeback/)
- Case law (secondary summaries):
  - Credenciador and issuer are jointly liable when security systems fail to detect fraud.
  - Remote processing is a "risk activity" (Código Civil art. 927 parágrafo único).
  - The STJ held a merchant liable for chargebacks where it transacted "sem cautela".
  - [advocaciareis](https://advocaciareis.adv.br/blog/chargeback/); [SCVBS on STJ](https://www.scvbs.com.br/noticias:stj-estabelece-que-lojista-e-responsavel-por-contestacao-de-compra--chargeback--se-realizar-transacoes-sem-cautela)
- Chargeback debits reduce the UR's constituted value (Res. 264 art. 3º §3º II), or they are offset through art. 8º blocks for transactions already settled. — [LegisWeb Res. 264](https://www.legisweb.com.br/legislacao/?id=438920)

### Inferences
- Jupiter's dispute module should do four things:
  - track scheme reason code and deadlines per scheme adapter;
  - enforce the 180-day liability boundary;
  - debit the UR (reducing free value first, then LIFO effects) or the merchant reserve block;
  - support the 7-day e-commerce withdrawal refund flow.

### Gaps
- I did not verify specific STJ case numbers (REsp) and did not check the final Res. 522 art. 35-G wording.

---

## 5. KYC/AML (Lei 9.613, Circular 3.978/2020) and LGPD

### Takeaway
Circular BCB 3.978/2020 still governs AML/CFT for BCB-authorized institutions, amended by Res. BCB 282/2022. It uses a risk-based approach. Res. 522 adds arranjo-level AML standardization duties for schemes. LGPD (Lei 13.709/2018) applies to all transaction and registry data.

### Cited Findings
- Circular 3.978 (23 Jan 2020) sets the AML/CFT policy, procedures and internal controls for institutions authorized by the BCB. It is built on a risk-based approach and was amended by Res. BCB 282 (31 Dec 2022). — [BCB PDF](https://normativos.bcb.gov.br/Lists/Normativos/Attachments/50905/Circ_3978_v3_P.pdf); [Compliasset](https://www.compliasset.com/alerta/765-alterada-a-circular-3-978-que-trata-de-procedimentos-de-pld-ft/); [Proteo](https://www.proteo.com.br/regulacao/circular-3978)
- Voto 159/2025 found that arranjo-level structures for ML/TF/proliferation risk had "baixa padronização". The resolution adds AML duties for instituidores over their participants. — [BCB Voto 159](https://normativos.bcb.gov.br/Votos/BCB/2025159/Voto_do_BC_159_2025.pdf)
- Res. BCB 518/2025 requires closing payment accounts that have serious registration irregularities, that are used to hide third-party funds, or that are used for unauthorized services, with 10-year record retention. — [Silveiro](https://silveiro.com.br/banco-central-reforma-regras-de-capital-e-determina-encerramento-compulsorio-de-contas-bolsao/)
- AML/CFT compliance and governance are prerequisites for the May 2026 authorization filings under Res. 494. — [AML Reputacional](https://www.amlreputacional.com.br/2025/11/18/instituicoes-de-pagamento-reguladas-como-se-preparar-diante-da-resolucao-bcb-no-494/)
- The registradora Convenção classifies personal data under LGPD (Lei 13.709/2018) as confidential information. — [Convenção](https://www.cerc.com/wp-content/uploads/2025/04/convencao_entre_entidades_registradoras_recebiveis_de_arranjos_de_pagamento.pdf)
- Agenda disclosure to financiers requires merchant opt-in (Res. 264 art. 15 III), a consent-gated data flow. — [LegisWeb Res. 264](https://www.legisweb.com.br/legislacao/?id=438920)

### Inferences
- Modules to model:
  - KYC/KYB onboarding (CPF/CNPJ validation, UBO, PEP and sanctions screening);
  - transaction monitoring with COAF reporting (simulated);
  - account-closure workflows under Res. 518;
  - 10-year retention;
  - LGPD-aware consent records for agenda opt-in/opt-out.

### Gaps
- I did not verify Circular 3.978 specifics: the COAF reporting deadline (believed to be the next business day) and record-keeping periods. I also did not find any 2024–2026 amendment beyond Res. 282/2022.
- No PSP-specific LGPD/ANPD guidance was found.

---

## 6. Interchange caps (Res. BCB 246/2022)

### Takeaway
Res. BCB 246/2022 (26 Sep 2022, in force 1 Apr 2023) caps interchange at **0.5% for debit** and **0.7% for prepaid**. It also aligned merchant-funding timing for prepaid with debit. Credit interchange is not capped.

### Cited Findings
- Prepaid interchange is capped at 0.7% and debit at 0.5%, effective 1 Apr 2023. The rule also standardized the deadline for funds to reach merchants. — [Silva Lopes](https://silvalopes.adv.br/resolucao-bcb-no-246-22-teto-do-tic-para-cartoes-pre-pagos/); [FGV Regulação em Números](https://regulacaoemnumeros-direitorio.fgv.br/post/banco-central-aprova-resolucao-que-disciplina-tarifa-de-intercambio-de-cartao-de-debito-e-pre); [Finsiders](https://fintechsbrasil.com.br/2022/09/26/bc-limita-tarifa-de-intercambio-de-cartoes-pre-pagos-a-07-medida-afeta-fintechs-finsiders)

### Inferences
- The fee engine should validate that the interchange passed through for debit and prepaid never exceeds 0.5% and 0.7%. Credit interchange should come from scheme tables.

### Gaps
- Not verified:
  - whether the cap is measured as a weighted average or per transaction (as I recall, it is a weighted average with a per-transaction ceiling of 0.8%/1.0%);
  - the exact prepaid settlement deadline (believed to be D+2, matching debit);
  - whether any 2024–2026 change applies.
- The Mattos Filho page returned 403.
