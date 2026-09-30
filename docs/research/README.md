# Research

The research that grounds Jupiter's design, carried out on 2026-09-29 before any code was
written. Every later decision in [`../plan.md`](../plan.md) and in the ADRs should be
traceable to a finding here, or state why it departs from one.

| Document | Covers |
|---|---|
| [`payments-market.md`](payments-market.md) | The synthesis: how the payments market works globally and in Brazil, and what it implies for Jupiter |
| [`notes/01-market-structure.md`](notes/01-market-structure.md) | Roles (issuer, acquirer, sub-acquirer, scheme), message and money flow, MDR and interchange, market size and share |
| [`notes/02-brazil-regulation-and-receivables.md`](notes/02-brazil-regulation-and-receivables.md) | Payment-institution licensing, centralized settlement, client-fund segregation, the receivables registry and anticipation |
| [`notes/03-pix-and-boleto-specs.md`](notes/03-pix-and-boleto-specs.md) | Pix identifiers, the BCB Pix API, BR Code, MED 2.0, Pix Automático, boleto barcode and CNAB 240 |
| [`notes/04-card-rails.md`](notes/04-card-rails.md) | ISO 8583, authorization validity, 3DS2, network tokens, PCI DSS v4.0.1, disputes and risk scoring |
| [`notes/05-psp-api-design.md`](notes/05-psp-api-design.md) | How Stripe, Adyen and Brazilian PSPs shape their APIs and objects |
| [`notes/06-engineering-writeups.md`](notes/06-engineering-writeups.md) | Ledgers, hot accounts, idempotency, unknown outcomes and correctness testing, from published engineering writeups |
| [`notes/07-open-source-landscape.md`](notes/07-open-source-landscape.md) | Existing open-source ledgers, orchestrators, ISO 8583, Pix, CNAB and Go infrastructure, with what to reuse |

## How to read it

Claims are cited inline. Anything marked **unverified** came from background knowledge or a
weak source and must be checked against a primary text before Jupiter treats it as a rule;
until then it is modelled as configuration, not code. The synthesis keeps a table of every
conflicting or unverified claim and a list of primary texts that were not read.

Brazilian regulation changes quarter by quarter. These documents describe the state of
things on the research date and are not updated in place: a later revision is a new dated
document that names what it supersedes.
