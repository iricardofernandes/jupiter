-- +goose Up

CREATE SCHEMA IF NOT EXISTS bank;

-- A boleto Jupiter issued, by the attempt that pays with it: numbered (nosso número)
-- from the mode's sequence, sent to the bank in a remittance, and followed through the
-- bank's returns.
CREATE TABLE bank.titles (
    attempt_id          text PRIMARY KEY,
    livemode            boolean NOT NULL,
    sequence            bigint NOT NULL,
    our_number          text NOT NULL,
    barcode             text NOT NULL,
    line                text NOT NULL,
    due                 date NOT NULL,
    amount              bigint NOT NULL CHECK (amount > 0),
    payer_name          text NOT NULL,
    payer_tax_id        text NOT NULL,
    hybrid              boolean NOT NULL,
    days_after_due      integer NOT NULL,
    status              text NOT NULL CHECK (status IN ('issued', 'remitted', 'registered', 'rejected', 'paid', 'written_off')),
    remittance          bigint,
    write_off_requested boolean NOT NULL DEFAULT false,
    write_off_remittance bigint,
    pix_code            text NOT NULL DEFAULT '',
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL,
    UNIQUE (livemode, sequence),
    UNIQUE (livemode, our_number)
);

-- A remittance, kept as sent: sending it again sends the same bytes.
CREATE TABLE bank.remittances (
    livemode   boolean NOT NULL,
    sequence   bigint NOT NULL,
    data       bytea NOT NULL,
    sent       boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (livemode, sequence)
);

-- The bank's return files Jupiter read.
CREATE TABLE bank.returns (
    livemode    boolean NOT NULL,
    sequence    bigint NOT NULL,
    titles      integer NOT NULL,
    imported_at timestamptz NOT NULL,
    PRIMARY KEY (livemode, sequence)
);

-- What a return said that Jupiter could not apply, for an operator: a payment of another
-- amount than the boleto's, of a boleto no longer waited on or not Jupiter's, or for
-- another account.
CREATE TABLE bank.exceptions (
    id              bigserial PRIMARY KEY,
    livemode        boolean NOT NULL,
    return_sequence bigint NOT NULL,
    our_number      text NOT NULL,
    attempt_id      text NOT NULL DEFAULT '',
    kind            text NOT NULL CHECK (kind IN ('amount_mismatch', 'paid_not_waiting', 'unknown_title', 'foreign_account')),
    amount          bigint NOT NULL DEFAULT 0,
    detail          text NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL
);

-- +goose Down
DROP TABLE bank.exceptions;
DROP TABLE bank.returns;
DROP TABLE bank.remittances;
DROP TABLE bank.titles;
