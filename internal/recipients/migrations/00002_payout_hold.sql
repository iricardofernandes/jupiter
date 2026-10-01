-- +goose Up

-- An operator's hold on a recipient's payouts: they wait, held, until it is lifted.
ALTER TABLE recipients.recipients ADD COLUMN payouts_held boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE recipients.recipients DROP COLUMN payouts_held;
