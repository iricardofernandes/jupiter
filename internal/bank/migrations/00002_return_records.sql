-- +goose Up

-- Every title record of every return file read, by its place in the file: what the bank
-- said, for reconciliation, duplicates included.
CREATE TABLE bank.return_records (
    livemode        boolean NOT NULL,
    return_sequence bigint NOT NULL,
    line            integer NOT NULL,
    our_number      text NOT NULL,
    occurrence      integer NOT NULL,
    paid            bigint NOT NULL,
    occurred_on     date,
    credit_on       date,
    imported_on     date NOT NULL,
    PRIMARY KEY (livemode, return_sequence, line)
);

CREATE INDEX return_records_by_day ON bank.return_records (livemode, imported_on);

-- When a paid boleto's money is credited to the account, as its return said.
ALTER TABLE bank.titles ADD COLUMN credit_on date;

-- +goose Down
ALTER TABLE bank.titles DROP COLUMN credit_on;
DROP TABLE bank.return_records;
