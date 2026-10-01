-- +goose Up

-- When a dispute's money left: what reconciliation dates its debit by, even once given back.
ALTER TABLE payments.dispute_funds ADD COLUMN withdrawn_at timestamptz;
UPDATE payments.dispute_funds SET withdrawn_at = updated_at WHERE status IN ('withdrawn', 'reinstated');

-- +goose Down
ALTER TABLE payments.dispute_funds DROP COLUMN withdrawn_at;
