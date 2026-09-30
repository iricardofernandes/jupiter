-- +goose Up

-- A saved card. The number lives in the vault, which holds it under vault_token; this
-- table has only what PCI DSS allows outside it: the brand, the BIN, the last four
-- digits, the expiry, and the vault's keyed fingerprint, from which the fingerprint a
-- merchant sees is derived.
CREATE TABLE payments.payment_methods (
    id                text PRIMARY KEY,
    merchant_id       text NOT NULL,
    livemode          boolean NOT NULL,
    type              text NOT NULL CHECK (type = 'card'),
    vault_token       text NOT NULL UNIQUE,
    brand             text NOT NULL,
    bin               text NOT NULL CHECK (bin ~ '^[0-9]{6}([0-9]{2})?$'),
    last4             text NOT NULL CHECK (last4 ~ '^[0-9]{4}$'),
    exp_month         integer NOT NULL CHECK (exp_month BETWEEN 1 AND 12),
    exp_year          integer NOT NULL,
    vault_fingerprint text NOT NULL,
    created_at        timestamptz NOT NULL
);

CREATE INDEX payment_methods_by_merchant ON payments.payment_methods (merchant_id, livemode, id);

-- +goose Down
DROP TABLE payments.payment_methods;
