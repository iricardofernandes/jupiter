-- +goose Up

-- The merchant's CPF or CNPJ: who its receivables belong to at the registry.
ALTER TABLE merchant.merchants ADD COLUMN tax_id text NOT NULL DEFAULT '';
-- A registry keys receivables by the holder's tax id, so one tax id is one merchant.
CREATE UNIQUE INDEX merchants_by_tax_id ON merchant.merchants (tax_id) WHERE tax_id <> '';

-- +goose Down
DROP INDEX merchant.merchants_by_tax_id;
ALTER TABLE merchant.merchants DROP COLUMN tax_id;
