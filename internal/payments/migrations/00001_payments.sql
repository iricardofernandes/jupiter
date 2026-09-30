-- +goose Up

CREATE SCHEMA IF NOT EXISTS payments;

CREATE TABLE payments.intents (
    id                  text PRIMARY KEY,
    merchant_id         text NOT NULL,
    livemode            boolean NOT NULL,
    amount              bigint NOT NULL CHECK (amount > 0),
    currency            text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    capture_method      text NOT NULL CHECK (capture_method IN ('automatic', 'manual')),
    status              text NOT NULL CHECK (status IN ('requires_payment_method', 'requires_confirmation',
                            'requires_action', 'processing', 'requires_capture', 'succeeded', 'canceled')),
    payment_method      text NOT NULL,
    description         text NOT NULL,
    amount_capturable   bigint NOT NULL DEFAULT 0 CHECK (amount_capturable >= 0 AND amount_capturable <= amount),
    amount_received     bigint NOT NULL DEFAULT 0 CHECK (amount_received >= 0 AND amount_received <= amount),
    amount_refunded     bigint NOT NULL DEFAULT 0 CHECK (amount_refunded >= 0 AND amount_refunded <= amount_received),
    latest_attempt      text,
    last_error_code     text NOT NULL DEFAULT '',
    last_decline_code   text NOT NULL DEFAULT '',
    last_error_message  text NOT NULL DEFAULT '',
    next_action         text NOT NULL DEFAULT '',
    cancellation_reason text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    CHECK (status <> 'succeeded' OR amount_received > 0),
    CHECK (status = 'requires_capture' OR amount_capturable = 0)
);

CREATE INDEX intents_by_merchant ON payments.intents (merchant_id, livemode, id);

-- One try at paying an intent. A decline ends it and the intent may be tried again with
-- another payment method, so an intent keeps every attempt as its history. The attempt
-- id is the idempotency key of every call to the rail made on its behalf.
CREATE TABLE payments.attempts (
    id                       text PRIMARY KEY,
    intent_id                text NOT NULL REFERENCES payments.intents (id),
    number                   integer NOT NULL CHECK (number > 0),
    payment_method           text NOT NULL,
    amount                   bigint NOT NULL CHECK (amount > 0),
    status                   text NOT NULL CHECK (status IN ('authorizing', 'authorization_unknown',
                                 'requires_action', 'authorized', 'declined', 'failed', 'capturing',
                                 'capture_unknown', 'captured', 'voiding', 'void_unknown', 'voided')),
    authenticated            boolean NOT NULL DEFAULT false,
    rail_reference           text NOT NULL DEFAULT '',
    decline_code             text NOT NULL DEFAULT '',
    ledger_hold              text,
    capture_amount           bigint CHECK (capture_amount > 0 AND capture_amount <= amount),
    amount_captured          bigint NOT NULL DEFAULT 0 CHECK (amount_captured >= 0 AND amount_captured <= amount),
    authorization_expires_at timestamptz,
    unknown_since            timestamptz,
    resolutions              integer NOT NULL DEFAULT 0,
    created_at               timestamptz NOT NULL,
    updated_at               timestamptz NOT NULL,
    UNIQUE (intent_id, number)
);

CREATE INDEX attempts_to_resolve ON payments.attempts (updated_at)
    WHERE status IN ('authorizing', 'authorization_unknown', 'capturing', 'capture_unknown', 'voiding', 'void_unknown');
CREATE INDEX attempts_to_expire ON payments.attempts (authorization_expires_at) WHERE status = 'authorized';

CREATE TABLE payments.refunds (
    id             text PRIMARY KEY,
    intent_id      text NOT NULL REFERENCES payments.intents (id),
    attempt_id     text NOT NULL REFERENCES payments.attempts (id),
    merchant_id    text NOT NULL,
    livemode       boolean NOT NULL,
    amount         bigint NOT NULL CHECK (amount > 0),
    currency       text NOT NULL,
    reason         text NOT NULL,
    status         text NOT NULL CHECK (status IN ('pending', 'refund_unknown', 'succeeded', 'failed')),
    rail_reference text NOT NULL DEFAULT '',
    failure_reason text NOT NULL DEFAULT '',
    ledger_txn     text,
    unknown_since  timestamptz,
    resolutions    integer NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL,
    updated_at     timestamptz NOT NULL
);

CREATE INDEX refunds_by_merchant ON payments.refunds (merchant_id, livemode, id);
CREATE INDEX refunds_by_intent ON payments.refunds (intent_id);
CREATE INDEX refunds_to_resolve ON payments.refunds (updated_at) WHERE status IN ('pending', 'refund_unknown');

-- The ledger accounts each merchant, mode and currency uses. Platform accounts have an
-- empty merchant_id.
CREATE TABLE payments.ledger_accounts (
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    currency    text NOT NULL,
    role        text NOT NULL CHECK (role IN ('merchant_balance', 'network_receivable')),
    account_id  text NOT NULL,
    PRIMARY KEY (merchant_id, livemode, currency, role)
);

-- The test rail's memory of every request it received, keyed by the caller's
-- idempotency key, so that it answers a repeated request as it did the first time.
-- Captures, voids and refunds name the authorization they act on (authorization_key),
-- which the rail checks as a real network would.
CREATE TABLE payments.test_rail (
    key               text PRIMARY KEY,
    kind              text NOT NULL CHECK (kind IN ('authorize', 'capture', 'void', 'refund')),
    authorization_key text NOT NULL DEFAULT '',
    amount            bigint NOT NULL,
    status            text NOT NULL,
    reference         text NOT NULL,
    detail            text NOT NULL DEFAULT '',
    calls             integer NOT NULL DEFAULT 1,
    queries           integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL
);

CREATE INDEX test_rail_by_authorization ON payments.test_rail (authorization_key) WHERE authorization_key <> '';

-- +goose Down
DROP TABLE payments.test_rail;
DROP TABLE payments.ledger_accounts;
DROP TABLE payments.refunds;
DROP TABLE payments.attempts;
DROP TABLE payments.intents;
