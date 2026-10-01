-- +goose Up

-- Units from before recipients were a merchant's, keyed by it; they cannot become a
-- recipient's here, where recipients live in another schema. Phase 9 never ran anywhere
-- but development databases, so it refuses rather than guess: reset the database.
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM receivables.units) THEN
        RAISE EXCEPTION 'receivables: units from before recipients exist; this migration needs an empty receivables schema';
    END IF;
END $$;
-- +goose StatementEnd

-- Units and installments belong to recipients: a payment split among sellers constitutes
-- each seller's units under its own CPF or CNPJ. anticipated is what Jupiter bought of a
-- unit; the rest, unsettled, is the recipient's pending balance.
ALTER TABLE receivables.units ADD COLUMN recipient_id text NOT NULL DEFAULT '';
ALTER TABLE receivables.units ADD COLUMN anticipated bigint NOT NULL DEFAULT 0;
ALTER TABLE receivables.units ADD CONSTRAINT units_anticipated_check CHECK (anticipated >= 0 AND anticipated <= value);
ALTER TABLE receivables.units DROP CONSTRAINT units_merchant_id_livemode_arrangement_settlement_date_key;
ALTER TABLE receivables.units ADD CONSTRAINT units_recipient_key UNIQUE (recipient_id, livemode, arrangement, settlement_date);

ALTER TABLE receivables.installments ADD COLUMN recipient_id text NOT NULL DEFAULT '';
ALTER TABLE receivables.installments DROP CONSTRAINT installments_pkey;
ALTER TABLE receivables.installments ADD PRIMARY KEY (attempt_id, recipient_id, number);

-- A capture's split, as typed lines: each recipient's share (commission for the
-- merchant's own), what of Jupiter's fee it paid, and the centavos rounding left over.
CREATE TABLE receivables.split_lines (
    attempt_id     text NOT NULL,
    payment_intent text NOT NULL,
    recipient_id   text NOT NULL,
    type           text NOT NULL CHECK (type IN ('recipient', 'commission', 'fee', 'remainder')),
    amount         bigint NOT NULL CHECK (amount >= 0),
    liable         boolean NOT NULL,
    ledger_txn     text NOT NULL,
    PRIMARY KEY (attempt_id, recipient_id, type)
);

CREATE INDEX split_lines_by_intent ON receivables.split_lines (payment_intent);

-- Each recipient's ledger accounts, and Jupiter's for anticipation fees (recipient '').
CREATE TABLE receivables.ledger_accounts (
    recipient_id text NOT NULL,
    livemode     boolean NOT NULL,
    currency     text NOT NULL,
    role         text NOT NULL CHECK (role IN ('pending', 'available', 'reserved', 'anticipation_fees', 'financiers')),
    account_id   text NOT NULL UNIQUE,
    PRIMARY KEY (recipient_id, livemode, currency, role)
);

-- Every amount posted to a recipient's accounts, by why: what the ledger must hold.
CREATE TABLE receivables.movements (
    id           bigserial PRIMARY KEY,
    recipient_id text NOT NULL,
    livemode     boolean NOT NULL,
    currency     text NOT NULL,
    bucket       text NOT NULL CHECK (bucket IN ('pending', 'available', 'reserved')),
    kind         text NOT NULL CHECK (kind IN ('split', 'refund', 'anticipation', 'settlement', 'payout')),
    amount       bigint NOT NULL,
    reference    text NOT NULL,
    at           timestamptz NOT NULL
);

CREATE INDEX movements_by_recipient ON receivables.movements (recipient_id, livemode, currency, bucket);

-- A quote for anticipating units, good until it expires, and used once.
CREATE TABLE receivables.anticipation_quotes (
    id           text PRIMARY KEY,
    merchant_id  text NOT NULL,
    livemode     boolean NOT NULL,
    recipient_id text NOT NULL,
    monthly_rate text NOT NULL,
    units        jsonb NOT NULL,
    amount       bigint NOT NULL,
    price        bigint NOT NULL,
    expires_at   timestamptz NOT NULL,
    anticipation text,
    created_at   timestamptz NOT NULL
);

-- An anticipation: units Jupiter bought from a recipient, for their present value.
CREATE TABLE receivables.anticipations (
    id           text PRIMARY KEY,
    merchant_id  text NOT NULL,
    livemode     boolean NOT NULL,
    recipient_id text NOT NULL,
    currency     text NOT NULL,
    amount       bigint NOT NULL CHECK (amount > 0),
    price        bigint NOT NULL CHECK (price > 0 AND price <= amount),
    monthly_rate text NOT NULL,
    automatic    boolean NOT NULL,
    ledger_txn   text NOT NULL,
    created_at   timestamptz NOT NULL
);

CREATE INDEX anticipations_by_merchant ON receivables.anticipations (merchant_id, livemode, id);

CREATE TABLE receivables.anticipation_units (
    anticipation_id text NOT NULL REFERENCES receivables.anticipations (id),
    unit_id         text NOT NULL REFERENCES receivables.units (id),
    amount          bigint NOT NULL CHECK (amount > 0),
    price           bigint NOT NULL CHECK (price > 0),
    days            integer NOT NULL,
    contract        text NOT NULL,
    PRIMARY KEY (anticipation_id, unit_id)
);

-- +goose Down
DROP TABLE receivables.anticipation_units;
DROP TABLE receivables.anticipations;
DROP TABLE receivables.anticipation_quotes;
DROP TABLE receivables.movements;
DROP TABLE receivables.ledger_accounts;
DROP TABLE receivables.split_lines;
ALTER TABLE receivables.installments DROP CONSTRAINT installments_pkey;
ALTER TABLE receivables.installments ADD PRIMARY KEY (attempt_id, number);
ALTER TABLE receivables.installments DROP COLUMN recipient_id;
ALTER TABLE receivables.units DROP CONSTRAINT units_recipient_key;
ALTER TABLE receivables.units ADD CONSTRAINT units_merchant_id_livemode_arrangement_settlement_date_key UNIQUE (merchant_id, livemode, arrangement, settlement_date);
ALTER TABLE receivables.units DROP CONSTRAINT units_anticipated_check;
ALTER TABLE receivables.units DROP COLUMN anticipated;
ALTER TABLE receivables.units DROP COLUMN recipient_id;
