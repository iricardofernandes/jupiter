-- +goose Up

-- A network token the card network issued for the card: its number (a DPAN), encrypted
-- under the card's own data key like the card number, and its expiry and reference.
ALTER TABLE vault.cards
    ADD COLUMN network_token          bytea,
    ADD COLUMN network_token_month    integer,
    ADD COLUMN network_token_year     integer,
    ADD COLUMN network_token_reference text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE vault.cards
    DROP COLUMN network_token_reference, DROP COLUMN network_token_year, DROP COLUMN network_token_month,
    DROP COLUMN network_token;
