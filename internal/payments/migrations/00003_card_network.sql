-- +goose Up

-- Installments ("parcelado") are carried on the authorization: how many, and who finances
-- them, the merchant (sem juros) or the issuer (com juros).
ALTER TABLE payments.intents
    ADD COLUMN installments             integer CHECK (installments BETWEEN 2 AND 12),
    ADD COLUMN installments_financed_by text CHECK (installments_financed_by IN ('merchant', 'issuer')),
    ADD COLUMN setup_future_usage       text NOT NULL DEFAULT '' CHECK (setup_future_usage IN ('', 'off_session')),
    ADD CONSTRAINT intents_installments_complete CHECK ((installments IS NULL) = (installments_financed_by IS NULL));

-- An attempt records who initiated it and whether it stores the card for later
-- merchant-initiated payments, and the network's identifier for it, which those later
-- payments must quote. Clearing is when the network confirms what was captured.
ALTER TABLE payments.attempts
    ADD COLUMN initiator                text NOT NULL DEFAULT 'customer' CHECK (initiator IN ('customer', 'merchant')),
    ADD COLUMN stores_credential        boolean NOT NULL DEFAULT false,
    ADD COLUMN installments             integer CHECK (installments BETWEEN 2 AND 12),
    ADD COLUMN installments_financed_by text CHECK (installments_financed_by IN ('merchant', 'issuer')),
    ADD COLUMN network_transaction_id   text NOT NULL DEFAULT '',
    ADD COLUMN cleared_on               date,
    ADD COLUMN amount_cleared           bigint CHECK (amount_cleared > 0),
    ADD CONSTRAINT attempts_cleared_complete CHECK ((cleared_on IS NULL) = (amount_cleared IS NULL));

ALTER TABLE payments.payment_methods ADD COLUMN network_transaction_id text NOT NULL DEFAULT '';

ALTER TABLE payments.refunds ADD COLUMN cleared_on date;

-- +goose Down
ALTER TABLE payments.refunds DROP COLUMN cleared_on;
ALTER TABLE payments.payment_methods DROP COLUMN network_transaction_id;
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_cleared_complete, DROP COLUMN amount_cleared, DROP COLUMN cleared_on,
    DROP COLUMN network_transaction_id, DROP COLUMN installments_financed_by, DROP COLUMN installments,
    DROP COLUMN stores_credential, DROP COLUMN initiator;
ALTER TABLE payments.intents
    DROP CONSTRAINT intents_installments_complete, DROP COLUMN setup_future_usage,
    DROP COLUMN installments_financed_by, DROP COLUMN installments;
