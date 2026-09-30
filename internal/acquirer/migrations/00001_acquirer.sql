-- +goose Up

CREATE SCHEMA IF NOT EXISTS acquirer;

-- STANs identify messages on the wire and wrap around; RRNs identify transactions for
-- clearing and disputes. Both come from sequences so every process draws unique ones.
CREATE SEQUENCE acquirer.stan MINVALUE 1 MAXVALUE 999999 CYCLE;
CREATE SEQUENCE acquirer.rrn MINVALUE 1 MAXVALUE 99999999 CYCLE;

-- One row per rail operation, keyed by the rail's idempotency key, written before the
-- message is sent. Its state is what Query answers from, and what the store-and-forward
-- loop drives to an end:
--   authorize, refund: sending -> approved | declined | reversing -> reversed
--   capture:           sending -> acknowledged | declined | advising -> acknowledged | declined
--   void:              sending -> reversed | declined | reversing -> reversed
-- A request that times out is reversed (0420, repeated as 0421 until acknowledged); an
-- advice that times out is repeated (0221) until acknowledged.
CREATE TABLE acquirer.exchanges (
    key                    text PRIMARY KEY,
    kind                   text NOT NULL CHECK (kind IN ('authorize', 'capture', 'void', 'refund')),
    authorization_key      text,
    mti                    text NOT NULL,
    stan                   text NOT NULL,
    rrn                    text NOT NULL UNIQUE,
    transmitted_at         text NOT NULL,
    amount                 bigint NOT NULL CHECK (amount >= 0),
    installments           integer,
    merchant_code          text NOT NULL,
    state                  text NOT NULL CHECK (state IN ('sending', 'approved', 'declined', 'reversing',
                               'reversed', 'advising', 'acknowledged')),
    response_code          text NOT NULL DEFAULT '',
    authorization_code     text NOT NULL DEFAULT '',
    network_transaction_id text NOT NULL DEFAULT '',
    decline_code           text NOT NULL DEFAULT '',
    late_response_code     text NOT NULL DEFAULT '',
    forward_stan           text,
    forward_attempts       integer NOT NULL DEFAULT 0,
    next_forward_at        timestamptz,
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    CHECK ((state IN ('reversing', 'advising')) = (next_forward_at IS NOT NULL))
);

CREATE INDEX exchanges_to_forward ON acquirer.exchanges (next_forward_at) WHERE next_forward_at IS NOT NULL;
CREATE INDEX exchanges_by_stan ON acquirer.exchanges (stan);
CREATE INDEX exchanges_by_forward_stan ON acquirer.exchanges (forward_stan) WHERE forward_stan IS NOT NULL;

-- Each business day's clearing file is imported once. A record that does not match a
-- capture or refund is kept as an exception and tried again on later imports, since the
-- capture may still be waiting for the network's acknowledgement; what never matches is
-- left for reconciliation.
CREATE TABLE acquirer.clearing_files (
    business_date date PRIMARY KEY,
    sha256        bytea NOT NULL,
    records       integer NOT NULL,
    imported_at   timestamptz NOT NULL
);

CREATE TABLE acquirer.clearing_exceptions (
    id                     bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    business_date          date NOT NULL REFERENCES acquirer.clearing_files (business_date),
    kind                   text NOT NULL,
    rrn                    text NOT NULL,
    network_transaction_id text NOT NULL,
    amount                 bigint NOT NULL,
    merchant_code          text NOT NULL,
    reason                 text NOT NULL,
    resolved_at            timestamptz
);

CREATE INDEX clearing_exceptions_open ON acquirer.clearing_exceptions (id) WHERE resolved_at IS NULL;

-- +goose Down
DROP TABLE acquirer.clearing_exceptions;
DROP TABLE acquirer.clearing_files;
DROP TABLE acquirer.exchanges;
DROP SEQUENCE acquirer.rrn;
DROP SEQUENCE acquirer.stan;
