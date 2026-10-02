-- +goose Up

-- What a balance's payouts asked for took today, and the payouts held for an operator.
CREATE INDEX payouts_of_the_day ON payments.payouts (merchant_id, livemode, recipient_id, created_at) WHERE scheduled_on IS NULL;
CREATE INDEX payouts_held ON payments.payouts (id) WHERE status = 'held';

-- +goose Down
DROP INDEX payments.payouts_held;
DROP INDEX payments.payouts_of_the_day;
