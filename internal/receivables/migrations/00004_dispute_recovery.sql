-- +goose Up

-- A dispute takes its amount from the liable recipient's available balance; what that
-- leaves owing is recovered by reducing the recipient's future units (dispute and
-- recovery movements).
ALTER TABLE receivables.movements DROP CONSTRAINT movements_kind_check;
ALTER TABLE receivables.movements ADD CONSTRAINT movements_kind_check
    CHECK (kind IN ('split', 'refund', 'anticipation', 'settlement', 'payout', 'dispute', 'recovery'));

-- What each dispute took from a recipient, and gave back if it was won.
CREATE TABLE receivables.disputes (
    reference    text PRIMARY KEY,
    recipient_id text NOT NULL,
    livemode     boolean NOT NULL,
    currency     text NOT NULL,
    amount       bigint NOT NULL CHECK (amount > 0),
    reinstated   boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL
);

-- Each reduction of a unit to recover what a recipient owes: how much the registry's
-- order took from what was free and from other financiers' contracts.
CREATE TABLE receivables.recoveries (
    id             bigserial PRIMARY KEY,
    recipient_id   text NOT NULL,
    unit_id        text NOT NULL REFERENCES receivables.units (id),
    amount         bigint NOT NULL CHECK (amount > 0),
    from_free      bigint NOT NULL CHECK (from_free >= 0),
    from_contracts jsonb NOT NULL,
    at             timestamptz NOT NULL,
    CHECK (from_free <= amount)
);

CREATE INDEX recoveries_by_recipient ON receivables.recoveries (recipient_id, at);

-- +goose Down
DROP TABLE receivables.recoveries;
DROP TABLE receivables.disputes;
ALTER TABLE receivables.movements DROP CONSTRAINT movements_kind_check;
ALTER TABLE receivables.movements ADD CONSTRAINT movements_kind_check
    CHECK (kind IN ('split', 'refund', 'anticipation', 'settlement', 'payout'));
