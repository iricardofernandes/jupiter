-- +goose Up

CREATE SCHEMA IF NOT EXISTS events;

CREATE TABLE events.events (
    id          text PRIMARY KEY,
    merchant_id text NOT NULL,
    livemode    boolean NOT NULL,
    type        text NOT NULL CHECK (type ~ '^[a-z_]+\.[a-z_]+$'),
    object_id   text NOT NULL,
    object_type text NOT NULL,
    created_at  timestamptz NOT NULL
);

CREATE INDEX events_by_merchant ON events.events (merchant_id, livemode, id);

CREATE TABLE events.endpoints (
    id             text PRIMARY KEY,
    merchant_id    text NOT NULL,
    livemode       boolean NOT NULL,
    url            text NOT NULL CHECK (length(url) <= 2048),
    description    text NOT NULL,
    enabled_events text[] NOT NULL CHECK (cardinality(enabled_events) > 0),
    status         text NOT NULL CHECK (status IN ('enabled', 'disabled')),
    api_version    text NOT NULL,
    created_at     timestamptz NOT NULL,
    deleted_at     timestamptz
);

CREATE INDEX endpoints_by_merchant ON events.endpoints (merchant_id, livemode, id) WHERE deleted_at IS NULL;

-- Sealed with AES-GCM under the process's secret key, bound to endpoint and secret id.
-- More than one secret is active while a rotation's overlap window lasts.
CREATE TABLE events.endpoint_secrets (
    id          text PRIMARY KEY,
    endpoint_id text NOT NULL REFERENCES events.endpoints (id),
    sealed      bytea NOT NULL,
    created_at  timestamptz NOT NULL,
    expires_at  timestamptz
);

CREATE INDEX endpoint_secrets_by_endpoint ON events.endpoint_secrets (endpoint_id);

CREATE TABLE events.deliveries (
    id              text PRIMARY KEY,
    event_id        text NOT NULL REFERENCES events.events (id),
    endpoint_id     text NOT NULL REFERENCES events.endpoints (id),
    attempt         integer NOT NULL,
    succeeded       boolean NOT NULL,
    response_status integer,
    error           text NOT NULL,
    duration_ms     integer NOT NULL,
    attempted_at    timestamptz NOT NULL
);

CREATE INDEX deliveries_by_event ON events.deliveries (event_id, endpoint_id);

-- +goose Down
DROP TABLE events.deliveries;
DROP TABLE events.endpoint_secrets;
DROP TABLE events.endpoints;
DROP TABLE events.events;
