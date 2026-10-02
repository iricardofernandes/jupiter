-- +goose Up

-- The address a web page sent the card from, as the vault saw it, for the risk engine.
ALTER TABLE payments.payment_methods ADD COLUMN client_ip text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE payments.payment_methods DROP COLUMN client_ip;
