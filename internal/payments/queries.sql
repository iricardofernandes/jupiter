-- name: InsertIntent :exec
INSERT INTO payments.intents (id, merchant_id, livemode, amount, currency, capture_method, status, payment_method,
                              description, installments, installments_financed_by, setup_future_usage,
                              request_three_d_secure, pix_options, created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @amount, @currency, @capture_method, @status, @payment_method, @description,
        sqlc.narg('installments'), sqlc.narg('installments_financed_by'), @setup_future_usage, @request_three_d_secure,
        @pix_options, @created_at, @created_at);

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
    cancellation_reason = @cancellation_reason, installments = sqlc.narg('installments'),
    installments_financed_by = sqlc.narg('installments_financed_by'), setup_future_usage = @setup_future_usage,
    request_three_d_secure = @request_three_d_secure, next_action_url = @next_action_url,
    risk_decision = @risk_decision, risk_decision_id = @risk_decision_id, pix_options = @pix_options,
    next_action_data = @next_action_data, next_action_expires_at = @next_action_expires_at, updated_at = @updated_at
WHERE id = @id;

-- name: InsertAttempt :exec
INSERT INTO payments.attempts (id, intent_id, number, payment_method, amount, status, initiator, stores_credential,
                              installments, installments_financed_by, ip, created_at, updated_at)
VALUES (@id, @intent_id, @number, @payment_method, @amount, @status, @initiator, @stores_credential,
        sqlc.narg('installments'), sqlc.narg('installments_financed_by'), @ip, @created_at, @created_at);

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
    unknown_since = @unknown_since, resolutions = @resolutions, network_transaction_id = @network_transaction_id,
    cleared_on = @cleared_on, amount_cleared = @amount_cleared, risk_decision = @risk_decision,
    risk_decision_id = @risk_decision_id, three_ds_server_trans_id = @three_ds_server_trans_id,
    three_ds_version = @three_ds_version, three_ds_status = @three_ds_status, ds_trans_id = @ds_trans_id,
    acs_trans_id = @acs_trans_id, acs_url = @acs_url, eci = @eci, authentication_value = @authentication_value,
    liability_shift = @liability_shift, updated_at = @updated_at
WHERE id = @id;

