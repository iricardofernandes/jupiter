-- name: Velocity :one
SELECT
    count(*) FILTER (WHERE card_fingerprint = @card AND created_at > @hour_ago)::integer AS card_attempts_1h,
    count(*) FILTER (WHERE card_fingerprint = @card)::integer AS card_attempts_24h,
    count(*) FILTER (WHERE card_fingerprint = @card AND outcome = 'declined')::integer AS card_declines_24h
FROM risk.attempts
WHERE card_fingerprint = @card AND livemode = @livemode AND created_at > @day_ago;

-- name: IPVelocity :one
SELECT
    count(*) FILTER (WHERE created_at > @hour_ago)::integer AS ip_attempts_1h,
    count(DISTINCT card_fingerprint)::integer AS ip_cards_24h
FROM risk.attempts
WHERE ip = @ip AND merchant_id = @merchant_id AND livemode = @livemode AND created_at > @day_ago;

-- name: MerchantVelocity :one
SELECT
    count(*) FILTER (WHERE created_at > @minute_ago)::integer AS attempts_1m,
    count(*)::integer AS attempts_10m,
    count(*) FILTER (WHERE outcome = 'declined')::integer AS declines_10m,
    count(*) FILTER (WHERE outcome IN ('approved', 'declined'))::integer AS decided_10m
FROM risk.attempts
WHERE merchant_id = @merchant_id AND livemode = @livemode AND created_at > @ten_minutes_ago;

-- name: InsertAttempt :exec
INSERT INTO risk.attempts (attempt_id, merchant_id, livemode, card_fingerprint, ip, created_at, outcome)
VALUES (@attempt_id, @merchant_id, @livemode, @card_fingerprint, @ip, @created_at, @outcome)
ON CONFLICT (attempt_id) DO NOTHING;

-- name: SetOutcome :exec
UPDATE risk.attempts SET outcome = @outcome WHERE attempt_id = @attempt_id AND outcome = 'pending';

-- name: InsertDecision :exec
INSERT INTO risk.decisions (id, attempt_id, intent_id, merchant_id, livemode, action, rules, features, created_at)
VALUES (@id, @attempt_id, @intent_id, @merchant_id, @livemode, @action, @rules, @features, @created_at)
ON CONFLICT (attempt_id) DO NOTHING;

-- name: GetDecision :one
SELECT * FROM risk.decisions WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: DecisionForAttempt :one
SELECT * FROM risk.decisions WHERE attempt_id = @attempt_id;

-- name: ListDecisions :many
SELECT * FROM risk.decisions
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@action::text = '' OR action = @action)
  AND (@intent_id::text = '' OR intent_id = @intent_id)
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: ActiveRules :many
SELECT * FROM risk.rules WHERE merchant_id = @merchant_id AND livemode = @livemode AND deleted_at IS NULL ORDER BY id;

-- name: InsertRule :one
INSERT INTO risk.rules (id, merchant_id, livemode, action, expression, description, created_at)
VALUES (@id, @merchant_id, @livemode, @action, @expression, @description, @created_at)
RETURNING *;

-- name: DeleteRule :one
UPDATE risk.rules SET deleted_at = @now
WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode AND deleted_at IS NULL
RETURNING *;

-- name: InsertListItem :one
INSERT INTO risk.list_items (id, merchant_id, livemode, list, kind, value, created_at)
VALUES (@id, @merchant_id, @livemode, @list, @kind, @value, @created_at)
ON CONFLICT (merchant_id, livemode, list, kind, value) DO UPDATE SET value = excluded.value
RETURNING *;

-- name: DeleteListItem :execrows
DELETE FROM risk.list_items WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: ListItems :many
SELECT * FROM risk.list_items
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: LockListItems :exec
-- Serializes adding a merchant's entries in a mode, so that two cannot pass the cap.
SELECT pg_advisory_xact_lock(hashtext('risk.list_items/' || @scope::text));

-- name: CountListItems :one
SELECT count(*) FROM risk.list_items WHERE merchant_id = @merchant_id AND livemode = @livemode;

-- name: MatchingListItems :many
SELECT * FROM risk.list_items
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND ((kind = 'card_fingerprint' AND value = @card) OR (kind = 'ip' AND value = ANY(@ips::text[])) OR (kind = 'bin' AND value = @bin))
ORDER BY list = 'allow' DESC, id;

-- name: ActiveThrottle :one
SELECT * FROM risk.throttles
WHERE merchant_id = @merchant_id AND livemode = @livemode AND until > @now
ORDER BY started_at DESC
LIMIT 1;

-- name: InsertThrottle :exec
INSERT INTO risk.throttles (merchant_id, livemode, started_at, until, ratio, attempts)
VALUES (@merchant_id, @livemode, @started_at, @until, @ratio, @attempts)
ON CONFLICT DO NOTHING;

-- name: LockMerchant :exec
-- A decision's lock: on a card, an address at a merchant, or a merchant.
SELECT pg_advisory_xact_lock(hashtextextended('risk/' || @key::text, 0));
