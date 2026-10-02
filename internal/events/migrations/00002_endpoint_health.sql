-- +goose Up

-- When an endpoint last took a delivery, to disable one that has taken none for days.
CREATE INDEX deliveries_succeeded_by_endpoint ON events.deliveries (endpoint_id, attempted_at) WHERE succeeded;

-- +goose Down
DROP INDEX events.deliveries_succeeded_by_endpoint;
