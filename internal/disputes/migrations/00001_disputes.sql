-- +goose Up

CREATE SCHEMA IF NOT EXISTS disputes;

-- A dispute of a captured payment: a card chargeback the network opened, or a MED claim
-- the payer's bank made on a Pix. network is the card scheme, or pix; network_id the
-- network's case, or the infraction report; network_version the last change applied.
--   stage:  chargeback -> pre_arbitration -> arbitration, for cards;
--           med_analysis -> med_contestation, for Pix.
--   status: needs_response (the merchant can act until due_by), under_review (waiting on
--           the network, the bank or an operator), won, lost.
-- network_due_by is when Jupiter must have answered the network or the bank;
-- pending_action, what it has still to tell them, sent until they take it.
CREATE TABLE disputes.disputes (
    id                     text PRIMARY KEY,
    merchant_id            text NOT NULL,
    livemode               boolean NOT NULL,
    payment_intent         text NOT NULL,
    attempt_id             text NOT NULL,
    kind                   text NOT NULL CHECK (kind IN ('chargeback', 'med')),
    network                text NOT NULL,
    network_id             text NOT NULL,
    network_transaction_id text NOT NULL,
    reason                 text NOT NULL,
    reason_code            text NOT NULL,
    amount                 bigint NOT NULL CHECK (amount > 0),
    currency               text NOT NULL,
    stage                  text NOT NULL CHECK (stage IN ('chargeback', 'pre_arbitration', 'arbitration', 'med_analysis', 'med_contestation')),
    status                 text NOT NULL CHECK (status IN ('needs_response', 'under_review', 'won', 'lost')),
    liability              text NOT NULL CHECK (liability IN ('merchant', 'scheme')),
    evidence               jsonb NOT NULL DEFAULT '{}',
    evidence_submitted_at  timestamptz,
    due_by                 timestamptz,
    network_due_by         timestamptz,
    authorized_at          timestamptz NOT NULL,
    notified_at            timestamptz NOT NULL,
    contested_at           timestamptz,
    blocked                bigint NOT NULL DEFAULT 0 CHECK (blocked >= 0 AND blocked <= amount),
    blocked_at             timestamptz,
    trace                  jsonb NOT NULL DEFAULT '[]',
    funds                  text NOT NULL DEFAULT 'none' CHECK (funds IN ('none', 'held', 'withdrawn', 'reinstated', 'released')),
    pending_action         text NOT NULL DEFAULT '',
    action_error           text NOT NULL DEFAULT '',
    action_retry_at        timestamptz,
    network_version        integer NOT NULL DEFAULT 0,
    outcome                text NOT NULL DEFAULT '',
    created_at             timestamptz NOT NULL,
    updated_at             timestamptz NOT NULL,
    closed_at              timestamptz,
    UNIQUE (livemode, kind, network_id)
);

CREATE INDEX disputes_by_merchant ON disputes.disputes (merchant_id, livemode, id);
CREATE INDEX disputes_by_intent ON disputes.disputes (payment_intent);
CREATE INDEX disputes_open ON disputes.disputes (updated_at) WHERE status IN ('needs_response', 'under_review') OR pending_action <> '';

-- What happened to each dispute, in order.
CREATE TABLE disputes.history (
    id         bigserial PRIMARY KEY,
    dispute_id text NOT NULL REFERENCES disputes.disputes (id),
    at         timestamptz NOT NULL,
    kind       text NOT NULL,
    detail     text NOT NULL
);

CREATE INDEX history_by_dispute ON disputes.history (dispute_id, id);

-- An issuer's report that a card payment was fraud, apart from any dispute (TC40, SAFE).
CREATE TABLE disputes.fraud_reports (
    id             text PRIMARY KEY,
    merchant_id    text NOT NULL,
    livemode       boolean NOT NULL,
    payment_intent text NOT NULL,
    network        text NOT NULL,
    network_id     text NOT NULL,
    fraud_type     text NOT NULL,
    amount         bigint NOT NULL,
    currency       text NOT NULL,
    reported_at    timestamptz NOT NULL,
    created_at     timestamptz NOT NULL,
    UNIQUE (livemode, network_id)
);

CREATE INDEX fraud_reports_by_merchant ON disputes.fraud_reports (merchant_id, livemode, id);

-- The test-mode network's cases: what it would do, played from evidence keywords.
CREATE TABLE disputes.test_cases (
    id                     text PRIMARY KEY,
    network_transaction_id text NOT NULL,
    network                text NOT NULL,
    amount                 bigint NOT NULL,
    currency               text NOT NULL,
    reason_code            text NOT NULL,
    stage                  text NOT NULL,
    status                 text NOT NULL,
    outcome                text NOT NULL DEFAULT '',
    respond_by             timestamptz,
    decide_by              timestamptz,
    escalated_evidence     text NOT NULL DEFAULT '',
    authorized_at          timestamptz NOT NULL,
    opened_at              timestamptz NOT NULL,
    version                integer NOT NULL
);

-- +goose Down
DROP TABLE disputes.test_cases;
DROP TABLE disputes.fraud_reports;
DROP TABLE disputes.history;
DROP TABLE disputes.disputes;
