-- +goose Up

-- A card payment's split among recipients, and what each capture and refund moved
-- between the merchant's balance and the recipients' (by receivables).
ALTER TABLE payments.intents ADD COLUMN split jsonb;
ALTER TABLE payments.attempts ADD COLUMN split_out bigint NOT NULL DEFAULT 0 CHECK (split_out >= 0);
ALTER TABLE payments.refunds ADD COLUMN split_back bigint NOT NULL DEFAULT 0 CHECK (split_back >= 0);

-- A payout from a recipient's available balance instead of the merchant's.
ALTER TABLE payments.payouts ADD COLUMN recipient_id text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE payments.payouts DROP COLUMN recipient_id;
ALTER TABLE payments.refunds DROP COLUMN split_back;
ALTER TABLE payments.attempts DROP COLUMN split_out;
ALTER TABLE payments.intents DROP COLUMN split;
