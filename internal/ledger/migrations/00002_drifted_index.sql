-- +goose Up

-- The health gauge counts drifted balances on every sample: of the few there ever are.
CREATE INDEX balances_drifted ON ledger.balances (account_id) WHERE drifted_at IS NOT NULL;

-- +goose Down
DROP INDEX ledger.balances_drifted;
