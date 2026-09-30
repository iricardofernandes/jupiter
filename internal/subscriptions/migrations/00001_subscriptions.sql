-- +goose Up

CREATE SCHEMA IF NOT EXISTS subscriptions;

-- A subscription charges a customer by Pix Automático: a recurrence at Jupiter's bank,
-- which the customer authorizes once, and a payment intent per cycle, which their bank
-- debits on its due date. The recurrence's status at the bank drives the subscription's.
CREATE TABLE subscriptions.subscriptions (
    id                   text PRIMARY KEY,
    merchant_id          text NOT NULL,
    livemode             boolean NOT NULL,
    amount               bigint NOT NULL CHECK (amount > 0),
    currency             text NOT NULL,
    interval             text NOT NULL CHECK (interval IN ('week', 'month', 'quarter', 'half_year', 'year')),
    start_date           date NOT NULL,
    end_date             date,
    description          text NOT NULL,
    customer_name        text NOT NULL,
    customer_tax_id      text NOT NULL,
    retries              boolean NOT NULL,
    authorization_method text NOT NULL CHECK (authorization_method IN ('qr_code', 'payer_request')),
    payer_ispb           text NOT NULL DEFAULT '',
    payer_branch         text NOT NULL DEFAULT '',
    payer_account        text NOT NULL DEFAULT '',
    status               text NOT NULL CHECK (status IN ('incomplete', 'active', 'past_due', 'rejected', 'canceled', 'ended')),
    -- Until the bank has the recurrence, the setup is pending, and the worker repeats it;
    -- setup_attempted_at is when it last asked the bank to make one.
    set_up               boolean NOT NULL DEFAULT false,
    setup_attempted_at   timestamptz,
    recurrence_id        text NOT NULL DEFAULT '',
    qr_code              text NOT NULL DEFAULT '',
    canceled_by          text NOT NULL DEFAULT '' CHECK (canceled_by IN ('', 'merchant', 'customer')),
    end_code             text NOT NULL DEFAULT '',
    created_at           timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    CHECK (end_date IS NULL OR end_date >= start_date)
);

CREATE INDEX subscriptions_by_merchant ON subscriptions.subscriptions (merchant_id, livemode, id);
CREATE INDEX subscriptions_by_recurrence ON subscriptions.subscriptions (recurrence_id) WHERE recurrence_id <> '';
CREATE INDEX subscriptions_to_advance ON subscriptions.subscriptions (updated_at) WHERE status IN ('incomplete', 'active', 'past_due');

-- One payment per cycle: the payment intent charged by Pix Automático for it.
CREATE TABLE subscriptions.cycles (
    subscription_id text NOT NULL REFERENCES subscriptions.subscriptions (id),
    number          integer NOT NULL CHECK (number >= 0),
    due_date        date NOT NULL,
    payment_intent  text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'paid', 'failed', 'canceled')),
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    PRIMARY KEY (subscription_id, number)
);

-- +goose Down
DROP TABLE subscriptions.cycles;
DROP TABLE subscriptions.subscriptions;
