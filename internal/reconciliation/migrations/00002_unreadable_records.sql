-- +goose Up

-- A record that cannot be matched, with no identity, key or positive amount, is a break of
-- its own: kept for a person, while the reconciliation goes on.
ALTER TABLE reconciliation.breaks DROP CONSTRAINT breaks_kind_check;
ALTER TABLE reconciliation.breaks ADD CONSTRAINT breaks_kind_check
    CHECK (kind IN ('missing_at_counterparty', 'missing_at_jupiter', 'duplicate', 'amount_mismatch', 'probable_match', 'divergence', 'unreadable'));

-- +goose Down
DELETE FROM reconciliation.breaks WHERE kind = 'unreadable';
ALTER TABLE reconciliation.breaks DROP CONSTRAINT breaks_kind_check;
ALTER TABLE reconciliation.breaks ADD CONSTRAINT breaks_kind_check
    CHECK (kind IN ('missing_at_counterparty', 'missing_at_jupiter', 'duplicate', 'amount_mismatch', 'probable_match', 'divergence'));
