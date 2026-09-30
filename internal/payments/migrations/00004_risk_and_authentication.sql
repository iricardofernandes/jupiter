-- +goose Up

-- request_three_d_secure: automatic lets the risk engine decide; any always asks the
-- cardholder to authenticate, where the rail supports it.
-- next_action_url is where the customer goes to complete a 3-D Secure challenge.
ALTER TABLE payments.intents
    ADD COLUMN request_three_d_secure text NOT NULL DEFAULT 'automatic' CHECK (request_three_d_secure IN ('automatic', 'any')),
    ADD COLUMN next_action_url        text NOT NULL DEFAULT '',
    ADD COLUMN risk_decision          text NOT NULL DEFAULT '',
    ADD COLUMN risk_decision_id       text NOT NULL DEFAULT '';

-- An attempt records the risk decision it got, and its 3-D Secure authentication: the
-- directory server's transaction, the result, and the authentication value and ECI the
-- authorization carries.
ALTER TABLE payments.attempts
    ADD COLUMN ip                       text NOT NULL DEFAULT '',
    ADD COLUMN risk_decision            text NOT NULL DEFAULT '',
    ADD COLUMN risk_decision_id         text NOT NULL DEFAULT '',
    ADD COLUMN three_ds_server_trans_id text NOT NULL DEFAULT '',
    ADD COLUMN three_ds_version         text NOT NULL DEFAULT '',
    ADD COLUMN three_ds_status          text NOT NULL DEFAULT '',
    ADD COLUMN ds_trans_id              text NOT NULL DEFAULT '',
    ADD COLUMN acs_trans_id             text NOT NULL DEFAULT '',
    ADD COLUMN acs_url                  text NOT NULL DEFAULT '',
    ADD COLUMN eci                      text NOT NULL DEFAULT '',
    ADD COLUMN authentication_value     text NOT NULL DEFAULT '',
    ADD COLUMN liability_shift          boolean NOT NULL DEFAULT false,
    DROP CONSTRAINT attempts_status_check,
    ADD CONSTRAINT attempts_status_check CHECK (status IN ('authenticating', 'authorizing', 'authorization_unknown',
        'requires_action', 'authorized', 'declined', 'failed', 'capturing', 'capture_unknown', 'captured',
        'voiding', 'void_unknown', 'voided'));

-- A live card is given a network token by the card network, which the authorization
-- then carries instead of the card number; the token is kept in the vault, and here only
-- its reference and state: '' waiting, provisioning, active, failed, suspended. Events
-- about a token are applied in the order the network says they happened.
ALTER TABLE payments.payment_methods
    ADD COLUMN network_token_reference text NOT NULL DEFAULT '',
    ADD COLUMN network_token_status    text NOT NULL DEFAULT ''
        CHECK (network_token_status IN ('', 'provisioning', 'active', 'failed', 'suspended')),
    ADD COLUMN network_token_since     timestamptz,
    ADD COLUMN network_token_event_at  timestamptz;

CREATE INDEX payment_methods_to_tokenize ON payments.payment_methods (id)
    WHERE livemode AND network_token_status IN ('', 'provisioning');
CREATE INDEX payment_methods_by_network_token ON payments.payment_methods (network_token_reference)
    WHERE network_token_reference <> '';

DROP INDEX payments.attempts_to_resolve;
CREATE INDEX attempts_to_resolve ON payments.attempts (updated_at)
    WHERE status IN ('authenticating', 'authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown');

CREATE INDEX attempts_by_server_transaction ON payments.attempts (three_ds_server_trans_id) WHERE three_ds_server_trans_id <> '';

-- +goose Down
DROP INDEX payments.attempts_by_server_transaction;
DROP INDEX payments.attempts_to_resolve;
CREATE INDEX attempts_to_resolve ON payments.attempts (updated_at)
    WHERE status IN ('authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown');
DROP INDEX payments.payment_methods_by_network_token;
DROP INDEX payments.payment_methods_to_tokenize;
ALTER TABLE payments.payment_methods
    DROP COLUMN network_token_event_at, DROP COLUMN network_token_since, DROP COLUMN network_token_status, DROP COLUMN network_token_reference;
ALTER TABLE payments.attempts
    DROP CONSTRAINT attempts_status_check,
    ADD CONSTRAINT attempts_status_check CHECK (status IN ('authorizing', 'authorization_unknown',
        'requires_action', 'authorized', 'declined', 'failed', 'capturing', 'capture_unknown', 'captured',
        'voiding', 'void_unknown', 'voided')),
    DROP COLUMN liability_shift, DROP COLUMN authentication_value, DROP COLUMN eci, DROP COLUMN acs_url,
    DROP COLUMN acs_trans_id, DROP COLUMN ds_trans_id, DROP COLUMN three_ds_status, DROP COLUMN three_ds_version,
    DROP COLUMN three_ds_server_trans_id, DROP COLUMN risk_decision_id, DROP COLUMN risk_decision, DROP COLUMN ip;
ALTER TABLE payments.intents
    DROP COLUMN risk_decision_id, DROP COLUMN risk_decision, DROP COLUMN next_action_url, DROP COLUMN request_three_d_secure;
