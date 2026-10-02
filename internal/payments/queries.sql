-- name: InsertIntent :exec
INSERT INTO payments.intents (id, merchant_id, livemode, amount, currency, capture_method, status, payment_method,
                              description, installments, installments_financed_by, setup_future_usage,
                              request_three_d_secure, pix_options, split, boleto_options, created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @amount, @currency, @capture_method, @status, @payment_method, @description,
        sqlc.narg('installments'), sqlc.narg('installments_financed_by'), @setup_future_usage, @request_three_d_secure,
        @pix_options, @split, @boleto_options, @created_at, @created_at);

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
    risk_decision = @risk_decision, risk_decision_id = @risk_decision_id, pix_options = @pix_options, split = @split, boleto_options = @boleto_options,
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

-- name: GetCardLedgerAccounts :many
-- A merchant's accounts and the platform's, in one read.
SELECT merchant_id, role, account_id FROM payments.ledger_accounts
WHERE merchant_id IN (@merchant_id::text, '') AND livemode = @livemode AND currency = @currency;

-- name: InsertLedgerAccount :exec
INSERT INTO payments.ledger_accounts (merchant_id, livemode, currency, role, account_id) VALUES ($1, $2, $3, $4, $5);

-- name: LockLedgerAccountCreation :exec
SELECT pg_advisory_xact_lock(hashtext('payments.ledger_accounts/' || @scope::text));

-- name: MerchantLedgerAccounts :many
SELECT merchant_id, livemode, currency, account_id FROM payments.ledger_accounts
WHERE role = 'merchant_balance' ORDER BY merchant_id, livemode, currency;

-- name: MerchantTotals :many
-- What each merchant's balance should hold: posted, what payments received less what
-- was refunded, Jupiter's fees net of what refunds gave back, what was paid out, and what
-- disputes took net of what recipients gave back for them; held for the merchant, what
-- open card authorizations hold; held against the merchant, what payouts in flight and
-- MED claims under analysis hold. Disputed is what disputes took, the most a chargeback
-- can leave the balance below zero.
WITH received AS (
    SELECT i.merchant_id, i.livemode, i.currency,
           sum(i.amount_received - i.amount_refunded
               - coalesce((SELECT sum(a.fee + a.split_out) FROM payments.attempts a WHERE a.intent_id = i.id), 0)
               + coalesce((SELECT sum(r.fee_returned + r.split_back) FROM payments.refunds r WHERE r.intent_id = i.id AND r.status = 'succeeded'), 0)) AS posted
    FROM payments.intents i GROUP BY i.merchant_id, i.livemode, i.currency
), held AS (
    SELECT i.merchant_id, i.livemode, i.currency, sum(a.amount) AS held
    FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
    WHERE a.ledger_hold IS NOT NULL AND a.status IN ('authorized', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
    GROUP BY i.merchant_id, i.livemode, i.currency
), paid_out AS (
    SELECT merchant_id, livemode, currency,
           coalesce(sum(amount) FILTER (WHERE status = 'paid'), 0) AS paid,
           coalesce(sum(amount) FILTER (WHERE status IN ('held', 'sending', 'unknown')), 0) AS in_flight
    FROM payments.payouts WHERE recipient_id = '' GROUP BY merchant_id, livemode, currency
), disputed AS (
    SELECT merchant_id, livemode, currency,
           coalesce(sum(amount - split_back) FILTER (WHERE status = 'withdrawn'), 0) AS withdrawn,
           coalesce(sum(amount) FILTER (WHERE status = 'held'), 0) AS held
    FROM payments.dispute_funds GROUP BY merchant_id, livemode, currency
)
SELECT r.merchant_id, r.livemode, r.currency,
       (r.posted - coalesce(p.paid, 0) - coalesce(d.withdrawn, 0))::bigint AS posted,
       coalesce(h.held, 0)::bigint AS held,
       (coalesce(p.in_flight, 0) + coalesce(d.held, 0))::bigint AS paying_out,
       coalesce(d.withdrawn, 0)::bigint AS disputed
FROM received r
LEFT JOIN held h ON h.merchant_id = r.merchant_id AND h.livemode = r.livemode AND h.currency = r.currency
LEFT JOIN paid_out p ON p.merchant_id = r.merchant_id AND p.livemode = r.livemode AND p.currency = r.currency
LEFT JOIN disputed d ON d.merchant_id = r.merchant_id AND d.livemode = r.livemode AND d.currency = r.currency;

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
                              recipient_id, method, bank_ispb, bank_branch, bank_account, holder_name, holder_tax_id,
                              scheduled_on, source_account, created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @amount, @currency, @pix_key, @description, @status, @ledger_hold,
        @recipient_id, @method, @bank_ispb, @bank_branch, @bank_account, @holder_name, @holder_tax_id,
        sqlc.narg('scheduled_on'), @source_account, @now, @now);

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
    arrived_at = @arrived_at, returned_at = @returned_at, updated_at = @updated_at
WHERE id = @id;

