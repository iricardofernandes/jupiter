-- name: InsertDispute :exec
INSERT INTO disputes.disputes (
    id, merchant_id, livemode, payment_intent, attempt_id, kind, network, network_id, network_transaction_id,
    reason, reason_code, amount, currency, stage, status, liability, due_by, network_due_by, authorized_at,
    notified_at, contested_at, pending_action, network_version, created_at, updated_at
) VALUES (
    @id, @merchant_id, @livemode, @payment_intent, @attempt_id, @kind, @network, @network_id, @network_transaction_id,
    @reason, @reason_code, @amount, @currency, @stage, @status, @liability, @due_by, @network_due_by, @authorized_at,
    @notified_at, @contested_at, @pending_action, @network_version, @now, @now
);

-- name: LockByNetworkID :one
SELECT * FROM disputes.disputes WHERE livemode = @livemode AND kind = @kind AND network_id = @network_id FOR UPDATE;

-- name: GetByNetworkID :one
SELECT * FROM disputes.disputes WHERE livemode = @livemode AND kind = @kind AND network_id = @network_id;

-- name: LockDispute :one
SELECT * FROM disputes.disputes WHERE id = @id FOR UPDATE;

-- name: GetDispute :one
SELECT * FROM disputes.disputes WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: GetDisputeByID :one
SELECT * FROM disputes.disputes WHERE id = @id;

-- name: SaveDispute :exec
UPDATE disputes.disputes
SET stage = @stage, status = @status, liability = @liability, evidence = @evidence, evidence_submitted_at = @evidence_submitted_at,
    due_by = @due_by, network_due_by = @network_due_by, blocked = @blocked, blocked_at = @blocked_at, trace = @trace,
    funds = @funds, pending_action = @pending_action, action_error = @action_error, action_retry_at = @action_retry_at,
    network_version = @network_version, outcome = @outcome, updated_at = @updated_at, closed_at = @closed_at
WHERE id = @id;

-- name: ListDisputes :many
SELECT * FROM disputes.disputes
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@payment_intent::text = '' OR payment_intent = @payment_intent)
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before::text <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: InsertHistory :exec
INSERT INTO disputes.history (dispute_id, at, kind, detail) VALUES (@dispute_id, @at, @kind, @detail);

-- name: HistoryOf :many
SELECT * FROM disputes.history WHERE dispute_id = @dispute_id ORDER BY id;

-- name: OverdueResponses :many
-- Disputes whose merchant's deadline passed with no answer.
SELECT id FROM disputes.disputes
WHERE status = 'needs_response' AND due_by < @now AND pending_action = ''
ORDER BY due_by, id
LIMIT 500;

-- name: LapsedAnalyses :many
-- MED claims still under analysis, with no answer yet, when their block ends.
SELECT id FROM disputes.disputes
WHERE kind = 'med' AND stage = 'med_analysis' AND status = 'under_review' AND network_due_by < @now AND pending_action = '' AND outcome = ''
ORDER BY network_due_by, id
LIMIT 500;

-- name: PendingActions :many
SELECT id FROM disputes.disputes
WHERE pending_action <> '' AND (action_retry_at IS NULL OR action_retry_at <= @now)
ORDER BY updated_at, id
LIMIT 500;

-- name: ToRefresh :many
-- Open disputes to ask the network or the bank about: past the deadline Jupiter has with
-- them, or quiet for a while.
SELECT id FROM disputes.disputes
WHERE pending_action = '' AND (status IN ('needs_response', 'under_review'))
  AND (network_due_by < @now OR updated_at < @quiet_since)
ORDER BY updated_at, id
LIMIT 500;

-- name: LateBlocks :many
-- MED claims whose funds were blocked later than the window after the bank's notice, or
-- not yet blocked past it.
SELECT id, notified_at, blocked_at FROM disputes.disputes
WHERE kind = 'med' AND blocked > 0 AND blocked_at > notified_at + make_interval(mins => @minutes::integer)
ORDER BY id
LIMIT 1000;

-- name: StuckDisputes :many
-- Disputes open past the network's deadline by more than a grace, or with an action the
-- network or bank has refused to take for that long.
SELECT id, status, pending_action, action_error FROM disputes.disputes
WHERE (status IN ('needs_response', 'under_review') AND network_due_by < @before)
   OR (pending_action <> '' AND updated_at < @before)
ORDER BY id
LIMIT 1000;

-- name: FundsOf :many
-- What each dispute says it held or took, for the check against payments.
SELECT id, funds, blocked, amount, kind FROM disputes.disputes WHERE funds <> 'none' ORDER BY id;

-- name: InsertFraudReport :execrows
INSERT INTO disputes.fraud_reports (id, merchant_id, livemode, payment_intent, network, network_id, fraud_type, amount, currency, reported_at, created_at)
VALUES (@id, @merchant_id, @livemode, @payment_intent, @network, @network_id, @fraud_type, @amount, @currency, @reported_at, @now)
ON CONFLICT (livemode, network_id) DO NOTHING;

-- name: GetFraudReportByNetworkID :one
SELECT * FROM disputes.fraud_reports WHERE livemode = @livemode AND network_id = @network_id;

-- name: ListFraudReports :many
SELECT * FROM disputes.fraud_reports
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before::text <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: MonthCounts :one
-- A merchant's card disputes opened and fraud reports received in a period.
SELECT
    (SELECT count(*) FROM disputes.disputes d WHERE d.merchant_id = @merchant_id AND d.livemode = @livemode AND d.kind = 'chargeback'
        AND d.created_at >= @from_time AND d.created_at < @to_time)::bigint AS disputes,
    (SELECT count(*) FROM disputes.fraud_reports f WHERE f.merchant_id = @merchant_id AND f.livemode = @livemode
        AND f.reported_at >= @from_time AND f.reported_at < @to_time)::bigint AS fraud_reports;

-- name: ActiveMerchants :many
-- Merchants with card disputes or fraud reports in a period: those the monitor looks at.
SELECT d.merchant_id, d.livemode FROM disputes.disputes d
WHERE d.kind = 'chargeback' AND d.created_at >= @from_time AND d.created_at < @to_time
UNION
SELECT f.merchant_id, f.livemode FROM disputes.fraud_reports f
WHERE f.reported_at >= @from_time AND f.reported_at < @to_time
ORDER BY 1, 2;

-- name: GetTestCase :one
SELECT * FROM disputes.test_cases WHERE id = @id FOR UPDATE;

-- name: InsertTestCase :exec
INSERT INTO disputes.test_cases (id, network_transaction_id, network, amount, currency, reason_code, stage, status, respond_by, authorized_at, opened_at, version)
VALUES (@id, @network_transaction_id, @network, @amount, @currency, @reason_code, @stage, @status, @respond_by, @authorized_at, @opened_at, 1);

-- name: SaveTestCase :exec
UPDATE disputes.test_cases
SET stage = @stage, status = @status, outcome = @outcome, respond_by = @respond_by, decide_by = @decide_by,
    escalated_evidence = @escalated_evidence, version = @version
WHERE id = @id;

-- name: Health :one
-- Disputes that need the merchant's answer before a deadline soon. The first condition
-- is the open disputes' index's.
SELECT count(*)::bigint FROM disputes.disputes
WHERE (status IN ('needs_response', 'under_review') OR pending_action <> '') AND status = 'needs_response' AND due_by < @soon::timestamptz;
