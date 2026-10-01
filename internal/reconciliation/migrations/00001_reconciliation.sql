-- +goose Up

CREATE SCHEMA IF NOT EXISTS reconciliation;

-- A movement of money as one side recorded it: Jupiter (from the ledger's postings and
-- the records that name them) or a counterparty (a clearing file, a settlement grade, a
-- CNAB return, a statement). identity names it on its side: Jupiter's object, or the
-- counterparty's line, so a duplicate line is a record of its own. key is what both sides
-- know it by. A record is matched to one of the other side's, or settled by the break an
-- operator resolved.
CREATE TABLE reconciliation.records (
    id           bigserial PRIMARY KEY,
    livemode     boolean NOT NULL,
    counterparty text NOT NULL,
    stream       text NOT NULL,
    side         text NOT NULL CHECK (side IN ('jupiter', 'counterparty')),
    identity     text NOT NULL,
    key          text NOT NULL,
    direction    text NOT NULL CHECK (direction IN ('in', 'out')),
    amount       bigint NOT NULL CHECK (amount > 0),
    value_date   date NOT NULL,
    merchant_id  text NOT NULL DEFAULT '',
    reference    text NOT NULL DEFAULT '',
    matched_with bigint REFERENCES reconciliation.records (id),
    match_rule   text NOT NULL DEFAULT '',
    matched_on   date,
    settled_by   text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL,
    UNIQUE (livemode, counterparty, stream, side, identity)
);

CREATE INDEX records_unmatched ON reconciliation.records (livemode, counterparty, stream) WHERE matched_with IS NULL AND settled_by = '';
CREATE INDEX records_by_key ON reconciliation.records (livemode, counterparty, stream, key);
CREATE INDEX records_by_merchant ON reconciliation.records (merchant_id, livemode, value_date);

-- What does not match, until it does or someone resolves it: a record missing on the
-- other side, a duplicate, a record of another amount, a probable match to confirm, or a
-- divergence another reconciliation found (the registry's).
CREATE TABLE reconciliation.breaks (
    id              text PRIMARY KEY,
    livemode        boolean NOT NULL,
    counterparty    text NOT NULL,
    stream          text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('missing_at_counterparty', 'missing_at_jupiter', 'duplicate', 'amount_mismatch', 'probable_match', 'divergence')),
    record_id       bigint REFERENCES reconciliation.records (id),
    other_record_id bigint REFERENCES reconciliation.records (id),
    key             text NOT NULL DEFAULT '',
    subject         text NOT NULL DEFAULT '',
    detail          text NOT NULL DEFAULT '',
    merchant_id     text NOT NULL DEFAULT '',
    amount          bigint NOT NULL DEFAULT 0,
    value_date      date,
    score           integer NOT NULL DEFAULT 0,
    reasons         jsonb NOT NULL DEFAULT '[]',
    status          text NOT NULL CHECK (status IN ('open', 'resolved')),
    resolution      text NOT NULL DEFAULT '',
    opened_on       date NOT NULL,
    resolved_on     date,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);

CREATE UNIQUE INDEX breaks_open_record ON reconciliation.breaks (record_id) WHERE status = 'open' AND record_id IS NOT NULL;
CREATE UNIQUE INDEX breaks_open_subject ON reconciliation.breaks (livemode, counterparty, subject) WHERE status = 'open' AND subject <> '';
CREATE INDEX breaks_by_merchant ON reconciliation.breaks (merchant_id, livemode, status);

-- Each mode's reconciliations, by the day they reconciled through.
CREATE TABLE reconciliation.runs (
    livemode boolean NOT NULL,
    day      date NOT NULL,
    ran_at   timestamptz NOT NULL,
    matched  integer NOT NULL,
    opened   integer NOT NULL,
    resolved integer NOT NULL,
    PRIMARY KEY (livemode, day)
);

-- +goose Down
DROP TABLE reconciliation.runs;
DROP TABLE reconciliation.breaks;
DROP TABLE reconciliation.records;
