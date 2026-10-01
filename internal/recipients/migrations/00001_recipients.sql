-- +goose Up

CREATE SCHEMA IF NOT EXISTS recipients;

-- A recipient: someone a merchant's payments are split to, a seller in a marketplace,
-- or the merchant itself (is_default). Its receivables are registered under its CPF or
-- CNPJ, so a tax id is one recipient in each mode.
CREATE TABLE recipients.recipients (
    id                        text PRIMARY KEY,
    merchant_id               text NOT NULL,
    livemode                  boolean NOT NULL,
    name                      text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    tax_id                    text NOT NULL,
    is_default                boolean NOT NULL DEFAULT false,
    status                    text NOT NULL CHECK (status IN ('pending', 'verified', 'rejected')),
    payout_method             text NOT NULL DEFAULT '' CHECK (payout_method IN ('', 'pix', 'bank_account')),
    pix_key                   text NOT NULL DEFAULT '',
    bank_ispb                 text NOT NULL DEFAULT '',
    bank_branch               text NOT NULL DEFAULT '',
    bank_account              text NOT NULL DEFAULT '',
    transfer_interval         text NOT NULL DEFAULT 'manual' CHECK (transfer_interval IN ('manual', 'daily', 'weekly', 'monthly')),
    transfer_day              integer NOT NULL DEFAULT 0,
    auto_anticipation         boolean NOT NULL DEFAULT false,
    auto_anticipation_delay   integer NOT NULL DEFAULT 1 CHECK (auto_anticipation_delay BETWEEN 1 AND 30),
    created_at                timestamptz NOT NULL,
    updated_at                timestamptz NOT NULL
);

CREATE UNIQUE INDEX recipients_by_tax_id ON recipients.recipients (livemode, tax_id);
CREATE UNIQUE INDEX recipients_default ON recipients.recipients (merchant_id, livemode) WHERE is_default;
CREATE INDEX recipients_by_merchant ON recipients.recipients (merchant_id, livemode, id);
CREATE INDEX recipients_auto_anticipation ON recipients.recipients (livemode) WHERE auto_anticipation AND status = 'verified';

-- +goose Down
DROP TABLE recipients.recipients;
