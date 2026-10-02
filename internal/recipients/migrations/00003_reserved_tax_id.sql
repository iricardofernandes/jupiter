-- +goose Up

-- A recipient reserves its CPF or CNPJ in its mode once it is verified, and the merchant's
-- own from the start. It keeps it if a change sends it back to pending, since its
-- receivables are registered under it. One pending does not reserve it, so creating one
-- can neither keep a document from its holder nor tell whether another merchant has it.
ALTER TABLE recipients.recipients ADD COLUMN reserved boolean NOT NULL DEFAULT false;
UPDATE recipients.recipients SET reserved = true WHERE is_default OR status = 'verified';
DROP INDEX recipients.recipients_by_tax_id;
CREATE UNIQUE INDEX recipients_reserved_tax_id ON recipients.recipients (livemode, tax_id) WHERE reserved;

-- +goose Down
DROP INDEX recipients.recipients_reserved_tax_id;
CREATE UNIQUE INDEX recipients_by_tax_id ON recipients.recipients (livemode, tax_id);
ALTER TABLE recipients.recipients DROP COLUMN reserved;
