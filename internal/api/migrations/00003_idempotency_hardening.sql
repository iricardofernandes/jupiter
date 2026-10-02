-- +goose Up

-- How many times the completer took a request on; past a bound it is left for a person.
ALTER TABLE api.idempotency_keys ADD COLUMN completer_runs integer NOT NULL DEFAULT 0;

-- Whether the request body is sealed, as it is from now on; one stored before is read as
-- it was stored.
ALTER TABLE api.idempotency_keys ADD COLUMN request_sealed boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE api.idempotency_keys DROP COLUMN request_sealed;
ALTER TABLE api.idempotency_keys DROP COLUMN completer_runs;
