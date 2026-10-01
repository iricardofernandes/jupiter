-- name: InsertEvent :many
-- Records an event and returns the endpoints subscribed to it.
WITH inserted AS (
    INSERT INTO events.events (id, merchant_id, livemode, type, object_id, object_type, created_at)
    VALUES (@id, @merchant_id, @livemode, @type, @object_id, @object_type, @created_at)
    RETURNING merchant_id, livemode, type
)
SELECT e.id FROM events.endpoints e JOIN inserted i ON e.merchant_id = i.merchant_id AND e.livemode = i.livemode
WHERE e.deleted_at IS NULL AND e.status = 'enabled' AND (i.type = ANY(e.enabled_events) OR '*' = ANY(e.enabled_events))
ORDER BY e.id;

-- name: GetEvent :one
SELECT * FROM events.events WHERE id = $1 AND merchant_id = $2 AND livemode = $3;

-- name: GetEventByID :one
SELECT * FROM events.events WHERE id = $1;

-- name: ListEvents :many
SELECT * FROM events.events
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@type::text = '' OR type = @type)
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: SubscribedEndpoints :many
SELECT id FROM events.endpoints
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND deleted_at IS NULL AND status = 'enabled'
  AND (@type::text = ANY(enabled_events) OR '*' = ANY(enabled_events))
ORDER BY id;

-- name: InsertEndpoint :exec
INSERT INTO events.endpoints (id, merchant_id, livemode, url, description, enabled_events, status, api_version, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetEndpoint :one
SELECT * FROM events.endpoints WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL;

-- name: LockEndpoint :one
SELECT * FROM events.endpoints WHERE id = $1 AND merchant_id = $2 AND livemode = $3 AND deleted_at IS NULL FOR UPDATE;

-- name: GetEndpointByID :one
SELECT * FROM events.endpoints WHERE id = $1;

-- name: ListEndpoints :many
SELECT * FROM events.endpoints
WHERE merchant_id = @merchant_id AND livemode = @livemode AND deleted_at IS NULL
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: UpdateEndpoint :one
UPDATE events.endpoints
SET url = @url, description = @description, enabled_events = @enabled_events, status = @status
WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode AND deleted_at IS NULL
RETURNING *;

-- name: DeleteEndpoint :one
UPDATE events.endpoints SET deleted_at = @now::timestamptz
WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode AND deleted_at IS NULL
RETURNING *;

-- name: InsertSecret :exec
INSERT INTO events.endpoint_secrets (id, endpoint_id, sealed, created_at) VALUES ($1, $2, $3, $4);

-- name: ExpireSecrets :exec
UPDATE events.endpoint_secrets
SET expires_at = least(coalesce(expires_at, @expires_at::timestamptz), @expires_at::timestamptz)
WHERE endpoint_id = @endpoint_id AND (expires_at IS NULL OR expires_at > @now::timestamptz);

-- name: ActiveSecrets :many
SELECT id, sealed FROM events.endpoint_secrets
WHERE endpoint_id = @endpoint_id AND (expires_at IS NULL OR expires_at > @now::timestamptz)
ORDER BY created_at DESC, id DESC;

-- name: InsertDelivery :exec
INSERT INTO events.deliveries (id, event_id, endpoint_id, attempt, succeeded, response_status, error, duration_ms, attempted_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListDeliveries :many
SELECT * FROM events.deliveries WHERE event_id = $1 ORDER BY attempted_at, id;
