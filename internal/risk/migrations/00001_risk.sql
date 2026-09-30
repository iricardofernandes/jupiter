-- +goose Up

CREATE SCHEMA IF NOT EXISTS risk;

-- One row per payment attempt the engine saw, for the velocity counters: how often a
-- card, an address or a merchant tried to pay, and how often it was declined, over a
-- sliding window read with the indexes below. Cards are counted across merchants, by the
-- vault's fingerprint, which no merchant can forge; addresses only within a merchant,
-- since the merchant is who says what the customer's address is.
CREATE TABLE risk.attempts (
    attempt_id       text PRIMARY KEY,
    merchant_id      text NOT NULL,
    livemode         boolean NOT NULL,
    card_fingerprint text NOT NULL,
    ip               text NOT NULL,
    created_at       timestamptz NOT NULL,
    outcome          text NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'approved', 'declined', 'blocked'))
);

CREATE INDEX attempts_by_merchant ON risk.attempts (merchant_id, livemode, created_at);
CREATE INDEX attempts_by_card ON risk.attempts (card_fingerprint, created_at);
CREATE INDEX attempts_by_ip ON risk.attempts (merchant_id, ip, created_at) WHERE ip <> '';

-- Every decision, with the rules that fired and the features they saw: the decision log,
-- and the labelled data a model could one day be trained on.
CREATE TABLE risk.decisions (
    id           text PRIMARY KEY,
    attempt_id   text NOT NULL UNIQUE,
    intent_id    text NOT NULL,
    merchant_id  text NOT NULL,
    livemode     boolean NOT NULL,
    action       text NOT NULL CHECK (action IN ('allow', 'review', 'request_3ds', 'block')),
    rules        jsonb NOT NULL,
    features     jsonb NOT NULL,
    created_at   timestamptz NOT NULL
);

CREATE INDEX decisions_by_merchant ON risk.decisions (merchant_id, livemode, id);

-- A merchant's own rules, in the rules language, beside the platform's.
CREATE TABLE risk.rules (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    action      text NOT NULL CHECK (action IN ('allow', 'review', 'request_3ds', 'block')),
    expression  text NOT NULL CHECK (length(expression) BETWEEN 1 AND 1000),
    description text NOT NULL,
    created_at  timestamptz NOT NULL,
    deleted_at  timestamptz
);

CREATE INDEX rules_by_merchant ON risk.rules (merchant_id, livemode, id) WHERE deleted_at IS NULL;

CREATE TABLE risk.list_items (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    list        text NOT NULL CHECK (list IN ('allow', 'block')),
    kind        text NOT NULL CHECK (kind IN ('card_fingerprint', 'ip', 'bin')),
    value       text NOT NULL CHECK (length(value) BETWEEN 1 AND 100),
    created_at  timestamptz NOT NULL,
    UNIQUE (merchant_id, livemode, list, kind, value)
);

-- A merchant under card testing: while throttled, only a few attempts a minute go through.
CREATE TABLE risk.throttles (
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    started_at  timestamptz NOT NULL,
    until       timestamptz NOT NULL,
    ratio       double precision NOT NULL,
    attempts    integer NOT NULL,
    PRIMARY KEY (merchant_id, livemode, started_at)
);

-- +goose Down
DROP TABLE risk.throttles;
DROP TABLE risk.list_items;
DROP TABLE risk.rules;
DROP TABLE risk.decisions;
DROP TABLE risk.attempts;
