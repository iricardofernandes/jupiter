-- +goose Up

-- A boleto's terms, on an intent paid by boleto, and Jupiter's account at the bank that
-- collects its boletos and makes its transfers.
ALTER TABLE payments.intents ADD COLUMN boleto_options jsonb;

ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched', 'card_fees', 'bank_settlement'));

-- +goose Down
ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched', 'card_fees'));
ALTER TABLE payments.intents DROP COLUMN boleto_options;