-- name: PayoutsToResolve :many
-- Payouts whose transfer has no answer yet, and bank transfers paid in the last week,
-- which the receiving bank may still send back.
SELECT id FROM payments.payouts
WHERE (status IN ('sending', 'unknown') AND updated_at <= @before::timestamptz)
   OR (status = 'paid' AND method = 'bank_transfer' AND arrived_at >= @watch_since::timestamptz AND updated_at <= @before::timestamptz)
ORDER BY updated_at, id
LIMIT @max_count::integer;

-- name: ScheduledPayoutMade :one
SELECT EXISTS (SELECT 1 FROM payments.payouts WHERE merchant_id = @merchant_id AND livemode = @livemode
                                               AND recipient_id = @recipient_id AND scheduled_on = @scheduled_on)::boolean;

-- name: TouchPayout :exec
UPDATE payments.payouts SET updated_at = @updated_at WHERE id = @id;

-- name: HeldPayouts :many
SELECT * FROM payments.payouts
WHERE status = 'held' AND (recipient_id = @recipient_id
    OR (@own::boolean AND recipient_id = '' AND merchant_id = @merchant_id AND livemode = @livemode))
ORDER BY id
FOR UPDATE;

-- name: PayoutsOfTheDay :one
-- What a balance's payouts asked for since the day began took, but those that failed or
-- came back; scheduled payouts are not counted.
SELECT coalesce(sum(amount), 0)::bigint FROM payments.payouts
WHERE merchant_id = @merchant_id AND livemode = @livemode AND recipient_id = @recipient_id AND currency = @currency
  AND created_at >= @since AND scheduled_on IS NULL AND status NOT IN ('failed', 'returned');

-- name: LockPayouts :exec
-- Serializes payouts from one balance, so two cannot both spend what is available once.
SELECT pg_advisory_xact_lock(hashtext('payments.payouts/' || @scope::text));

-- name: RefundsInFlight :one
-- What refunds not yet confirmed will take from a merchant's balance.
SELECT coalesce(sum(amount), 0)::bigint FROM payments.refunds
WHERE merchant_id = @merchant_id AND livemode = @livemode AND currency = @currency AND status IN ('pending', 'refund_unknown');

-- name: SetAttemptFee :exec
UPDATE payments.attempts SET fee = @fee WHERE id = @id;

-- name: SetRefundFee :exec
UPDATE payments.refunds SET fee_returned = @fee_returned WHERE id = @id;

-- name: SetSplitOut :exec
UPDATE payments.attempts SET split_out = @split_out WHERE id = @id;

-- name: SetSplitBack :exec
UPDATE payments.refunds SET split_back = @split_back WHERE id = @id;

-- name: FeesReturned :one
SELECT coalesce(sum(fee_returned), 0)::bigint FROM payments.refunds WHERE intent_id = @intent_id AND status = 'succeeded';

-- name: FeeTotals :many
-- What each mode's fee account should hold: the fees charged less what refunds gave back.
SELECT i.livemode, i.currency,
       (coalesce(sum(a.fee), 0) - coalesce((SELECT sum(r.fee_returned) FROM payments.refunds r
            JOIN payments.intents ri ON ri.id = r.intent_id
            WHERE r.status = 'succeeded' AND ri.livemode = i.livemode AND ri.currency = i.currency), 0))::bigint AS held
FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
GROUP BY i.livemode, i.currency;

-- name: FeeLedgerAccounts :many
SELECT livemode, currency, account_id FROM payments.ledger_accounts WHERE merchant_id = '' AND role = 'card_fees';

-- name: CapturedAttemptByNetworkID :one
-- The captured attempt a card network or the Pix bank names a payment by: its network
-- transaction id, or its end-to-end id.
SELECT a.* FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
WHERE a.network_transaction_id = @network_id AND i.livemode = @livemode AND a.status = 'captured'
ORDER BY a.created_at DESC
LIMIT 1;

-- name: InsertDisputeFunds :execrows
INSERT INTO payments.dispute_funds (reference, intent_id, attempt_id, merchant_id, livemode, currency, amount, status, ledger_hold, ledger_txn, created_at, updated_at, withdrawn_at)
VALUES (@reference, @intent_id, @attempt_id, @merchant_id, @livemode, @currency, @amount, @status, @ledger_hold, @ledger_txn, @now, @now,
        CASE WHEN @status = 'withdrawn' THEN @now::timestamptz END)
ON CONFLICT (reference) DO NOTHING;

-- name: LockDisputeFunds :one
SELECT * FROM payments.dispute_funds WHERE reference = @reference FOR UPDATE;

-- name: SaveDisputeFunds :exec
UPDATE payments.dispute_funds
SET status = @status, split_back = @split_back, ledger_txn = @ledger_txn, updated_at = @now,
    withdrawn_at = CASE WHEN @status = 'withdrawn' AND withdrawn_at IS NULL THEN @now ELSE withdrawn_at END
WHERE reference = @reference;

-- name: DisputedAmount :one
-- What disputes hold or took of a payment: not refundable.
SELECT coalesce(sum(amount), 0)::bigint FROM payments.dispute_funds
WHERE intent_id = @intent_id AND status IN ('held', 'withdrawn');

