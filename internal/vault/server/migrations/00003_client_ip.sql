-- +goose Up

-- The address a web page sent the card from, as the vault saw it: what Jupiter knows of
-- the cardholder that the merchant does not tell it.
ALTER TABLE vault.cards ADD COLUMN client_ip text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE vault.cards DROP COLUMN client_ip;
