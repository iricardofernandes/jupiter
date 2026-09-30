-- +goose Up

-- What a phase hands to the phases after it, such as the id of an object the first phase
-- created. It is saved with the recovery point, so a completer resuming the request
-- after a crash sees what the dead runner saw.
ALTER TABLE api.idempotency_keys ADD COLUMN recovery_state jsonb NOT NULL DEFAULT '{}';

-- +goose Down
ALTER TABLE api.idempotency_keys DROP COLUMN recovery_state;
