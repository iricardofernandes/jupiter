-- name: InsertCard :one
-- A repeated request key inserts nothing and returns no row; the caller then reads the
-- card the first request stored.
INSERT INTO vault.cards (
    token, owner, request_key, publishable_key, claim_expires_at, fingerprint, brand, bin, last4,
    exp_month, exp_year, encrypted_number, wrapped_key, key_id, client_ip, created_at
) VALUES (
    @token, sqlc.narg('owner'), sqlc.narg('request_key'), sqlc.narg('publishable_key'), sqlc.narg('claim_expires_at'),
    @fingerprint, @brand, @bin, @last4, @exp_month, @exp_year, @encrypted_number, @wrapped_key, @key_id, @client_ip, @created_at
)
ON CONFLICT (owner, request_key) DO NOTHING
RETURNING *;

-- name: CardByRequestKey :one
SELECT * FROM vault.cards WHERE owner = @owner AND request_key = @request_key;

-- name: GetCard :one
SELECT * FROM vault.cards WHERE token = @token;

-- name: ClaimCard :one
UPDATE vault.cards SET owner = @owner, claim_expires_at = NULL
WHERE token = @token AND owner IS NULL AND claim_expires_at > @now
RETURNING *;

-- name: CardsToRewrap :many
SELECT token, wrapped_key, key_id FROM vault.cards
WHERE key_id <> @active_key
ORDER BY token
LIMIT @max_count
FOR UPDATE SKIP LOCKED;

-- name: RewrapCard :exec
UPDATE vault.cards SET wrapped_key = @wrapped_key, key_id = @key_id WHERE token = @token;

-- name: KeyUsage :many
SELECT key_id, count(*)::bigint AS cards FROM vault.cards GROUP BY key_id ORDER BY key_id;

-- name: CountCardsUnderKey :one
SELECT count(*)::bigint FROM vault.cards WHERE key_id = @key_id;

-- name: PurgeUnclaimed :execrows
-- A batch at a time, so that a backlog cannot outlast the statement timeout.
DELETE FROM vault.cards WHERE token IN (
    SELECT c.token FROM vault.cards c WHERE c.owner IS NULL AND c.claim_expires_at <= @now::timestamptz LIMIT @max_count::integer
);

-- name: StoreNetworkToken :execrows
UPDATE vault.cards
SET network_token = @network_token, network_token_month = @network_token_month, network_token_year = @network_token_year,
    network_token_reference = @network_token_reference
WHERE token = @token AND owner = @owner;
