-- name: InsertAccount :exec
INSERT INTO ledger.accounts (id, book, code, currency, normal, non_negative, batched, clearing, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetAccounts :many
SELECT id, book, code, currency, normal, non_negative, batched, clearing, created_at
FROM ledger.accounts
WHERE id = ANY(@ids::text[]);

-- name: InsertTransaction :exec
INSERT INTO ledger.transactions (id, kind, resolves_id, reverses_id, expires_at, entry_count, description, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- Set-returning functions in a select list advance together, so the arrays are zipped
-- in order, and rows are inserted (and balance rows locked) in the order Go sorted them.
-- name: InsertEntries :exec
INSERT INTO ledger.entries (transaction_id, transaction_kind, account_id, book, currency, layer, amount)
SELECT @transaction_id::text, @transaction_kind::text,
       unnest(@account_ids::text[]), unnest(@books::text[]), unnest(@currencies::text[]),
       unnest(@layers::text[]), unnest(@amounts::bigint[]);

-- name: GetTransaction :one
SELECT id, kind, resolves_id, reverses_id, expires_at, entry_count, description, created_at
FROM ledger.transactions
WHERE id = $1;

-- name: GetEntries :many
SELECT seq, transaction_id, transaction_kind, account_id, book, currency, layer, amount
FROM ledger.entries
WHERE transaction_id = $1
ORDER BY seq;

-- name: GetResolution :one
SELECT id, kind FROM ledger.transactions WHERE resolves_id = $1;

-- name: GetBalance :one
SELECT b.account_id, b.drifted_at, a.currency, a.normal,
       (b.posted_debits + coalesce(q.posted_debits, 0))::bigint     AS posted_debits,
       (b.posted_credits + coalesce(q.posted_credits, 0))::bigint   AS posted_credits,
       (b.pending_debits + coalesce(q.pending_debits, 0))::bigint   AS pending_debits,
       (b.pending_credits + coalesce(q.pending_credits, 0))::bigint AS pending_credits
FROM ledger.balances b
JOIN ledger.accounts a ON a.id = b.account_id
LEFT JOIN LATERAL (
    SELECT sum(posted_debits) AS posted_debits, sum(posted_credits) AS posted_credits,
           sum(pending_debits) AS pending_debits, sum(pending_credits) AS pending_credits
    FROM ledger.balance_queue
    WHERE account_id = b.account_id
) q ON true
WHERE b.account_id = $1;

-- name: DuePendingTransfers :many
SELECT t.id
FROM ledger.transactions t
WHERE t.kind = 'pending'
  AND t.expires_at <= @now::timestamptz
  AND NOT EXISTS (SELECT 1 FROM ledger.transactions r WHERE r.resolves_id = t.id)
ORDER BY t.expires_at, t.id
LIMIT @max_count::integer;

-- name: TryApplierLock :one
SELECT pg_try_advisory_xact_lock(hashtext('ledger.balance_queue'));

-- Applies the oldest queued deltas in entry order, in one statement: the rows are
-- deleted and their sums added to the cached balances atomically.
-- name: ApplyQueuedDeltas :one
WITH batch AS (
    DELETE FROM ledger.balance_queue
    WHERE entry_seq IN (SELECT entry_seq FROM ledger.balance_queue ORDER BY entry_seq LIMIT @max_count::integer)
    RETURNING account_id, posted_debits, posted_credits, pending_debits, pending_credits
), sums AS (
    SELECT account_id, count(*) AS n,
           sum(posted_debits)::bigint AS posted_debits, sum(posted_credits)::bigint AS posted_credits,
           sum(pending_debits)::bigint AS pending_debits, sum(pending_credits)::bigint AS pending_credits
    FROM batch
    GROUP BY account_id
), applied AS (
    UPDATE ledger.balances b
       SET posted_debits   = b.posted_debits + s.posted_debits,
           posted_credits  = b.posted_credits + s.posted_credits,
           pending_debits  = b.pending_debits + s.pending_debits,
           pending_credits = b.pending_credits + s.pending_credits
      FROM sums s
     WHERE b.account_id = s.account_id
    RETURNING s.n
)
SELECT coalesce(sum(n), 0)::bigint FROM applied;

-- name: BalanceMismatches :many
WITH recomputed AS (
    SELECT e.account_id,
           sum(d.posted_debits)::bigint AS posted_debits, sum(d.posted_credits)::bigint AS posted_credits,
           sum(d.pending_debits)::bigint AS pending_debits, sum(d.pending_credits)::bigint AS pending_credits
    FROM ledger.entries e
    CROSS JOIN LATERAL ledger.entry_deltas(e.transaction_kind, e.layer, e.amount) d
    GROUP BY e.account_id
), queued AS (
    SELECT account_id,
           sum(posted_debits)::bigint AS posted_debits, sum(posted_credits)::bigint AS posted_credits,
           sum(pending_debits)::bigint AS pending_debits, sum(pending_credits)::bigint AS pending_credits
    FROM ledger.balance_queue
    GROUP BY account_id
)
SELECT b.account_id,
       (b.posted_debits + coalesce(q.posted_debits, 0))::bigint     AS cached_posted_debits,
       (b.posted_credits + coalesce(q.posted_credits, 0))::bigint   AS cached_posted_credits,
       (b.pending_debits + coalesce(q.pending_debits, 0))::bigint   AS cached_pending_debits,
       (b.pending_credits + coalesce(q.pending_credits, 0))::bigint AS cached_pending_credits,
       coalesce(r.posted_debits, 0)::bigint   AS posted_debits,
       coalesce(r.posted_credits, 0)::bigint  AS posted_credits,
       coalesce(r.pending_debits, 0)::bigint  AS pending_debits,
       coalesce(r.pending_credits, 0)::bigint AS pending_credits
FROM ledger.balances b
LEFT JOIN recomputed r ON r.account_id = b.account_id
LEFT JOIN queued q ON q.account_id = b.account_id
WHERE (sqlc.narg('only')::text[] IS NULL OR b.account_id = ANY(sqlc.narg('only')::text[]))
  AND (b.posted_debits + coalesce(q.posted_debits, 0), b.posted_credits + coalesce(q.posted_credits, 0),
       b.pending_debits + coalesce(q.pending_debits, 0), b.pending_credits + coalesce(q.pending_credits, 0))
      IS DISTINCT FROM
      (coalesce(r.posted_debits, 0), coalesce(r.posted_credits, 0),
       coalesce(r.pending_debits, 0), coalesce(r.pending_credits, 0))
ORDER BY b.account_id;

-- name: UnbalancedTransactions :many
SELECT t.id, t.entry_count, count(e.seq)::integer AS actual_count
FROM ledger.transactions t
LEFT JOIN ledger.entries e ON e.transaction_id = t.id
GROUP BY t.id, t.entry_count
HAVING count(e.seq) <> t.entry_count
    OR EXISTS (
        SELECT 1 FROM ledger.entries x
        WHERE x.transaction_id = t.id
        GROUP BY x.book, x.currency, x.layer
        HAVING sum(x.amount) <> 0
    )
ORDER BY t.id;

-- A resolution must release exactly its hold, and a reversal must exactly cancel the
-- posted entries of what it reverses, account by account.
-- name: InconsistentLinks :many
SELECT l.id, l.kind
FROM ledger.transactions l
WHERE (l.resolves_id IS NOT NULL OR l.reverses_id IS NOT NULL)
  AND EXISTS (
      SELECT 1
      FROM ledger.entries e
      WHERE e.transaction_id IN (l.id, coalesce(l.resolves_id, l.reverses_id))
        AND e.layer = CASE WHEN l.resolves_id IS NOT NULL THEN 'pending' ELSE 'posted' END
      GROUP BY e.account_id
      HAVING sum(e.amount) <> 0
  )
ORDER BY l.id;

-- name: ClearingAccountsNotAtZero :many
SELECT a.id, sum(e.amount)::bigint AS posted_balance, max(t.created_at)::timestamptz AS last_activity
FROM ledger.accounts a
JOIN ledger.entries e ON e.account_id = a.id AND e.layer = 'posted'
JOIN ledger.transactions t ON t.id = e.transaction_id
WHERE a.clearing
GROUP BY a.id
HAVING sum(e.amount) <> 0 AND max(t.created_at) < @settled_before::timestamptz
ORDER BY a.id;

-- name: OverduePendingTransfers :many
SELECT t.id, t.expires_at
FROM ledger.transactions t
WHERE t.kind = 'pending'
  AND t.expires_at < @overdue_before::timestamptz
  AND NOT EXISTS (SELECT 1 FROM ledger.transactions r WHERE r.resolves_id = t.id)
ORDER BY t.expires_at, t.id;

-- name: MarkDrifted :exec
UPDATE ledger.balances SET drifted_at = @now::timestamptz
WHERE account_id = ANY(@account_ids::text[]) AND drifted_at IS NULL;

-- name: LockBalances :exec
SELECT account_id FROM ledger.balances WHERE account_id = ANY(@account_ids::text[]) ORDER BY account_id FOR UPDATE;

-- name: LockApplier :exec
SELECT pg_advisory_xact_lock(hashtext('ledger.balance_queue'));

-- name: LockBalance :one
SELECT account_id FROM ledger.balances WHERE account_id = $1 FOR UPDATE;

-- Resets one cached balance to what its entries prove, net of deltas still queued, and
-- clears its drift mark. Run it while holding the balance row's lock.
-- name: RepairBalance :exec
WITH recomputed AS (
    SELECT coalesce(sum(d.posted_debits), 0)::bigint AS posted_debits,
           coalesce(sum(d.posted_credits), 0)::bigint AS posted_credits,
           coalesce(sum(d.pending_debits), 0)::bigint AS pending_debits,
           coalesce(sum(d.pending_credits), 0)::bigint AS pending_credits
    FROM ledger.entries e
    CROSS JOIN LATERAL ledger.entry_deltas(e.transaction_kind, e.layer, e.amount) d
    WHERE e.account_id = @account_id::text
), queued AS (
    SELECT coalesce(sum(posted_debits), 0)::bigint AS posted_debits,
           coalesce(sum(posted_credits), 0)::bigint AS posted_credits,
           coalesce(sum(pending_debits), 0)::bigint AS pending_debits,
           coalesce(sum(pending_credits), 0)::bigint AS pending_credits
    FROM ledger.balance_queue
    WHERE account_id = @account_id::text
)
UPDATE ledger.balances b
   SET posted_debits   = r.posted_debits - q.posted_debits,
       posted_credits  = r.posted_credits - q.posted_credits,
       pending_debits  = r.pending_debits - q.pending_debits,
       pending_credits = r.pending_credits - q.pending_credits,
       drifted_at      = NULL
  FROM recomputed r, queued q
 WHERE b.account_id = @account_id::text;

-- name: Health :one
-- The batched balance deltas not yet applied, when the oldest was posted, and the
-- accounts the checker found drifted.
SELECT
    (SELECT count(*) FROM ledger.balance_queue)::bigint AS queued,
    (SELECT t.created_at FROM ledger.balance_queue q
     JOIN ledger.entries e ON e.seq = q.entry_seq JOIN ledger.transactions t ON t.id = e.transaction_id
     ORDER BY q.entry_seq LIMIT 1)::timestamptz AS oldest_queued,
    (SELECT count(*) FROM ledger.balances WHERE drifted_at IS NOT NULL)::bigint AS drifted;
