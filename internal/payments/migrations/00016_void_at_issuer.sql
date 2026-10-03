-- +goose Up

-- An attempt whose capture the rail refused: its authorization may still hold the
-- cardholder's limit at the issuer, and is voided there in the background.
ALTER TABLE payments.attempts ADD COLUMN void_at_issuer boolean NOT NULL DEFAULT false;
CREATE INDEX attempts_to_void_at_issuer ON payments.attempts (updated_at) WHERE void_at_issuer;

-- +goose Down
DROP INDEX payments.attempts_to_void_at_issuer;
ALTER TABLE payments.attempts DROP COLUMN void_at_issuer;
