-- +goose Up

CREATE SCHEMA IF NOT EXISTS vault;

-- One row per token. The number is encrypted under a data key of its own, and the data
-- key is stored only wrapped by a key-encryption key the KMS holds, named by key_id.
-- The security code has no column: it is held in memory only.
--
-- A token made from a web page has no owner until the merchant's server claims it, and
-- is deleted if that does not happen before claim_expires_at.
CREATE TABLE vault.cards (
    token            text PRIMARY KEY,
    owner            text,
    request_key      text,
    publishable_key  text,
    claim_expires_at timestamptz,
    -- HMAC-SHA256 of the number under a key the KMS holds, so the digits around the
    -- BIN and last four cannot be recovered by hashing the few million candidates.
    fingerprint      bytea NOT NULL CHECK (length(fingerprint) = 32),
    brand            text NOT NULL,
    bin              text NOT NULL CHECK (bin ~ '^[0-9]{6}([0-9]{2})?$'),
    last4            text NOT NULL CHECK (last4 ~ '^[0-9]{4}$'),
    exp_month        integer NOT NULL CHECK (exp_month BETWEEN 1 AND 12),
    exp_year         integer NOT NULL,
    encrypted_number bytea NOT NULL,
    wrapped_key      bytea NOT NULL,
    key_id           text NOT NULL,
    created_at       timestamptz NOT NULL,
    UNIQUE (owner, request_key),
    CHECK ((owner IS NULL) = (claim_expires_at IS NOT NULL)),
    CHECK (owner IS NOT NULL OR publishable_key IS NOT NULL)
);

CREATE INDEX cards_by_key ON vault.cards (key_id);
CREATE INDEX cards_unclaimed ON vault.cards (claim_expires_at) WHERE owner IS NULL;

-- +goose Down
DROP TABLE vault.cards;
