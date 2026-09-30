-- +goose Up

-- A Pix Automático payment waits, scheduled, for the payer's bank to debit it on its
-- date: no one has anything to do, unlike requires_action.
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_status_check,
    ADD CONSTRAINT attempts_status_check CHECK (status IN ('authenticating', 'authorizing', 'authorization_unknown',
        'requires_action', 'scheduled', 'authorized', 'declined', 'failed', 'capturing', 'capture_unknown', 'captured',
        'voiding', 'void_unknown', 'voided'));

ALTER TABLE payments.intents DROP CONSTRAINT intents_amount_received_check;
ALTER TABLE payments.intents ADD CONSTRAINT intents_amount_received_check
    CHECK (amount_received >= 0 AND (payment_method IN ('pix', 'pix_automatico') OR amount_received <= amount));
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_capture_amount_check,
    DROP CONSTRAINT attempts_amount_captured_check,
    ADD CONSTRAINT attempts_capture_amount_check CHECK (capture_amount > 0 AND (payment_method IN ('pix', 'pix_automatico') OR capture_amount <= amount)),
    ADD CONSTRAINT attempts_amount_captured_check CHECK (amount_captured >= 0 AND (payment_method IN ('pix', 'pix_automatico') OR amount_captured <= amount));

-- +goose Down
-- Only for a database without Pix Automático payments.
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_capture_amount_check,
    DROP CONSTRAINT attempts_amount_captured_check,
    ADD CONSTRAINT attempts_capture_amount_check CHECK (capture_amount > 0 AND (payment_method = 'pix' OR capture_amount <= amount)),
    ADD CONSTRAINT attempts_amount_captured_check CHECK (amount_captured >= 0 AND (payment_method = 'pix' OR amount_captured <= amount));
ALTER TABLE payments.intents DROP CONSTRAINT intents_amount_received_check;
ALTER TABLE payments.intents ADD CONSTRAINT intents_amount_received_check
    CHECK (amount_received >= 0 AND (payment_method = 'pix' OR amount_received <= amount));
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_status_check,
    ADD CONSTRAINT attempts_status_check CHECK (status IN ('authenticating', 'authorizing', 'authorization_unknown',
        'requires_action', 'authorized', 'declined', 'failed', 'capturing', 'capture_unknown', 'captured',
        'voiding', 'void_unknown', 'voided'));
