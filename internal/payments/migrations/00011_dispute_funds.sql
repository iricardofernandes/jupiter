-- +goose Up

-- What a dispute took from a payment's merchant balance, keyed by the dispute: held while
-- a MED claim is analysed, withdrawn when the network or the bank took it, reinstated
-- when it came back, released when a hold ended unused. split_back is what the payment's
-- recipients gave back to the merchant's balance for it, as with refunds.
CREATE TABLE payments.dispute_funds (
    reference   text PRIMARY KEY,
    intent_id   text NOT NULL REFERENCES payments.intents (id),
    attempt_id  text NOT NULL REFERENCES payments.attempts (id),
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    currency    text NOT NULL,
    amount      bigint NOT NULL CHECK (amount > 0),
    split_back  bigint NOT NULL DEFAULT 0 CHECK (split_back >= 0 AND split_back <= amount),
    status      text NOT NULL CHECK (status IN ('held', 'withdrawn', 'reinstated', 'released')),
    ledger_hold text,
    ledger_txn  text,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    CHECK (status <> 'held' OR ledger_hold IS NOT NULL)
);

CREATE INDEX dispute_funds_by_intent ON payments.dispute_funds (intent_id);
CREATE INDEX dispute_funds_by_merchant ON payments.dispute_funds (merchant_id, livemode, currency);

-- +goose Down
DROP TABLE payments.dispute_funds;
