-- name: NextSTAN :one
SELECT lpad(nextval('acquirer.stan')::text, 6, '0')::text;

-- name: NextRRN :one
SELECT lpad(nextval('acquirer.rrn')::text, 8, '0')::text;

-- name: InsertExchange :one
INSERT INTO acquirer.exchanges (key, kind, authorization_key, mti, stan, rrn, transmitted_at, amount, installments,
                                merchant_code, state, created_at, updated_at)
VALUES (@key, @kind, sqlc.narg('authorization_key'), @mti, @stan, @rrn, @transmitted_at, @amount,
        sqlc.narg('installments'), @merchant_code, 'sending', @now, @now)
ON CONFLICT (key) DO NOTHING
RETURNING *;

-- name: GetExchange :one
SELECT * FROM acquirer.exchanges WHERE key = @key;

-- name: LockExchange :one
SELECT * FROM acquirer.exchanges WHERE key = @key FOR UPDATE;

-- name: ExchangeByRRN :one
SELECT * FROM acquirer.exchanges WHERE rrn = @rrn;

-- name: ExchangeByMessage :one
-- The exchange a message from the network answers: the original by STAN and RRN, or a
-- store-and-forward repeat by the STAN it was last sent with.
SELECT * FROM acquirer.exchanges
WHERE (stan = @stan AND rrn = @rrn) OR (forward_stan = @stan AND rrn = @rrn)
ORDER BY updated_at DESC
LIMIT 1;

-- name: DeleteExchange :exec
DELETE FROM acquirer.exchanges WHERE key = @key AND state = 'sending';

-- name: SaveExchange :exec
UPDATE acquirer.exchanges
SET state = @state, response_code = @response_code, authorization_code = @authorization_code,
    network_transaction_id = @network_transaction_id, decline_code = @decline_code,
    late_response_code = @late_response_code, forward_stan = sqlc.narg('forward_stan'),
    forward_attempts = @forward_attempts, next_forward_at = sqlc.narg('next_forward_at'), updated_at = @now
WHERE key = @key;

-- name: ExchangesToForward :many
SELECT * FROM acquirer.exchanges
WHERE next_forward_at IS NOT NULL AND next_forward_at <= @now
ORDER BY next_forward_at
LIMIT @max_count
FOR UPDATE SKIP LOCKED;

-- name: InsertClearingFile :one
INSERT INTO acquirer.clearing_files (business_date, sha256, records, imported_at)
VALUES (@business_date, @sha256, @records, @now)
ON CONFLICT (business_date) DO NOTHING
RETURNING business_date;

-- name: GetClearingFile :one
SELECT * FROM acquirer.clearing_files WHERE business_date = @business_date;

-- name: InsertClearingException :exec
INSERT INTO acquirer.clearing_exceptions (business_date, kind, rrn, network_transaction_id, amount, merchant_code, reason)
VALUES (@business_date, @kind, @rrn, @network_transaction_id, @amount, @merchant_code, @reason);

-- name: OpenClearingExceptions :many
SELECT * FROM acquirer.clearing_exceptions WHERE resolved_at IS NULL ORDER BY id LIMIT @max_count FOR UPDATE SKIP LOCKED;

-- name: ResolveClearingException :exec
UPDATE acquirer.clearing_exceptions SET resolved_at = @now WHERE id = @id;

-- name: SetClearingExceptionReason :exec
UPDATE acquirer.clearing_exceptions SET reason = @reason WHERE id = @id;

-- name: ClearingExceptions :many
SELECT * FROM acquirer.clearing_exceptions WHERE business_date = @business_date ORDER BY id;

-- name: InsertClearingRecord :exec
INSERT INTO acquirer.clearing_records (business_date, line, kind, rrn, network_transaction_id, amount, merchant_code)
VALUES (@business_date, @line, @kind, @rrn, @network_transaction_id, @amount, @merchant_code);

-- name: ClearingRecordsOn :many
SELECT * FROM acquirer.clearing_records WHERE business_date = @business_date ORDER BY line;

-- name: ClearedExchangesSince :many
-- The captures the network acknowledged and the refunds it approved since a moment: what
-- its clearing files must list.
SELECT key, kind, authorization_key, rrn, amount, created_at FROM acquirer.exchanges
WHERE created_at >= @since AND ((kind = 'capture' AND state = 'acknowledged') OR (kind = 'refund' AND state = 'approved'))
ORDER BY created_at, key
LIMIT 100000;

-- name: Health :one
-- Reversals and advices the network has not acknowledged in time, and clearing records
-- that match nothing.
SELECT
    (SELECT count(*) FROM acquirer.exchanges WHERE next_forward_at IS NOT NULL AND created_at < @before::timestamptz)::bigint AS forwards_pending,
    (SELECT count(*) FROM acquirer.clearing_exceptions WHERE resolved_at IS NULL)::bigint AS clearing_exceptions;
