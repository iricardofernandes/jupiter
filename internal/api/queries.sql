-- name: InsertIdempotencyKey :one
INSERT INTO api.idempotency_keys (
    merchant_id, livemode, key, fingerprint, operation, path_id, request_body, api_version,
    key_id, key_kind, key_scopes, recovery_point, lock_token, locked_at, created_at, last_run_at, request_sealed
) VALUES (
    @merchant_id, @livemode, @key, @fingerprint, @operation, @path_id, @request_body, @api_version,
    @key_id, @key_kind, @key_scopes, 'started', @lock_token, @now::timestamptz, @now::timestamptz, @now::timestamptz, true
)
ON CONFLICT (merchant_id, livemode, key) DO NOTHING
RETURNING id;

-- name: LockIdempotencyKeyRow :one
SELECT * FROM api.idempotency_keys
WHERE merchant_id = @merchant_id AND livemode = @livemode AND key = @key
FOR UPDATE;

-- name: LockIdempotencyKeyByID :one
SELECT * FROM api.idempotency_keys WHERE id = $1 FOR UPDATE;

-- name: TakeIdempotencyLock :exec
UPDATE api.idempotency_keys
SET lock_token = @lock_token, locked_at = @now::timestamptz, last_run_at = @now::timestamptz
WHERE id = @id;

-- name: CheckIdempotencyLock :one
SELECT recovery_point FROM api.idempotency_keys WHERE id = @id AND lock_token = @lock_token FOR UPDATE;

-- name: AdvanceIdempotencyKey :exec
UPDATE api.idempotency_keys SET recovery_point = @recovery_point, recovery_state = @recovery_state
WHERE id = @id AND lock_token = @lock_token;

-- name: FinishIdempotencyKey :execrows
UPDATE api.idempotency_keys
SET recovery_point = 'finished', response_status = @response_status, response_body = @response_body,
    lock_token = NULL, locked_at = NULL
WHERE id = @id AND lock_token = @lock_token;

-- name: ReleaseIdempotencyKey :exec
UPDATE api.idempotency_keys SET lock_token = NULL, locked_at = NULL WHERE id = @id AND lock_token = @lock_token;

-- Keys whose request committed some work and then stopped: the runner died while it
-- held the lock, or failed and released it. Keys still at 'started' did nothing and
-- are left for the client to retry.
-- name: AbandonedIdempotencyKeys :many
-- Each run of the completer waits longer before the next, up to an hour, so that a
-- request held up by an outage of a counterparty outlasts a day of it.
WITH stale AS (SELECT @stale_before::timestamptz AS before)
SELECT k.id FROM api.idempotency_keys k, stale
WHERE k.response_status IS NULL AND k.recovery_point <> 'started' AND k.completer_runs < @max_runs::integer
  AND ((k.locked_at IS NOT NULL AND k.locked_at < stale.before)
       OR (k.locked_at IS NULL AND k.last_run_at < stale.before))
  AND k.last_run_at < stale.before - least(power(2, k.completer_runs), 60) * interval '1 minute'
ORDER BY k.last_run_at
LIMIT @max_count::integer;

-- name: ReapIdempotencyKeys :execrows
DELETE FROM api.idempotency_keys
WHERE created_at < @created_before::timestamptz
  AND (response_status IS NOT NULL OR recovery_point = 'started')
  AND locked_at IS NULL;

-- name: ReleaseStaleStartedKeys :exec
UPDATE api.idempotency_keys SET lock_token = NULL, locked_at = NULL
WHERE response_status IS NULL AND recovery_point = 'started' AND locked_at < @stale_before::timestamptz;

-- name: Health :one
-- Requests that stopped between phases and have waited longer than the completer should
-- take to finish them.
SELECT count(*)::bigint FROM api.idempotency_keys WHERE response_status IS NULL AND last_run_at < @before::timestamptz;

-- name: CountCompleterRun :exec
UPDATE api.idempotency_keys SET completer_runs = completer_runs + 1 WHERE id = @id;
