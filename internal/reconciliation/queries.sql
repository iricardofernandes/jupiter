-- name: LockMode :exec
SELECT pg_advisory_xact_lock(hashtext('reconciliation/' || @scope::text));

-- name: InsertRecord :execrows
INSERT INTO reconciliation.records (livemode, counterparty, stream, side, identity, key, direction, amount, value_date, merchant_id, reference, created_at)
VALUES (@livemode, @counterparty, @stream, @side, @identity, @key, @direction, @amount, @value_date, @merchant_id, @reference, @now)
ON CONFLICT (livemode, counterparty, stream, side, identity) DO NOTHING;

-- name: UnmatchedRecords :many
SELECT * FROM reconciliation.records
WHERE livemode = @livemode AND counterparty = @counterparty AND stream = @stream AND matched_with IS NULL AND settled_by = ''
ORDER BY value_date, id
LIMIT @max_count::integer;

-- name: SettleRecords :exec
-- Records an operator resolved a break of: taken as they are, never matched again.
UPDATE reconciliation.records SET settled_by = @break_id
WHERE id IN (@a::bigint, @b::bigint) AND matched_with IS NULL;

-- name: MatchedWithKeys :many
-- Records matched before that share a key with those still unmatched.
SELECT * FROM reconciliation.records
WHERE livemode = @livemode AND counterparty = @counterparty AND stream = @stream AND matched_with IS NOT NULL
  AND key = ANY(@keys::text[]);

-- name: Pair :execrows
UPDATE reconciliation.records
SET matched_with = CASE WHEN id = @a::bigint THEN @b::bigint ELSE @a::bigint END, match_rule = @rule, matched_on = @day
WHERE id IN (@a::bigint, @b::bigint) AND matched_with IS NULL;

-- name: MerchantOfKey :one
-- The merchant Jupiter's record of a key belongs to: whom a counterparty's record of it
-- concerns.
SELECT coalesce(max(merchant_id), '')::text FROM reconciliation.records
WHERE livemode = @livemode AND counterparty = @counterparty AND stream = @stream AND side = 'jupiter' AND key = @key;

-- name: OpenBreakOf :one
SELECT * FROM reconciliation.breaks WHERE record_id = @record_id AND status = 'open';

-- name: InsertBreak :exec
INSERT INTO reconciliation.breaks (
    id, livemode, counterparty, stream, kind, record_id, other_record_id, key, subject, detail, merchant_id, amount, value_date,
    score, reasons, status, opened_on, created_at, updated_at
) VALUES (
    @id, @livemode, @counterparty, @stream, @kind, @record_id, @other_record_id, @key, @subject, @detail, @merchant_id, @amount, @value_date,
    @score, @reasons, 'open', @opened_on, @now, @now
);

-- name: UpdateBreak :exec
UPDATE reconciliation.breaks
SET kind = @kind, other_record_id = @other_record_id, detail = @detail, score = @score, reasons = @reasons, updated_at = @now
WHERE id = @id;

-- name: ResolveBreak :execrows
UPDATE reconciliation.breaks SET status = 'resolved', resolution = @resolution, resolved_on = @day, updated_at = @now
WHERE id = @id AND status = 'open';

-- name: OpenBreaksOfMatched :many
-- Open breaks whose record, or the other record they name, is matched now.
SELECT b.id FROM reconciliation.breaks b
JOIN reconciliation.records r ON r.id = b.record_id
LEFT JOIN reconciliation.records o ON o.id = b.other_record_id
WHERE b.livemode = @livemode AND b.status = 'open' AND (r.matched_with IS NOT NULL OR o.matched_with IS NOT NULL);

-- name: OpenSubjectBreaks :many
SELECT * FROM reconciliation.breaks WHERE livemode = @livemode AND counterparty = @counterparty AND status = 'open' AND subject <> '';

-- name: GetBreak :one
SELECT * FROM reconciliation.breaks WHERE id = @id;

-- name: GetRecord :one
SELECT * FROM reconciliation.records WHERE id = @id;

-- name: LastRun :one
SELECT coalesce(max(day), '0001-01-01'::date)::date FROM reconciliation.runs WHERE livemode = @livemode;

-- name: InsertRun :exec
INSERT INTO reconciliation.runs (livemode, day, ran_at, matched, opened, resolved)
VALUES (@livemode, @day, @ran_at, @matched, @opened, @resolved)
ON CONFLICT (livemode, day) DO UPDATE
SET ran_at = excluded.ran_at, matched = reconciliation.runs.matched + excluded.matched,
    opened = reconciliation.runs.opened + excluded.opened, resolved = reconciliation.runs.resolved + excluded.resolved;

-- name: MatchedOn :many
-- Jupiter's records of a day that are matched, by counterparty and stream.
SELECT counterparty, stream, count(*)::bigint AS records, coalesce(sum(amount), 0)::bigint AS amount
FROM reconciliation.records
WHERE livemode = @livemode AND side = 'jupiter' AND value_date = @day AND matched_with IS NOT NULL
  AND (@merchant_id::text = '' OR merchant_id = @merchant_id)
GROUP BY counterparty, stream ORDER BY counterparty, stream;

-- name: BreaksForReport :many
-- The breaks open at the end of a day, or opened or resolved on it.
SELECT * FROM reconciliation.breaks
WHERE livemode = @livemode AND opened_on <= @day AND (status = 'open' OR resolved_on >= @day)
  AND (@merchant_id::text = '' OR merchant_id = @merchant_id)
ORDER BY opened_on, id
LIMIT 10000;

-- name: ListBreaks :many
SELECT * FROM reconciliation.breaks
WHERE livemode = @livemode AND (@merchant_id::text = '' OR merchant_id = @merchant_id)
  AND (@status::text = '' OR status = @status)
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before::text <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;
