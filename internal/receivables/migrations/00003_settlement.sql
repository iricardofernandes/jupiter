-- +goose Up

-- The part of an installment's fee given back by refunds: what the network no longer
-- pays Jupiter for it.
ALTER TABLE receivables.installments ADD COLUMN fee_reduced bigint NOT NULL DEFAULT 0;
ALTER TABLE receivables.installments ADD CONSTRAINT installments_fee_reduced_check CHECK (fee_reduced >= 0 AND fee_reduced <= fee);

-- A day's settlement through the SLC: built from the units settled that day, submitted,
-- then settled, when its cash is posted. The units it carries name it.
CREATE TABLE receivables.grades (
    livemode   boolean NOT NULL,
    date       date NOT NULL,
    entries    jsonb NOT NULL,
    total      bigint NOT NULL CHECK (total > 0),
    credited   bigint NOT NULL CHECK (credited >= 0 AND credited <= total),
    status     text NOT NULL CHECK (status IN ('built', 'submitted', 'settled', 'refused')),
    error      text NOT NULL DEFAULT '',
    ledger_txn text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (livemode, date)
);

CREATE INDEX grades_open ON receivables.grades (livemode, date) WHERE status IN ('built', 'submitted');

ALTER TABLE receivables.units ADD COLUMN grade_date date;

-- When each anticipated unit was reported to the SLC, which must have it by the business
-- day after.
ALTER TABLE receivables.anticipation_units ADD COLUMN reported_at timestamptz;
CREATE INDEX anticipation_units_to_report ON receivables.anticipation_units (anticipation_id) WHERE reported_at IS NULL;

-- +goose Down
DROP INDEX receivables.anticipation_units_to_report;
ALTER TABLE receivables.anticipation_units DROP COLUMN reported_at;
ALTER TABLE receivables.units DROP COLUMN grade_date;
DROP TABLE receivables.grades;
ALTER TABLE receivables.installments DROP CONSTRAINT installments_fee_reduced_check;
ALTER TABLE receivables.installments DROP COLUMN fee_reduced;
