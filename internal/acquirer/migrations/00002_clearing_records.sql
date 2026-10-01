-- +goose Up

-- Every record of every clearing file imported, by its line: what the network said, for
-- reconciliation, duplicates included.
CREATE TABLE acquirer.clearing_records (
    business_date          date NOT NULL REFERENCES acquirer.clearing_files (business_date),
    line                   integer NOT NULL,
    kind                   text NOT NULL,
    rrn                    text NOT NULL,
    network_transaction_id text NOT NULL,
    amount                 bigint NOT NULL,
    merchant_code          text NOT NULL,
    PRIMARY KEY (business_date, line)
);

CREATE INDEX exchanges_cleared ON acquirer.exchanges (created_at) WHERE (kind = 'capture' AND state = 'acknowledged') OR (kind = 'refund' AND state = 'approved');

-- +goose Down
DROP INDEX acquirer.exchanges_cleared;
DROP TABLE acquirer.clearing_records;
