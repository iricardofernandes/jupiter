-- +goose Up

CREATE SCHEMA IF NOT EXISTS receivables;

-- A receivable unit: what the card network will pay for a merchant, in an arrangement,
-- on a settlement date (Res. BCB 264 art. 2º). The registry knows it by the merchant's
-- tax id; Jupiter by the merchant. value is constituted and net: fees, refunds and
-- chargebacks already taken out. version counts its changes; registered_version is the
-- one the registry last took. constituted_on is the day of the latest change, which the
-- registry's next-business-day deadline counts from.
CREATE TABLE receivables.units (
    id                 text PRIMARY KEY,
    merchant_id        text NOT NULL,
    livemode           boolean NOT NULL,
    arrangement        text NOT NULL,
    settlement_date    date NOT NULL,
    value              bigint NOT NULL CHECK (value >= 0),
    blocked            bigint NOT NULL DEFAULT 0 CHECK (blocked >= 0 AND blocked <= value),
    currency           text NOT NULL,
    constituted_on     date NOT NULL,
    version            integer NOT NULL DEFAULT 1,
    registered_version integer NOT NULL DEFAULT 0,
    registered_at      timestamptz,
    register_error     text NOT NULL DEFAULT '',
    -- A unit the registry refused, or that cannot be sent, waits until then to be tried
    -- again, so it never holds up the others.
    register_retry_at  timestamptz,
    settled_on         date,
    settled_amount     bigint,
    payments           jsonb,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    UNIQUE (merchant_id, livemode, arrangement, settlement_date)
);

CREATE INDEX units_to_register ON receivables.units (livemode, updated_at) WHERE version > registered_version;
CREATE INDEX units_unregistered_since ON receivables.units (constituted_on) WHERE version > registered_version;

-- An installment of a captured card payment: the share of one unit it constituted.
CREATE TABLE receivables.installments (
    attempt_id     text NOT NULL,
    number         integer NOT NULL CHECK (number > 0),
    payment_intent text NOT NULL,
    unit_id        text NOT NULL REFERENCES receivables.units (id),
    gross          bigint NOT NULL CHECK (gross >= 0),
    fee            bigint NOT NULL CHECK (fee >= 0 AND fee <= gross),
    net            bigint NOT NULL CHECK (net = gross - fee),
    reduced        bigint NOT NULL DEFAULT 0 CHECK (reduced >= 0 AND reduced <= net),
    PRIMARY KEY (attempt_id, number)
);

CREATE INDEX installments_by_intent ON receivables.installments (payment_intent);

-- Every change to a unit's value or block, so its agenda can be read as of any moment.
CREATE TABLE receivables.unit_events (
    id        bigserial PRIMARY KEY,
    unit_id   text NOT NULL REFERENCES receivables.units (id),
    at        timestamptz NOT NULL,
    kind      text NOT NULL CHECK (kind IN ('constituted', 'reduced', 'uncovered', 'settled')),
    amount    bigint NOT NULL,
    reference text NOT NULL
);

CREATE INDEX unit_events_by_unit ON receivables.unit_events (unit_id, at);

-- What the registry said was committed to contracts on a unit, each time it changed.
CREATE TABLE receivables.effect_snapshots (
    id          bigserial PRIMARY KEY,
    unit_id     text NOT NULL REFERENCES receivables.units (id),
    observed_at timestamptz NOT NULL,
    commitments jsonb NOT NULL
);

CREATE INDEX effect_snapshots_by_unit ON receivables.effect_snapshots (unit_id, observed_at);

-- A merchant's authorization for a financier to see its agenda at the registry.
CREATE TABLE receivables.opt_ins (
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    financier   text NOT NULL,
    active      boolean NOT NULL,
    synced      boolean NOT NULL DEFAULT false,
    sync_error  text NOT NULL DEFAULT '',
    retry_at    timestamptz,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (merchant_id, livemode, financier)
);

-- The reconciliations with the registry that ran, by kind and day.
CREATE TABLE receivables.reconciliations (
    livemode    boolean NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('daily', 'weekly', 'fortnightly')),
    ran_on      date NOT NULL,
    ran_at      timestamptz NOT NULL,
    divergences integer NOT NULL,
    PRIMARY KEY (livemode, kind, ran_on)
);

-- What a reconciliation found that does not match, until it does.
CREATE TABLE receivables.divergences (
    id          bigserial PRIMARY KEY,
    livemode    boolean NOT NULL,
    kind        text NOT NULL,
    subject     text NOT NULL,
    detail      text NOT NULL,
    found_at    timestamptz NOT NULL,
    resolved_at timestamptz
);

CREATE UNIQUE INDEX divergences_open ON receivables.divergences (livemode, kind, subject) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE receivables.divergences;
DROP TABLE receivables.reconciliations;
DROP TABLE receivables.opt_ins;
DROP TABLE receivables.effect_snapshots;
DROP TABLE receivables.unit_events;
DROP TABLE receivables.installments;
DROP TABLE receivables.units;
