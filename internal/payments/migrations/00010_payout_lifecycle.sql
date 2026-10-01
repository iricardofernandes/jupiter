-- +goose Up

-- Payouts by bank transfer as well as by Pix; held until an operator releases them;
-- returned by the receiving bank after they were paid; and scheduled by a recipient's
-- transfer settings, once a day at most.
ALTER TABLE payments.payouts ADD COLUMN method text NOT NULL DEFAULT 'pix' CHECK (method IN ('pix', 'bank_transfer'));
ALTER TABLE payments.payouts ADD COLUMN bank_ispb text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts ADD COLUMN bank_branch text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts ADD COLUMN bank_account text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts ADD COLUMN holder_name text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts ADD COLUMN holder_tax_id text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts ADD COLUMN returned_at timestamptz;
ALTER TABLE payments.payouts ADD COLUMN scheduled_on date;
-- The account it was paid from, which a returned payout goes back to.
ALTER TABLE payments.payouts ADD COLUMN source_account text NOT NULL DEFAULT '';
ALTER TABLE payments.payouts DROP CONSTRAINT payouts_status_check;
ALTER TABLE payments.payouts ADD CONSTRAINT payouts_status_check CHECK (status IN ('held', 'sending', 'unknown', 'paid', 'failed', 'returned'));

CREATE UNIQUE INDEX payouts_scheduled ON payments.payouts (merchant_id, livemode, recipient_id, scheduled_on) WHERE scheduled_on IS NOT NULL;
CREATE INDEX payouts_to_watch ON payments.payouts (arrived_at) WHERE status = 'paid' AND method = 'bank_transfer';

-- +goose Down
DROP INDEX payments.payouts_to_watch;
DROP INDEX payments.payouts_scheduled;
ALTER TABLE payments.payouts DROP CONSTRAINT payouts_status_check;
ALTER TABLE payments.payouts ADD CONSTRAINT payouts_status_check CHECK (status IN ('sending', 'unknown', 'paid', 'failed'));
ALTER TABLE payments.payouts DROP COLUMN source_account;
ALTER TABLE payments.payouts DROP COLUMN scheduled_on;
ALTER TABLE payments.payouts DROP COLUMN returned_at;
ALTER TABLE payments.payouts DROP COLUMN holder_tax_id;
ALTER TABLE payments.payouts DROP COLUMN holder_name;
ALTER TABLE payments.payouts DROP COLUMN bank_account;
ALTER TABLE payments.payouts DROP COLUMN bank_branch;
ALTER TABLE payments.payouts DROP COLUMN bank_ispb;
ALTER TABLE payments.payouts DROP COLUMN method;
