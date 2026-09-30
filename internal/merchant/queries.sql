-- name: InsertMerchant :exec
INSERT INTO merchant.merchants (id, name, api_version, created_at) VALUES ($1, $2, $3, $4);

-- name: GetMerchant :one
SELECT id, name, api_version, created_at FROM merchant.merchants WHERE id = $1;

-- name: InsertKey :exec
INSERT INTO merchant.api_keys (id, merchant_id, livemode, kind, name, hash, last4, scopes, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: AuthenticateKey :one
SELECT k.id, k.merchant_id, k.livemode, k.kind, k.scopes, m.api_version
FROM merchant.api_keys k
JOIN merchant.merchants m ON m.id = k.merchant_id
WHERE k.hash = $1 AND k.revoked_at IS NULL;

-- name: GetKey :one
SELECT id, merchant_id, livemode, kind, name, last4, scopes, created_at, revoked_at
FROM merchant.api_keys
WHERE id = $1 AND merchant_id = $2 AND livemode = $3;

-- Newest first. With ending_before, rows come oldest first and the caller reverses them.
-- name: ListKeys :many
SELECT id, merchant_id, livemode, kind, name, last4, scopes, created_at, revoked_at
FROM merchant.api_keys
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: RevokeKey :one
UPDATE merchant.api_keys SET revoked_at = @now::timestamptz
WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode AND revoked_at IS NULL
RETURNING id, merchant_id, livemode, kind, name, last4, scopes, created_at, revoked_at;
