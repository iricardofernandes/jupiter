-- +goose Up

CREATE SCHEMA IF NOT EXISTS api;

-- One row per Idempotency-Key, after Brandur Leach's design: the request is recorded
-- before any work, each atomic phase advances recovery_point in the same transaction
-- as its effects, and the final response is kept so a retry gets it back verbatim.
-- lock_token names the one runner allowed to advance the row; a phase whose token no
-- longer matches rolls back instead of committing.
CREATE TABLE api.idempotency_keys (
    id              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    merchant_id     text NOT NULL,
    livemode        boolean NOT NULL,
    key             text NOT NULL CHECK (length(key) BETWEEN 1 AND 255),
    fingerprint     bytea NOT NULL,
    operation       text NOT NULL,
    path_id         text NOT NULL,
    request_body    bytea NOT NULL,
    api_version     text NOT NULL,
    key_id          text NOT NULL,
    key_kind        text NOT NULL,
    key_scopes      text[] NOT NULL,
    recovery_point  text NOT NULL,
    lock_token      uuid,
    locked_at       timestamptz,
    response_status integer,
    -- Sealed: a response can carry a secret shown once, such as a new API key.
    response_body   bytea,
    created_at      timestamptz NOT NULL,
    last_run_at     timestamptz NOT NULL,
    UNIQUE (merchant_id, livemode, key),
    CHECK ((response_status IS NULL) = (response_body IS NULL)),
    CHECK ((lock_token IS NULL) = (locked_at IS NULL))
);

CREATE INDEX idempotency_keys_unfinished ON api.idempotency_keys (last_run_at) WHERE response_status IS NULL;
CREATE INDEX idempotency_keys_by_age ON api.idempotency_keys (created_at);

-- +goose Down
DROP TABLE api.idempotency_keys;
