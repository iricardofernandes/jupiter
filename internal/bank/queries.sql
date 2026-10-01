-- name: GetTitle :one
SELECT * FROM bank.titles WHERE attempt_id = @attempt_id;

-- name: LockSequence :exec
SELECT pg_advisory_xact_lock(hashtext('bank/titles/' || @scope::text));

-- name: NextSequence :one
SELECT (coalesce(max(sequence), 0) + 1)::bigint FROM bank.titles WHERE livemode = @livemode;

-- name: InsertTitle :exec
INSERT INTO bank.titles (attempt_id, livemode, sequence, our_number, barcode, line, due, amount, payer_name, payer_tax_id,
    hybrid, days_after_due, status, created_at, updated_at)
VALUES (@attempt_id, @livemode, @sequence, @our_number, @barcode, @line, @due, @amount, @payer_name, @payer_tax_id,
    @hybrid, @days_after_due, 'issued', @now, @now);

-- name: RequestWriteOff :one
UPDATE bank.titles SET write_off_requested = true, updated_at = @now
WHERE attempt_id = @attempt_id AND status IN ('issued', 'remitted', 'registered')
RETURNING status;

-- name: TitlesToRemit :many
-- New titles, and registered ones to write off, not yet in a remittance.
SELECT * FROM bank.titles
WHERE livemode = @livemode
  AND ((status = 'issued' AND NOT write_off_requested)
       OR (status = 'registered' AND write_off_requested AND write_off_remittance IS NULL))
ORDER BY sequence
LIMIT 5000
FOR UPDATE;

-- name: WithdrawnBeforeRemitting :many
-- Titles canceled before they were ever sent: nothing to tell the bank.
UPDATE bank.titles SET status = 'written_off', updated_at = @now
WHERE livemode = @livemode AND status = 'issued' AND write_off_requested
RETURNING attempt_id;

-- name: MarkRemitted :exec
UPDATE bank.titles
SET status = CASE WHEN status = 'issued' THEN 'remitted' ELSE status END,
    remittance = CASE WHEN status = 'issued' THEN @remittance ELSE remittance END,
    write_off_remittance = CASE WHEN status = 'registered' THEN @remittance ELSE write_off_remittance END,
    updated_at = @now
WHERE attempt_id = ANY(@attempts::text[]);

-- name: NextRemittance :one
SELECT (coalesce(max(sequence), 0) + 1)::bigint FROM bank.remittances WHERE livemode = @livemode;

-- name: InsertRemittance :exec
INSERT INTO bank.remittances (livemode, sequence, data, created_at) VALUES (@livemode, @sequence, @data, @now);

-- name: UnsentRemittances :many
SELECT * FROM bank.remittances WHERE livemode = @livemode AND NOT sent ORDER BY sequence;

-- name: MarkSent :exec
UPDATE bank.remittances SET sent = true WHERE livemode = @livemode AND sequence = @sequence;

-- name: LastReturn :one
SELECT coalesce(max(sequence), 0)::bigint FROM bank.returns WHERE livemode = @livemode;

-- name: InsertReturn :execrows
INSERT INTO bank.returns (livemode, sequence, titles, imported_at) VALUES (@livemode, @sequence, @titles, @now)
ON CONFLICT DO NOTHING;

-- name: LockTitleByOurNumber :one
SELECT * FROM bank.titles WHERE livemode = @livemode AND our_number = @our_number FOR UPDATE;

-- name: InsertException :exec
INSERT INTO bank.exceptions (livemode, return_sequence, our_number, attempt_id, kind, amount, detail, created_at)
VALUES (@livemode, @return_sequence, @our_number, @attempt_id, @kind, @amount, @detail, @now);

-- name: SetTitleStatus :exec
UPDATE bank.titles SET status = @status, pix_code = @pix_code, updated_at = @now WHERE attempt_id = @attempt_id;

-- name: InsertReturnRecord :exec
INSERT INTO bank.return_records (livemode, return_sequence, line, our_number, occurrence, paid, occurred_on, credit_on, imported_on)
VALUES (@livemode, @return_sequence, @line, @our_number, @occurrence, @paid, @occurred_on, @credit_on, @imported_on);

-- name: SetTitleCredit :exec
UPDATE bank.titles SET credit_on = @credit_on WHERE attempt_id = @attempt_id;

-- name: ReturnRecordsImportedOn :many
SELECT * FROM bank.return_records WHERE livemode = @livemode AND imported_on = @day ORDER BY return_sequence, line;

-- name: PaidTitlesSince :many
SELECT attempt_id, our_number, amount, credit_on, updated_at FROM bank.titles
WHERE livemode = @livemode AND status = 'paid' AND updated_at >= @since
ORDER BY updated_at, attempt_id;
