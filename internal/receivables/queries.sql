-- name: LockUnit :one
SELECT * FROM receivables.units
WHERE merchant_id = @merchant_id AND livemode = @livemode AND arrangement = @arrangement AND settlement_date = @settlement_date
FOR UPDATE;

-- name: LockUnitByID :one
SELECT * FROM receivables.units WHERE id = @id FOR UPDATE;

-- name: GetUnit :one
SELECT * FROM receivables.units WHERE id = @id;

-- name: InsertUnit :exec
INSERT INTO receivables.units (id, merchant_id, livemode, arrangement, settlement_date, value, currency, constituted_on, created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @arrangement, @settlement_date, 0, @currency, @constituted_on, @now, @now)
ON CONFLICT (merchant_id, livemode, arrangement, settlement_date) DO NOTHING;

-- name: ChangeUnitValue :exec
-- Any change is a new version the registry must take.
UPDATE receivables.units
SET value = value + @delta, constituted_on = greatest(constituted_on, @constituted_on), version = version + 1,
    register_retry_at = NULL, updated_at = @now
WHERE id = @id;

-- name: InsertInstallment :exec
INSERT INTO receivables.installments (attempt_id, number, payment_intent, unit_id, gross, fee, net)
VALUES (@attempt_id, @number, @payment_intent, @unit_id, @gross, @fee, @net);

-- name: InstallmentsOfIntent :many
SELECT i.*, (u.settled_on IS NOT NULL)::boolean AS unit_settled
FROM receivables.installments i JOIN receivables.units u ON u.id = i.unit_id
WHERE i.payment_intent = @payment_intent
ORDER BY i.number
FOR UPDATE OF i, u;

-- name: ReduceInstallment :exec
UPDATE receivables.installments SET reduced = reduced + @amount WHERE attempt_id = @attempt_id AND number = @number;

-- name: InsertUnitEvent :exec
INSERT INTO receivables.unit_events (unit_id, at, kind, amount, reference) VALUES (@unit_id, @at, @kind, @amount, @reference);

-- name: UnitsToRegister :many
SELECT * FROM receivables.units
WHERE livemode = @livemode AND version > registered_version AND (register_retry_at IS NULL OR register_retry_at <= @now)
ORDER BY updated_at, id
LIMIT @max_count::integer;

-- name: UnregisteredSince :many
-- Units not registered as they are, from sales on or before a day: the late ones.
SELECT id, livemode, constituted_on, register_error FROM receivables.units
WHERE version > registered_version AND constituted_on <= @day AND livemode = ANY(@modes::boolean[])
ORDER BY constituted_on, id
LIMIT 1000;

-- name: MarkRegistered :exec
UPDATE receivables.units
SET registered_version = greatest(registered_version, @version), registered_at = @now, register_error = ''
WHERE id = @id;

-- name: MarkRegisterError :exec
UPDATE receivables.units SET register_error = @error, register_retry_at = @retry_at WHERE id = @id;

-- name: Reregister :exec
-- The registry disagrees with what it was sent: send it again.
-- The correction is due from the day it is found.
UPDATE receivables.units
SET registered_version = 0, register_retry_at = NULL, constituted_on = greatest(constituted_on, @today)
WHERE id = @id;

-- name: MarkSettled :exec
UPDATE receivables.units
SET settled_on = @settled_on, settled_amount = @amount, payments = @payments, updated_at = @now
WHERE id = @id AND settled_on IS NULL;

-- name: LatestSnapshot :one
SELECT commitments FROM receivables.effect_snapshots
WHERE unit_id = @unit_id AND observed_at <= @at
ORDER BY observed_at DESC, id DESC LIMIT 1;

-- name: InsertSnapshot :exec
INSERT INTO receivables.effect_snapshots (unit_id, observed_at, commitments) VALUES (@unit_id, @observed_at, @commitments);

-- name: SetOptIn :exec
INSERT INTO receivables.opt_ins (merchant_id, livemode, financier, active, synced, updated_at)
VALUES (@merchant_id, @livemode, @financier, @active, false, @now)
ON CONFLICT (merchant_id, livemode, financier) DO UPDATE
SET active = excluded.active, synced = false, sync_error = '', retry_at = NULL, updated_at = excluded.updated_at;

-- name: OptInsToSync :many
SELECT * FROM receivables.opt_ins
WHERE livemode = @livemode AND NOT synced AND (retry_at IS NULL OR retry_at <= @now)
ORDER BY updated_at LIMIT 100;