-- name: AttemptsToResolve :many
SELECT id FROM payments.attempts
WHERE status IN ('authenticating', 'authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
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
-- What each merchant's balance should hold: posted, what payments received less what
-- was refunded and paid out; held for the merchant, what open card authorizations hold;
-- held against the merchant, what payouts in flight hold.
WITH received AS (
    SELECT merchant_id, livemode, currency, sum(amount_received - amount_refunded) AS posted
    FROM payments.intents GROUP BY merchant_id, livemode, currency
), held AS (
    SELECT i.merchant_id, i.livemode, i.currency, sum(a.amount) AS held
    FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
    WHERE a.ledger_hold IS NOT NULL AND a.status IN ('authorized', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
    GROUP BY i.merchant_id, i.livemode, i.currency
), paid_out AS (
    SELECT merchant_id, livemode, currency,
           coalesce(sum(amount) FILTER (WHERE status = 'paid'), 0) AS paid,
           coalesce(sum(amount) FILTER (WHERE status IN ('sending', 'unknown')), 0) AS in_flight
    FROM payments.payouts GROUP BY merchant_id, livemode, currency
)
SELECT r.merchant_id, r.livemode, r.currency,
       (r.posted - coalesce(p.paid, 0))::bigint AS posted,
       coalesce(h.held, 0)::bigint AS held,
       coalesce(p.in_flight, 0)::bigint AS paying_out
FROM received r
LEFT JOIN held h ON h.merchant_id = r.merchant_id AND h.livemode = r.livemode AND h.currency = r.currency
LEFT JOIN paid_out p ON p.merchant_id = r.merchant_id AND p.livemode = r.livemode AND p.currency = r.currency;

-- An intent's status must agree with its latest attempt and its amounts. Statuses that
-- need an attempt fail the check when it is missing: coalesce turns NULL into false.
-- name: IntentInconsistencies :many
SELECT i.id, i.status, coalesce(a.status, '') AS attempt_status
FROM payments.intents i
LEFT JOIN payments.attempts a ON a.id = i.latest_attempt
WHERE NOT CASE i.status
    WHEN 'requires_capture' THEN coalesce(a.status = 'authorized' AND i.amount_capturable = a.amount, false)
    WHEN 'succeeded' THEN coalesce(a.status = 'captured' AND i.amount_received = a.amount_captured, false)
    WHEN 'processing' THEN coalesce(a.status IN ('authenticating', 'authorizing', 'authorization_unknown', 'scheduled', 'capturing', 'capture_unknown', 'voiding', 'void_unknown'), false)
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

-- name: InsertPaymentMethod :one
INSERT INTO payments.payment_methods (id, merchant_id, livemode, type, vault_token, brand, bin, last4, exp_month,
                                      exp_year, vault_fingerprint, created_at)
VALUES (@id, @merchant_id, @livemode, 'card', @vault_token, @brand, @bin, @last4, @exp_month, @exp_year,
        @vault_fingerprint, @created_at)
ON CONFLICT (vault_token) DO NOTHING
RETURNING *;

-- name: GetPaymentMethod :one
SELECT * FROM payments.payment_methods WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: GetPaymentMethodByToken :one
SELECT * FROM payments.payment_methods WHERE vault_token = @vault_token;

-- name: SetPaymentMethodNetworkTransaction :exec
UPDATE payments.payment_methods SET network_transaction_id = @network_transaction_id
WHERE id = @id AND network_transaction_id = '';

-- name: ClearRefund :execrows
UPDATE payments.refunds SET cleared_on = @cleared_on
WHERE id = @id AND status = 'succeeded' AND amount = @amount AND cleared_on IS NULL;

-- name: AttemptByServerTransaction :one
SELECT * FROM payments.attempts WHERE three_ds_server_trans_id = @three_ds_server_trans_id;

-- name: ClaimPaymentMethodsToTokenize :many
-- Live cards without a network token yet, or whose provisioning stopped half-way.
UPDATE payments.payment_methods SET network_token_status = 'provisioning', network_token_since = @now
WHERE id IN (
    SELECT c.id FROM payments.payment_methods c
    WHERE c.livemode AND (c.network_token_status = '' OR (c.network_token_status = 'provisioning' AND c.network_token_since < @stale_before))
    ORDER BY c.id
    LIMIT @max_count
    FOR UPDATE SKIP LOCKED
)
RETURNING id, merchant_id, livemode, vault_token;

-- name: SetNetworkToken :exec
UPDATE payments.payment_methods
SET network_token_reference = @network_token_reference, network_token_status = @network_token_status, network_token_since = @now
WHERE id = @id AND network_token_status = 'provisioning' AND network_token_since = @claimed_at;

-- name: UpdateCardFromNetworkToken :execrows
UPDATE payments.payment_methods
SET last4 = @last4, exp_month = @exp_month, exp_year = @exp_year, network_token_status = @network_token_status,
    network_token_since = @now, network_token_event_at = @occurred_at
WHERE network_token_reference = @network_token_reference
  AND network_token_status IN ('active', 'suspended')
  AND (network_token_event_at IS NULL OR network_token_event_at <= @occurred_at);

-- name: InsertPixCharge :exec
INSERT INTO payments.pix_charges (txid, attempt_id, livemode, due, copy_paste, expires_at, created_at)
VALUES (@txid, @attempt_id, @livemode, @due, @copy_paste, @expires_at, @created_at)
ON CONFLICT (txid) DO NOTHING;

-- name: PixChargeByTxid :one
SELECT * FROM payments.pix_charges WHERE txid = @txid AND livemode = @livemode;

-- name: PixChargeByAttempt :one
SELECT * FROM payments.pix_charges WHERE attempt_id = @attempt_id;

-- name: PixAttemptsDue :many
-- Attempts waiting for a Pix whose charge expired more than a grace period ago.
SELECT a.id FROM payments.attempts a JOIN payments.pix_charges c ON c.attempt_id = a.id
WHERE a.status = 'requires_action' AND c.expires_at <= @before::timestamptz
ORDER BY c.expires_at, a.id
LIMIT @max_count::integer;

-- name: InsertPixReceived :one
INSERT INTO payments.pix_received (livemode, e2e_id, txid, amount, currency, received_at, status, created_at, updated_at)
VALUES (@livemode, @e2e_id, @txid, @amount, @currency, @received_at, 'unmatched', @now, @now)
ON CONFLICT (livemode, e2e_id) DO NOTHING
RETURNING *;

-- name: LockPixReceived :one
SELECT * FROM payments.pix_received WHERE livemode = @livemode AND e2e_id = @e2e_id FOR UPDATE;

-- name: SavePixReceived :exec
UPDATE payments.pix_received
SET status = @status, attempt_id = sqlc.narg('attempt_id'), ledger_txn = sqlc.narg('ledger_txn'),
    return_txn = sqlc.narg('return_txn'), reason = @reason, updated_at = @updated_at
WHERE livemode = @livemode AND e2e_id = @e2e_id;

-- name: PixReceivedToReturn :many
SELECT livemode, e2e_id FROM payments.pix_received
WHERE status IN ('unmatched', 'returning') AND updated_at <= @before::timestamptz
ORDER BY updated_at, e2e_id
LIMIT @max_count::integer;

-- name: UnmatchedPixTotals :many
SELECT livemode, currency, coalesce(sum(amount), 0)::bigint AS held
FROM payments.pix_received WHERE status IN ('unmatched', 'returning', 'return_failed')
GROUP BY livemode, currency;

-- name: PixLedgerAccounts :many
SELECT livemode, currency, account_id FROM payments.ledger_accounts WHERE role = 'pix_unmatched';

-- name: InsertPayout :exec
INSERT INTO payments.payouts (id, merchant_id, livemode, amount, currency, pix_key, description, status, ledger_hold,
                              created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @amount, @currency, @pix_key, @description, 'sending', @ledger_hold, @now, @now);

-- name: GetPayout :one
SELECT * FROM payments.payouts WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: GetPayoutByID :one
SELECT * FROM payments.payouts WHERE id = @id;

-- name: LockPayoutByID :one
SELECT * FROM payments.payouts WHERE id = @id FOR UPDATE;

-- name: ListPayouts :many
SELECT * FROM payments.payouts
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: SavePayout :exec
UPDATE payments.payouts
SET status = @status, failure_code = @failure_code, failure_message = @failure_message, e2e_id = @e2e_id,
    recipient_name = @recipient_name, unknown_since = @unknown_since, resolutions = @resolutions,
    arrived_at = @arrived_at, updated_at = @updated_at
WHERE id = @id;

-- name: PayoutsToResolve :many
SELECT id FROM payments.payouts
WHERE status IN ('sending', 'unknown') AND updated_at <= @before::timestamptz
ORDER BY updated_at, id
LIMIT @max_count::integer;

-- name: LockPayouts :exec
-- Serializes payouts from one balance, so two cannot both spend what is available once.
SELECT pg_advisory_xact_lock(hashtext('payments.payouts/' || @scope::text));

-- name: RefundsInFlight :one
-- What refunds not yet confirmed will take from a merchant's balance.
SELECT coalesce(sum(amount), 0)::bigint FROM payments.refunds
WHERE merchant_id = @merchant_id AND livemode = @livemode AND currency = @currency AND status IN ('pending', 'refund_unknown');
