-- +goose Up

-- Jupiter's fees reach its client funds account with the merchants' money; when the day's
-- card settlement is posted, its fees move to Jupiter's own funds.
ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched', 'card_fees', 'bank_settlement',
                    'own_bank', 'card_fee_revenue'));

CREATE TABLE payments.fee_sweeps (
    reference  text NOT NULL,
    livemode   boolean NOT NULL,
    currency   text NOT NULL,
    amount     bigint NOT NULL CHECK (amount > 0),
    ledger_txn text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (livemode, reference)
);

-- +goose Down
DROP TABLE payments.fee_sweeps;
ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched', 'card_fees', 'bank_settlement'));
