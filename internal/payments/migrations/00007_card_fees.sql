-- +goose Up

-- Jupiter's fee on a captured card payment, and the part of it a refund gives back.
ALTER TABLE payments.attempts ADD COLUMN fee bigint NOT NULL DEFAULT 0 CHECK (fee >= 0);
ALTER TABLE payments.refunds ADD COLUMN fee_returned bigint NOT NULL DEFAULT 0 CHECK (fee_returned >= 0);

ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched', 'card_fees'));

-- +goose Down
ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched'));
ALTER TABLE payments.refunds DROP COLUMN fee_returned;
ALTER TABLE payments.attempts DROP COLUMN fee;
