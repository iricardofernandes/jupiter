-- +goose Up

CREATE SCHEMA IF NOT EXISTS authentication;

-- One row per 3-D Secure authentication Jupiter's 3DS server ran, keyed by its own
-- transaction id: what the directory server answered, and, for a challenge, where the
-- customer goes and returns.
CREATE TABLE authentication.transactions (
    server_trans_id      text PRIMARY KEY,
    attempt_id           text NOT NULL UNIQUE,
    intent_id            text NOT NULL,
    merchant_id          text NOT NULL,
    livemode             boolean NOT NULL,
    trans_status         text NOT NULL,
    ds_trans_id          text NOT NULL,
    acs_trans_id         text NOT NULL,
    acs_url              text NOT NULL,
    eci                  text NOT NULL,
    authentication_value text NOT NULL,
    return_url           text NOT NULL,
    result_status        text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL
);

-- +goose Down
DROP TABLE authentication.transactions;