-- name: PaidOutSince :many
-- The merchant's own payouts paid since a moment, oldest first: where its balance went.
SELECT id, amount, e2e_id, method, created_at FROM payments.payouts
WHERE merchant_id = @merchant_id AND livemode = @livemode AND currency = @currency AND recipient_id = ''
  AND status = 'paid' AND created_at >= @since
ORDER BY created_at, id
LIMIT 100;

-- name: CardAttemptsBetween :one
-- A merchant's card payments captured in a period, by when they were authorized.
SELECT count(*)::bigint FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
WHERE i.merchant_id = @merchant_id AND i.livemode = @livemode AND a.status = 'captured'
  AND a.payment_method NOT IN ('pix', 'boleto') AND a.created_at >= @from_time AND a.created_at < @to_time;

-- name: PixReceivedOf :one
SELECT * FROM payments.pix_received WHERE livemode = @livemode AND e2e_id = @e2e_id;

-- name: AllDisputeFunds :many
SELECT reference, status, amount FROM payments.dispute_funds ORDER BY reference;

-- name: MerchantsOfObjects :many
-- The merchants attempts and refunds belong to.
SELECT a.id::text AS object, i.merchant_id::text AS merchant_id FROM payments.attempts a JOIN payments.intents i ON i.id = a.intent_id
WHERE a.id = ANY(@ids::text[])
UNION ALL
SELECT r.id::text, r.merchant_id::text FROM payments.refunds r WHERE r.id = ANY(@ids::text[]);

-- name: CapturedAttempts :many
-- Which of some attempts were captured: their money booked.
SELECT id FROM payments.attempts WHERE id = ANY(@ids::text[]) AND status = 'captured';

-- name: PixReceivedSince :many
SELECT r.e2e_id, r.amount, r.received_at, r.status, r.updated_at, coalesce(i.merchant_id, '')::text AS merchant_id
FROM payments.pix_received r
LEFT JOIN payments.attempts a ON a.id = r.attempt_id
LEFT JOIN payments.intents i ON i.id = a.intent_id
WHERE r.livemode = @livemode AND (r.received_at >= @since OR r.updated_at >= @since)
ORDER BY r.received_at, r.e2e_id;

-- name: PixRefundsSince :many
-- Refunds of Pix payments the bank returned.
SELECT r.id, r.merchant_id, r.amount, r.updated_at FROM payments.refunds r JOIN payments.attempts a ON a.id = r.attempt_id
WHERE r.livemode = @livemode AND r.status = 'succeeded' AND a.payment_method = 'pix' AND r.updated_at >= @since
ORDER BY r.updated_at, r.id;

-- name: PayoutsSince :many
-- Payouts paid, or paid and returned, by a method, since a moment.
SELECT id, merchant_id, amount, status, arrived_at, returned_at FROM payments.payouts
WHERE livemode = @livemode AND method = @method AND status IN ('paid', 'returned') AND (arrived_at >= @since OR returned_at >= @since)
ORDER BY arrived_at, id;

-- name: PixDisputeFundsSince :many
-- What MED claims took from Pix payments, and gave back.
SELECT f.reference, f.merchant_id, f.amount, f.status, f.withdrawn_at, f.updated_at FROM payments.dispute_funds f
JOIN payments.attempts a ON a.id = f.attempt_id
WHERE f.livemode = @livemode AND a.payment_method = 'pix' AND f.status IN ('withdrawn', 'reinstated') AND f.withdrawn_at >= @since
ORDER BY f.withdrawn_at, f.reference;

-- name: Health :one
-- What has waited too long for the resolver: attempts, refunds and payouts whose outcome
-- is not final, and Pix received that paid nothing and are not returned. Payouts held for
-- an operator, and balances whose payouts today reached 80% of the daily limit.
SELECT
    (SELECT count(*) FROM payments.attempts
     WHERE status IN ('authenticating', 'authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown')
       AND updated_at < @attempts_before::timestamptz)::bigint AS attempts_unresolved,
    (SELECT count(*) FROM payments.refunds WHERE status IN ('pending', 'refund_unknown') AND updated_at < @attempts_before::timestamptz)::bigint AS refunds_unresolved,
    (SELECT count(*) FROM payments.payouts WHERE status IN ('sending', 'unknown') AND updated_at < @payouts_before::timestamptz)::bigint AS payouts_unresolved,
    (SELECT count(*) FROM payments.pix_received WHERE status IN ('unmatched', 'returning') AND updated_at < @payouts_before::timestamptz)::bigint AS pix_unreturned,
    (SELECT count(*) FROM payments.payouts WHERE status = 'held')::bigint AS payouts_held,
    (SELECT count(*) FROM (
        SELECT 1 FROM payments.payouts p
        WHERE p.created_at >= @day_began::timestamptz AND p.scheduled_on IS NULL AND p.status NOT IN ('failed', 'returned')
        GROUP BY p.merchant_id, p.livemode, p.recipient_id, p.currency
        HAVING sum(p.amount) >= @near_daily_limit::bigint
    ) near)::bigint AS balances_near_daily_limit;
