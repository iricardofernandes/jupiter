-- name: InsertRecipient :exec
INSERT INTO recipients.recipients (id, merchant_id, livemode, name, tax_id, is_default, status, payout_method, pix_key,
    bank_ispb, bank_branch, bank_account, transfer_interval, transfer_day, auto_anticipation, auto_anticipation_delay,
    created_at, updated_at)
VALUES (@id, @merchant_id, @livemode, @name, @tax_id, @is_default, @status, @payout_method, @pix_key,
    @bank_ispb, @bank_branch, @bank_account, @transfer_interval, @transfer_day, @auto_anticipation, @auto_anticipation_delay,
    @now, @now);

-- name: GetRecipient :one
SELECT * FROM recipients.recipients WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode;

-- name: GetRecipientByID :one
SELECT * FROM recipients.recipients WHERE id = @id;

-- name: LockRecipient :one
SELECT * FROM recipients.recipients WHERE id = @id AND merchant_id = @merchant_id AND livemode = @livemode FOR UPDATE;

-- name: DefaultRecipient :one
SELECT * FROM recipients.recipients WHERE merchant_id = @merchant_id AND livemode = @livemode AND is_default;

-- name: SaveRecipient :exec
UPDATE recipients.recipients
SET name = @name, status = @status, payout_method = @payout_method, pix_key = @pix_key, bank_ispb = @bank_ispb,
    bank_branch = @bank_branch, bank_account = @bank_account, transfer_interval = @transfer_interval,
    transfer_day = @transfer_day, auto_anticipation = @auto_anticipation, auto_anticipation_delay = @auto_anticipation_delay,
    updated_at = @updated_at
WHERE id = @id;

-- name: ListRecipients :many
SELECT * FROM recipients.recipients
WHERE merchant_id = @merchant_id AND livemode = @livemode
  AND (@starting_after::text = '' OR id < @starting_after)
  AND (@ending_before::text = '' OR id > @ending_before)
ORDER BY CASE WHEN @ending_before <> '' THEN id END ASC, id DESC
LIMIT @max_count::integer;

-- name: RecipientsOf :many
SELECT * FROM recipients.recipients WHERE merchant_id = @merchant_id AND livemode = @livemode AND id = ANY(@ids::text[]);

-- name: AutoAnticipating :many
-- A page of them, after the last one a pass reached.
SELECT * FROM recipients.recipients
WHERE livemode = @livemode AND auto_anticipation AND status = 'verified' AND id > @after
ORDER BY id LIMIT 200;

-- name: TransferringRecipients :many
-- A page of the verified recipients of a mode whose payouts are scheduled.
SELECT * FROM recipients.recipients
WHERE livemode = @livemode AND transfer_interval <> 'manual' AND status = 'verified' AND id > @after
ORDER BY id LIMIT 200;

-- name: SetPayoutsHeld :exec
UPDATE recipients.recipients SET payouts_held = @held, updated_at = @now WHERE id = @id;

-- name: CountRecipients :one
SELECT count(*) FROM recipients.recipients WHERE merchant_id = @merchant_id AND livemode = @livemode;
