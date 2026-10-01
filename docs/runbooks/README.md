# Runbooks

One for each alert in [`deploy/prometheus/alerts.yml`](../../deploy/prometheus/alerts.yml),
named after it; each alert links to its own. `test/observability` checks that every alert
has a runbook, that every runbook belongs to an alert, and that every metric an alert
reads is one Jupiter reports.

**Critical** alerts page someone at once: money may be wrong, or nothing is being
processed. **Warning** alerts wait for working hours: something is late, and the system
is still correct.

The metrics come from three places:
- the API and the worker, through OpenTelemetry to the Collector and on to Prometheus;
- the background tasks, about themselves;
- the worker, which reads each module's state every minute.

Each runbook says what to look at, with SQL against the database the services use
(`JUPITER_DATABASE_URL`) and `jupiterctl`. Times in the database are UTC.

| Alert | Severity | What it watches |
|---|---|---|
| [JupiterLedgerInvariantViolated](JupiterLedgerInvariantViolated.md) | critical | The ledger's own checks |
| [JupiterLedgerBalanceDrift](JupiterLedgerBalanceDrift.md) | critical | Cached balances against entries |
| [JupiterAPIErrors](JupiterAPIErrors.md) | critical | Server errors from the API |
| [JupiterWorkerDown](JupiterWorkerDown.md) | critical | The worker being alive |
| [JupiterLedgerQueueLagging](JupiterLedgerQueueLagging.md) | warning | Batched balances being applied |
| [JupiterAttemptsUnresolved](JupiterAttemptsUnresolved.md) | warning | Card operations with unknown outcomes |
| [JupiterRefundsUnresolved](JupiterRefundsUnresolved.md) | warning | Refunds with unknown outcomes |
| [JupiterPayoutsUnresolved](JupiterPayoutsUnresolved.md) | warning | Payouts with unknown outcomes |
| [JupiterPixUnreturned](JupiterPixUnreturned.md) | warning | Pix that paid nothing, not returned |
| [JupiterNetworkForwardsPending](JupiterNetworkForwardsPending.md) | warning | Reversals and advices to the card network |
| [JupiterClearingExceptions](JupiterClearingExceptions.md) | warning | Clearing records that match nothing |
| [JupiterReconciliationBreaksAgeing](JupiterReconciliationBreaksAgeing.md) | warning | Reconciliation breaks older than a day |
| [JupiterReconciliationBehind](JupiterReconciliationBehind.md) | warning | Reconciliation keeping up |
| [JupiterDisputeDeadlineNear](JupiterDisputeDeadlineNear.md) | warning | Disputes waiting for merchants |
| [JupiterPaymentLatencyHigh](JupiterPaymentLatencyHigh.md) | warning | How long payments take |
| [JupiterDatabasePoolSaturated](JupiterDatabasePoolSaturated.md) | warning | Waiting for database connections |
| [JupiterRequestsUnfinished](JupiterRequestsUnfinished.md) | warning | Requests stopped between phases |
| [JupiterTaskFailing](JupiterTaskFailing.md) | warning | A background task failing |
| [JupiterTaskStalled](JupiterTaskStalled.md) | warning | A background task not succeeding |
| [JupiterJobsBacklog](JupiterJobsBacklog.md) | warning | The job queue keeping up |
| [JupiterJobsDiscarded](JupiterJobsDiscarded.md) | warning | Webhook deliveries given up |
