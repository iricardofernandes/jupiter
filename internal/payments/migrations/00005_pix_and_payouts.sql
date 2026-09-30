-- +goose Up

-- A Pix charge with a due date may be paid with a fine and interest, so what a Pix
-- payment receives may be more than its amount; a card's never is. The checks were
-- declared on the columns in 00001, so PostgreSQL named them: they are found by what they
-- say.
-- +goose StatementBegin
DO $$
DECLARE
    found record;
BEGIN
    FOR found IN
        SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
        WHERE conrelid IN ('payments.intents'::regclass, 'payments.attempts'::regclass) AND contype = 'c'
          AND (pg_get_constraintdef(oid) LIKE '%amount_received <= amount%'
            OR pg_get_constraintdef(oid) LIKE '%capture_amount <= amount%'
            OR pg_get_constraintdef(oid) LIKE '%amount_captured <= amount%')
    LOOP
        EXECUTE format('ALTER TABLE %s DROP CONSTRAINT %I', found.tbl, found.conname);
    END LOOP;
END $$;
-- +goose StatementEnd
ALTER TABLE payments.intents ADD CONSTRAINT intents_amount_received_check
    CHECK (amount_received >= 0 AND (payment_method = 'pix' OR amount_received <= amount));
ALTER TABLE payments.attempts
    ADD CONSTRAINT attempts_capture_amount_check CHECK (capture_amount > 0 AND (payment_method = 'pix' OR capture_amount <= amount)),
    ADD CONSTRAINT attempts_amount_captured_check CHECK (amount_captured >= 0 AND (payment_method = 'pix' OR amount_captured <= amount));

-- How a Pix charge is made for the intent, and what the customer is shown while it is
-- waiting to be paid: the BR Code, and until when it can be paid.
ALTER TABLE payments.intents
    ADD COLUMN pix_options            jsonb,
    ADD COLUMN next_action_data       text NOT NULL DEFAULT '',
    ADD COLUMN next_action_expires_at timestamptz;

-- The charge made at the bank for a Pix attempt, keyed by its txid, which is the
-- attempt's id without the underscore: never reused, since attempt ids are not.
CREATE TABLE payments.pix_charges (
    txid       text PRIMARY KEY,
    attempt_id text NOT NULL UNIQUE REFERENCES payments.attempts (id),
    livemode   boolean NOT NULL,
    due        boolean NOT NULL,
    copy_paste text NOT NULL,
    expires_at timestamptz NOT NULL,
    created_at timestamptz NOT NULL
);

-- Every Pix the bank says Jupiter received, once per endToEndId: applied to the attempt
-- whose charge it paid, or, when it paid none (no txid, or a charge no longer wanted),
-- held apart and returned to the payer.
CREATE TABLE payments.pix_received (
    livemode    boolean NOT NULL,
    e2e_id      text NOT NULL,
    txid        text NOT NULL DEFAULT '',
    amount      bigint NOT NULL CHECK (amount > 0),
    currency    text NOT NULL,
    received_at timestamptz NOT NULL,
    attempt_id  text REFERENCES payments.attempts (id),
    status      text NOT NULL CHECK (status IN ('applied', 'unmatched', 'returning', 'returned', 'return_failed')),
    ledger_txn  text,
    return_txn  text,
    reason      text NOT NULL DEFAULT '',
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (livemode, e2e_id),
    CHECK ((status = 'applied') = (attempt_id IS NOT NULL))
);

CREATE INDEX pix_received_to_return ON payments.pix_received (updated_at) WHERE status IN ('unmatched', 'returning');

ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check
    CHECK (role IN ('merchant_balance', 'network_receivable', 'pix_settlement', 'pix_unmatched'));

-- A payout: money leaving a merchant's balance for a Pix key. The amount is held on the
-- ledger when it is created and posted once the bank confirms the transfer.
CREATE TABLE payments.payouts (
    id              text PRIMARY KEY,
    merchant_id     text NOT NULL,
    livemode        boolean NOT NULL,
    amount          bigint NOT NULL CHECK (amount > 0),
    currency        text NOT NULL,
    pix_key         text NOT NULL,
    description     text NOT NULL DEFAULT '',
    status          text NOT NULL CHECK (status IN ('sending', 'unknown', 'paid', 'failed')),
    failure_code    text NOT NULL DEFAULT '',
    failure_message text NOT NULL DEFAULT '',
    e2e_id          text NOT NULL DEFAULT '',
    recipient_name  text NOT NULL DEFAULT '',
    ledger_hold     text NOT NULL,
    unknown_since   timestamptz,
    resolutions     integer NOT NULL DEFAULT 0,
    arrived_at      timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);

CREATE INDEX payouts_by_merchant ON payments.payouts (merchant_id, livemode, id);
CREATE INDEX payouts_to_resolve ON payments.payouts (updated_at) WHERE status IN ('sending', 'unknown');

-- +goose Down
-- Only for a database without Pix payments: the old checks refuse their rows.
DROP TABLE payments.payouts;
ALTER TABLE payments.ledger_accounts DROP CONSTRAINT ledger_accounts_role_check;
ALTER TABLE payments.ledger_accounts ADD CONSTRAINT ledger_accounts_role_check CHECK (role IN ('merchant_balance', 'network_receivable'));
DROP TABLE payments.pix_received;
DROP TABLE payments.pix_charges;
ALTER TABLE payments.intents DROP COLUMN next_action_expires_at, DROP COLUMN next_action_data, DROP COLUMN pix_options;
ALTER TABLE payments.attempts DROP CONSTRAINT attempts_amount_captured_check, DROP CONSTRAINT attempts_capture_amount_check;
ALTER TABLE payments.attempts
    ADD CONSTRAINT attempts_capture_amount_check CHECK (capture_amount > 0 AND capture_amount <= amount),
    ADD CONSTRAINT attempts_amount_captured_check CHECK (amount_captured >= 0 AND amount_captured <= amount);
ALTER TABLE payments.intents DROP CONSTRAINT intents_amount_received_check;
ALTER TABLE payments.intents ADD CONSTRAINT intents_amount_received_check CHECK (amount_received >= 0 AND amount_received <= amount);