-- name: MarkOptInSynced :exec
UPDATE receivables.opt_ins SET synced = true, sync_error = '', retry_at = NULL
WHERE merchant_id = @merchant_id AND livemode = @livemode AND financier = @financier AND updated_at = @updated_at;

-- name: MarkOptInError :exec
UPDATE receivables.opt_ins SET sync_error = @error, retry_at = @retry_at
WHERE merchant_id = @merchant_id AND livemode = @livemode AND financier = @financier;

-- name: CountOptIns :one
-- Others than financier: changing an existing one never counts against the cap.
SELECT count(*) FROM receivables.opt_ins WHERE merchant_id = @merchant_id AND livemode = @livemode AND financier <> @financier;

-- name: OptInsOf :many
SELECT * FROM receivables.opt_ins WHERE merchant_id = @merchant_id AND livemode = @livemode ORDER BY financier;

-- name: LastReconciliation :one
SELECT coalesce(max(ran_on), '0001-01-01'::date)::date FROM receivables.reconciliations WHERE livemode = @livemode AND kind = @kind;

-- name: InsertReconciliation :exec
INSERT INTO receivables.reconciliations (livemode, kind, ran_on, ran_at, divergences)
VALUES (@livemode, @kind, @ran_on, @ran_at, @divergences)
ON CONFLICT (livemode, kind, ran_on) DO UPDATE SET ran_at = excluded.ran_at, divergences = excluded.divergences;

-- name: OpenDivergence :exec
INSERT INTO receivables.divergences (livemode, kind, subject, detail, found_at)
VALUES (@livemode, @kind, @subject, @detail, @found_at)
ON CONFLICT (livemode, kind, subject) WHERE resolved_at IS NULL DO UPDATE SET detail = excluded.detail;

-- name: ResolveDivergences :exec
-- Those of a kind a reconciliation no longer found.
UPDATE receivables.divergences SET resolved_at = @now
WHERE livemode = @livemode AND kind = @kind AND resolved_at IS NULL AND NOT (subject = ANY(@still::text[]));

-- name: OpenDivergences :many
SELECT * FROM receivables.divergences WHERE resolved_at IS NULL ORDER BY found_at, id;

-- name: InstallmentTotals :many
-- For the check: each unit's value is what its installments constituted, less reductions.
SELECT u.id, u.value, coalesce(sum(i.net - i.reduced), 0)::bigint AS from_installments
FROM receivables.units u LEFT JOIN receivables.installments i ON i.unit_id = u.id
GROUP BY u.id, u.value;

-- name: AgendaAsOf :many
-- A merchant's units as they stood at a moment: their value from the events until then,
-- their commitments from the last snapshot before it.
SELECT u.id, u.arrangement, u.settlement_date, u.currency, u.blocked, u.registered_version, u.settled_on,
       coalesce(ev.value, 0)::bigint AS value, coalesce(ev.settled, 0)::bigint AS settled,
       coalesce(ev.is_settled, false)::boolean AS is_settled,
       snap.commitments
FROM receivables.units u
LEFT JOIN LATERAL (
    SELECT sum(e.amount) FILTER (WHERE e.kind IN ('constituted', 'reduced')) AS value,
           sum(e.amount) FILTER (WHERE e.kind = 'settled') AS settled,
           bool_or(e.kind = 'settled') AS is_settled
    FROM receivables.unit_events e WHERE e.unit_id = u.id AND e.at <= @at
) ev ON true
LEFT JOIN LATERAL (
    SELECT es.commitments FROM receivables.effect_snapshots es
    WHERE es.unit_id = u.id AND es.observed_at <= @at
    ORDER BY es.observed_at DESC, es.id DESC LIMIT 1
) snap ON true
WHERE u.merchant_id = @merchant_id AND u.livemode = @livemode
  AND u.settlement_date BETWEEN @from_date AND @to_date AND u.created_at <= @at
ORDER BY u.settlement_date, u.arrangement;

-- name: LatestSnapshots :many
-- What was last seen committed on each unsettled unit of a mode.
SELECT DISTINCT ON (es.unit_id) es.unit_id, es.commitments
FROM receivables.effect_snapshots es JOIN receivables.units u ON u.id = es.unit_id
WHERE u.livemode = @livemode AND u.settled_on IS NULL
ORDER BY es.unit_id, es.observed_at DESC, es.id DESC;

-- name: UnitsToReconcile :many
-- A mode's units still unsettled, or settled on or after since.
SELECT * FROM receivables.units
WHERE livemode = @livemode AND (settled_on IS NULL OR settlement_date >= @since)
ORDER BY settlement_date, id;
