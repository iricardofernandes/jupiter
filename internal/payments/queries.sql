-- name: InsertIntent :exec
INSERT INTO payments.intents (id, merchant_id, livemode, amount, currency, capture_method, status, payment_method,
                              description, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- name: GetIntent :one
SELECT * FROM payments.intents WHERE id = $1 AND merchant_id = $2 AND livemode = $3;

-- name: LockIntent :one
SELECT * FROM payments.intents WHERE id = $1 AND merchant_id = $2 AND livemode = $3 FOR UPDATE;

-- name: GetIntentByID :one
SELECT * FROM payments.intents WHERE id = $1;

-- name: GetRefundByID :one
SELECT * FROM payments.refunds WHERE id = $1;

-- name: LockIntentByID :one
SELECT * FROM payments.intents WHERE id = $1 FOR UPDATE;

-- name: ListIntents :many
SELECT * FROM payments.intents
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: SaveIntent :exec
UPDATE payments.intents
SET amount = @amount, status = @status, payment_method = @payment_method, description = @description,
    amount_capturable = @amount_capturable, amount_received = @amount_received, amount_refunded = @amount_refunded,
    latest_attempt = @latest_attempt, last_error_code = @last_error_code, last_decline_code = @last_decline_code,
    last_error_message = @last_error_message, next_action = @next_action,
    cancellation_reason = @cancellation_reason, updated_at = @updated_at
WHERE id = @id;

-- name: InsertAttempt :exec
INSERT INTO payments.attempts (id, intent_id, number, payment_method, amount, status, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7);

-- name: NextAttemptNumber :one
SELECT (coalesce(max(number), 0) + 1)::integer FROM payments.attempts WHERE intent_id = $1;

-- name: GetAttempt :one
SELECT * FROM payments.attempts WHERE id = $1;

-- name: LockAttempt :one
SELECT * FROM payments.attempts WHERE id = $1 FOR UPDATE;

-- name: SaveAttempt :exec
UPDATE payments.attempts
SET status = @status, authenticated = @authenticated, rail_reference = @rail_reference,
    decline_code = @decline_code, ledger_hold = @ledger_hold, capture_amount = @capture_amount,
    amount_captured = @amount_captured, authorization_expires_at = @authorization_expires_at,
    unknown_since = @unknown_since, resolutions = @resolutions, updated_at = @updated_at
WHERE id = @id;

-- name: AttemptsToResolve :many
SELECT id FROM payments.attempts
WHERE status IN ('authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
  AND updated_at <= @before::timestamptz
ORDER BY updated_at, id
LIMIT @max_count::integer;

-- name: AttemptsToExpire :many
SELECT id FROM payments.attempts
WHERE status = 'authorized' AND authorization_expires_at <= @now::timestamptz
ORDER BY authorization_expires_at, id
LIMIT @max_count::integer;

-- name: InsertRefund :exec
INSERT INTO payments.refunds (id, intent_id, attempt_id, merchant_id, livemode, amount, currency, reason, status,
                              created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending', $9, $9);

-- name: GetRefund :one
SELECT * FROM payments.refunds WHERE id = $1 AND merchant_id = $2 AND livemode = $3;

-- name: LockRefundByID :one
SELECT * FROM payments.refunds WHERE id = $1 FOR UPDATE;

-- name: SaveRefund :exec
UPDATE payments.refunds
SET status = @status, rail_reference = @rail_reference, failure_reason = @failure_reason, ledger_txn = @ledger_txn,
    unknown_since = @unknown_since, resolutions = @resolutions, updated_at = @updated_at
WHERE id = @id;

-- name: TouchRefund :exec
UPDATE payments.refunds SET resolutions = resolutions + 1, updated_at = @updated_at WHERE id = @id;

-- name: OutstandingRefunds :one
SELECT coalesce(sum(amount), 0)::bigint FROM payments.refunds
WHERE intent_id = $1 AND status IN ('pending', 'refund_unknown');

-- name: ListRefunds :many
SELECT * FROM payments.refunds
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@intent_id::text = '' OR intent_id = @intent_id)
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: RefundsToResolve :many
SELECT id FROM payments.refunds
WHERE status IN ('pending', 'refund_unknown') AND updated_at <= @before::timestamptz
ORDER BY updated_at, id
LIMIT @max_count::integer;

-- name: GetLedgerAccounts :many
SELECT role, account_id FROM payments.ledger_accounts
WHERE merchant_id = $1 AND livemode = $2 AND currency = $3;

-- name: InsertLedgerAccount :exec
INSERT INTO payments.ledger_accounts (merchant_id, livemode, currency, role, account_id) VALUES ($1, $2, $3, $4, $5);

-- name: LockLedgerAccountCreation :exec
SELECT pg_advisory_xact_lock(hashtext('payments.ledger_accounts/' || @scope::text));

-- name: MerchantLedgerAccounts :many
SELECT merchant_id, livemode, currency, account_id FROM payments.ledger_accounts
WHERE role = 'merchant_balance' ORDER BY merchant_id, livemode, currency;

-- name: MerchantTotals :many
SELECT merchant_id, livemode, currency,
       sum(amount_received - amount_refunded)::bigint AS posted,
       (SELECT coalesce(sum(a.amount), 0) FROM payments.attempts a
          JOIN payments.intents i2 ON i2.id = a.intent_id
         WHERE i2.merchant_id = i.merchant_id AND i2.livemode = i.livemode AND i2.currency = i.currency
           AND a.status IN ('authorized', 'capturing', 'capture_unknown', 'voiding', 'void_unknown'))::bigint AS held
FROM payments.intents i
GROUP BY merchant_id, livemode, currency;

-- An intent's status must agree with its latest attempt and its amounts. Statuses that
-- need an attempt fail the check when it is missing: coalesce turns NULL into false.
-- name: IntentInconsistencies :many
SELECT i.id, i.status, coalesce(a.status, '') AS attempt_status
FROM payments.intents i
LEFT JOIN payments.attempts a ON a.id = i.latest_attempt
WHERE NOT CASE i.status
    WHEN 'requires_capture' THEN coalesce(a.status = 'authorized' AND i.amount_capturable = a.amount, false)
    WHEN 'succeeded' THEN coalesce(a.status = 'captured' AND i.amount_received = a.amount_captured, false)
    WHEN 'processing' THEN coalesce(a.status IN ('authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown'), false)
    WHEN 'requires_action' THEN coalesce(a.status = 'requires_action', false)
    WHEN 'canceled' THEN a.status IS NULL OR a.status IN ('voided', 'declined', 'failed')
    ELSE a.status IS NULL OR a.status IN ('declined', 'failed')
  END
  OR i.amount_refunded <> (SELECT coalesce(sum(r.amount), 0) FROM payments.refunds r WHERE r.intent_id = i.id AND r.status = 'succeeded')
ORDER BY i.id;

-- name: GetRailOperation :one
SELECT * FROM payments.test_rail WHERE key = $1 FOR UPDATE;

-- name: InsertRailOperation :exec
INSERT INTO payments.test_rail (key, kind, authorization_key, amount, status, reference, detail, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: LockRailKey :exec
SELECT pg_advisory_xact_lock(hashtext('payments.test_rail/' || @key::text));

-- name: RailApprovedTotal :one
SELECT coalesce(sum(amount), 0)::bigint FROM payments.test_rail
WHERE authorization_key = @authorization_key AND kind = @kind AND status = 'approved';

-- name: SaveRailOperation :exec
UPDATE payments.test_rail SET status = $2, detail = $3, calls = $4, queries = $5 WHERE key = $1;
