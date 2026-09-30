-- +goose Up

CREATE SCHEMA IF NOT EXISTS merchant;

CREATE TABLE merchant.merchants (
    id          text PRIMARY KEY,
    name        text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    api_version text NOT NULL,
    created_at  timestamptz NOT NULL
);

-- Only a SHA-256 of each key is stored. Keys carry 256 random bits, so a slow password
-- hash would add nothing, and a plain hash can be looked up by index.
CREATE TABLE merchant.api_keys (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL REFERENCES merchant.merchants (id),
    livemode    boolean NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('secret', 'publishable', 'restricted')),
    name        text NOT NULL,
    hash        bytea NOT NULL UNIQUE CHECK (length(hash) = 32),
    last4       text NOT NULL,
    scopes      text[] NOT NULL,
    created_at  timestamptz NOT NULL,
    revoked_at  timestamptz
);

CREATE INDEX api_keys_by_merchant ON merchant.api_keys (merchant_id, livemode, id);

-- +goose Down
DROP TABLE merchant.api_keys;
DROP TABLE merchant.merchants;
