# Reconciliation

Each day, Jupiter matches every movement of your money against what the counterparty that
carried it reports:
- the card network's clearing files;
- the centralized settlement's grades;
- the bank's CNAB returns and statements;
- the Pix bank's SPI statement.

What does not match is a **break**, which stays open until it does or Jupiter's operations
team resolves it. [How it works](../reconciliation.md) describes the algorithm.

```sh
curl http://127.0.0.1:8080/v1/reconciliation/reports/2026-10-01 -H "Authorization: Bearer $SK"
```

A day's report covers your movements of that day, per counterparty and stream:
- `matched` and `matched_amount`: how many matched, and for how much;
- `open`: the breaks still open at the day's end;
- `opened` and `resolved`: those opened and resolved by that day's reconciliation;
- `ageing`: the open breaks by age, up to a day, a week, a month, or older.

The report lists the breaks themselves too. `GET /v1/reconciliation/breaks?status=open`
lists them all, newest first.

| Kind | Meaning |
|---|---|
| `missing_at_counterparty` | Jupiter recorded the movement; the counterparty has not |
| `missing_at_jupiter` | The counterparty reports a movement of yours that Jupiter has no record of |
| `duplicate` | The counterparty reports the same movement twice |
| `amount_mismatch` | The two sides disagree on the amount |
| `probable_match` | Two records with different keys look like the same movement, and wait for a person |
| `unreadable` | The counterparty listed a record of yours that cannot be matched, such as one without a reference; a person looks into it |

Each break says what the movement is known by (`key`: an RRN, a nosso número, an
end-to-end id, a transfer), how alike the two sides are (`score`, out of 100), and why
(`reasons`).

A record that arrives late resolves its break the day it arrives (`resolution:
matched`). Anything else is resolved by Jupiter's operations team (`resolved_by_operator`,
or `confirmed` for a probable match).

Movements that concern no merchant, such as a settlement's credit, are reconciled too, but
are not shown to you. Keys need `reconciliation:read`.
