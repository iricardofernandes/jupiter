# 0041. Metrics through OpenTelemetry, alerts in Prometheus, a runbook for each

- Status: Accepted
- Date: 2026-10-01

## Context

Since phase 0 the stack has run an OpenTelemetry Collector, Prometheus and Grafana (ADR
0002), but Jupiter itself sent nothing. Everything that goes wrong without failing a
request was invisible:
- a background task that keeps failing;
- an outcome that stays unknown;
- a reversal the network never acknowledges;
- a reconciliation break ageing.

The plan asks for a runbook for every alert. Alerts need metrics first.

## Decision

**Metrics** go out over OTLP/HTTP to the Collector, which exposes them to Prometheus.
They are on when `OTEL_EXPORTER_OTLP_ENDPOINT` names the Collector, and off otherwise.

| Source | Metrics |
|---|---|
| The API | `jupiter.api.request.duration`, by route pattern and status |
| Every background task (`service.Every`) | runs by outcome, duration, last success and interval |
| Every database pool | connections by state; acquires, and those that waited |
| The worker, every minute | gauges each module reads of its own state (below) |

The worker's gauges cover:
- the ledger's last check, drifted accounts and balance queue;
- outcomes left unknown for card operations, refunds and payouts;
- Pix received and not returned;
- requests stopped between phases;
- reversals unacknowledged and clearing exceptions;
- disputes near their deadline;
- reconciliation breaks by counterparty and age, and how far reconciliation is behind;
- the job queue's backlog, retries and discards.

Each module declares its own gauges and reads them through partial indexes that already
exist. A probe that fails leaves its gauges absent rather than zero.

**Alerts** are Prometheus rules (`deploy/prometheus/alerts.yml`), 21 of them:
- **critical**, which pages: money may be wrong, or nothing is processed;
- **warning**, for working hours: something is late, and the system is still correct.

`promtool` unit tests check the rules' logic (`make check-alerts`).

**Runbooks** (`docs/runbooks`), one per alert and named after it, say:
- what the alert means and what it costs;
- how to diagnose it, with SQL and `jupiterctl`;
- how to fix it, and when to escalate.

A test ties the three together:
- every alert has a runbook, and every runbook an alert;
- every Jupiter metric an alert reads is one Jupiter reports.

## Alternatives rejected

- **A Prometheus client and a `/metrics` endpoint per service.** It is simpler, but it
  bypasses the Collector that ADR 0002 chose as the one place a deployment changes.
- **Alerting on logs.** The worker already logs every failure. But "every run in fifteen
  minutes failed" or "the oldest delta is a minute old" are statements about rates and
  ages, which metrics answer and logs do not.
- **Domain gauges computed at scrape time.** Every scrape would query the database from
  the scraping path. A sample a minute old is fresh enough for alerts measured in
  minutes.

## Consequences

- Alertmanager and on-call routing are left to the deployment (phase 15). Prometheus
  evaluates the alerts and shows them.
- A new alert needs a runbook to pass the tests, and a new gauge a module that reports
  it.
