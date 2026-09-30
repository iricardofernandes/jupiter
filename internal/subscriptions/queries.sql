-- name: InsertSubscription :one
INSERT INTO subscriptions.subscriptions (id, merchant_id, livemode, amount, currency, interval, start_date, end_date,
    description, customer_name, customer_tax_id, retries, authorization_method, payer_ispb, payer_branch, payer_account,
    status, created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @amount, @currency, @interval, @start_date, sqlc.narg('end_date'), @description,
    @customer_name, @customer_tax_id, @retries, @authorization_method, @payer_ispb, @payer_branch, @payer_account,
    'incomplete', @now, @now)
RETURNING *;

-- name: GetSubscription :one
SELECT * FROM subscriptions.subscriptions WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: GetSubscriptionByID :one
SELECT * FROM subscriptions.subscriptions WHERE id = @id;

-- name: LockSubscription :one
SELECT * FROM subscriptions.subscriptions WHERE id = @id FOR UPDATE;

-- name: SubscriptionByRecurrence :one
SELECT * FROM subscriptions.subscriptions WHERE recurrence_id = @recurrence_id AND livemode = @livemode;

-- name: ListSubscriptions :many
SELECT * FROM subscriptions.subscriptions
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: SaveSubscription :exec
UPDATE subscriptions.subscriptions
SET status = @status, set_up = @set_up, recurrence_id = @recurrence_id, qr_code = @qr_code, canceled_by = @canceled_by,
    end_code = @end_code, updated_at = @updated_at
WHERE id = @id;

-- name: SubscriptionsToAdvance :many
-- Subscriptions still charging, and those ended with a charge still in flight.
SELECT s.id FROM subscriptions.subscriptions s
WHERE s.updated_at <= @before::timestamptz
  AND (s.status IN ('incomplete', 'active', 'past_due')
       OR EXISTS (SELECT 1 FROM subscriptions.cycles c WHERE c.subscription_id = s.id AND c.status = 'pending'))
ORDER BY s.updated_at, s.id
LIMIT @max_count::integer;

-- name: TryLockSubscription :one
-- Held by whoever advances or sets up a subscription, for as long as it talks to the bank.
SELECT pg_try_advisory_lock(hashtext('subscriptions/' || @id::text));

-- name: UnlockSubscription :exec
SELECT pg_advisory_unlock(hashtext('subscriptions/' || @id::text));

-- name: MarkSetupAttempt :exec
UPDATE subscriptions.subscriptions SET setup_attempted_at = @at WHERE id = @id;

-- name: IncompleteFor :one
-- Subscriptions of a merchant waiting for the same customer's authorization.
SELECT count(*) FROM subscriptions.subscriptions
WHERE merchant_id = @merchant_id AND livemode = @livemode AND customer_tax_id = @customer_tax_id AND status = 'incomplete';

-- name: TouchSubscription :exec
UPDATE subscriptions.subscriptions SET updated_at = @updated_at WHERE id = @id;

-- name: Cycles :many
SELECT * FROM subscriptions.cycles WHERE subscription_id = @subscription_id ORDER BY number;

-- name: InsertCycle :exec
INSERT INTO subscriptions.cycles (subscription_id, number, due_date, payment_intent, status, created_at, updated_at)
VALUES (@subscription_id, @number, @due_date, @payment_intent, 'pending', @now, @now);

-- name: SetCycleStatus :exec
UPDATE subscriptions.cycles SET status = @status, updated_at = @updated_at
WHERE subscription_id = @subscription_id AND number = @number;
