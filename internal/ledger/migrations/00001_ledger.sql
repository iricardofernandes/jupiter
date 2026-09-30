-- +goose Up

CREATE SCHEMA IF NOT EXISTS ledger;

CREATE TABLE ledger.accounts (
    id           text PRIMARY KEY,
    book         text NOT NULL CHECK (book IN ('client_funds', 'own_funds')),
    code         text NOT NULL CHECK (code ~ '^[a-z][a-z0-9_]*$'),
    currency     text NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    normal       text NOT NULL CHECK (normal IN ('debit', 'credit')),
    non_negative boolean NOT NULL,
    batched      boolean NOT NULL,
    clearing     boolean NOT NULL,
    created_at   timestamptz NOT NULL,
    UNIQUE (id, book, currency),
    UNIQUE (id, normal, non_negative, batched),
    -- A batched account's cached balance lags its entries, so no constraint can be
    -- checked against it when an entry is written.
    CHECK (NOT (non_negative AND batched))
);

CREATE TABLE ledger.transactions (
    id          text PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN
                    ('posted', 'pending', 'post_pending', 'void_pending', 'expire_pending', 'reversal')),
    resolves_id text,
    -- Makes the foreign key below accept only a pending transaction as the one resolved.
    resolves_kind text GENERATED ALWAYS AS (CASE WHEN resolves_id IS NOT NULL THEN 'pending' END) STORED,
    reverses_id text REFERENCES ledger.transactions (id),
    expires_at  timestamptz,
    entry_count integer NOT NULL CHECK (entry_count >= 2),
    description text NOT NULL,
    created_at  timestamptz NOT NULL,
    UNIQUE (id, kind),
    FOREIGN KEY (resolves_id, resolves_kind) REFERENCES ledger.transactions (id, kind),
    CHECK ((kind IN ('post_pending', 'void_pending', 'expire_pending')) = (resolves_id IS NOT NULL)),
    CHECK ((kind = 'reversal') = (reverses_id IS NOT NULL)),
    CHECK (kind = 'pending' OR expires_at IS NULL)
);

-- A pending transfer is resolved once, and a transaction is reversed once.
CREATE UNIQUE INDEX transactions_resolves_once ON ledger.transactions (resolves_id) WHERE resolves_id IS NOT NULL;
CREATE UNIQUE INDEX transactions_reversed_once ON ledger.transactions (reverses_id) WHERE reverses_id IS NOT NULL;
CREATE INDEX transactions_pending_expiry ON ledger.transactions (expires_at) WHERE kind = 'pending';

-- Amounts are signed: a debit is positive, a credit negative.
CREATE TABLE ledger.entries (
    seq              bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transaction_id   text NOT NULL,
    transaction_kind text NOT NULL,
    account_id       text NOT NULL,
    book             text NOT NULL,
    currency         text NOT NULL,
    layer            text NOT NULL CHECK (layer IN ('pending', 'posted')),
    amount           bigint NOT NULL CHECK (amount <> 0 AND amount <> -9223372036854775808),
    FOREIGN KEY (transaction_id, transaction_kind) REFERENCES ledger.transactions (id, kind),
    FOREIGN KEY (account_id, book, currency) REFERENCES ledger.accounts (id, book, currency),
    CHECK (layer = 'posted' OR transaction_kind IN ('pending', 'post_pending', 'void_pending', 'expire_pending')),
    CHECK (layer = 'pending' OR transaction_kind IN ('posted', 'post_pending', 'reversal'))
);

CREATE INDEX entries_by_account ON ledger.entries (account_id, seq);
CREATE INDEX entries_by_transaction ON ledger.entries (transaction_id);

-- The cache of each account's balance. It is the only mutable ledger table, and the
-- invariant checker proves it against the entries.
CREATE TABLE ledger.balances (
    account_id      text PRIMARY KEY,
    normal          text NOT NULL,
    non_negative    boolean NOT NULL,
    batched         boolean NOT NULL,
    posted_debits   bigint NOT NULL DEFAULT 0 CHECK (posted_debits >= 0),
    posted_credits  bigint NOT NULL DEFAULT 0 CHECK (posted_credits >= 0),
    pending_debits  bigint NOT NULL DEFAULT 0 CHECK (pending_debits >= 0),
    pending_credits bigint NOT NULL DEFAULT 0 CHECK (pending_credits >= 0),
    drifted_at      timestamptz,
    FOREIGN KEY (account_id, normal, non_negative, batched)
        REFERENCES ledger.accounts (id, normal, non_negative, batched),
    -- Pending amounts count against the balance but never for it, as in TigerBeetle.
    CONSTRAINT balances_non_negative CHECK (NOT non_negative OR CASE normal
        WHEN 'debit' THEN pending_credits + posted_credits <= posted_debits
        ELSE pending_debits + posted_debits <= posted_credits
    END)
);

-- Deltas waiting to be applied to batched accounts' cached balances. Rows are inserted
-- in the entry's transaction and deleted when applied, so none is skipped or applied
-- twice whatever order transactions commit in.
CREATE TABLE ledger.balance_queue (
    entry_seq       bigint PRIMARY KEY REFERENCES ledger.entries (seq),
    account_id      text NOT NULL REFERENCES ledger.accounts (id),
    posted_debits   bigint NOT NULL,
    posted_credits  bigint NOT NULL,
    pending_debits  bigint NOT NULL,
    pending_credits bigint NOT NULL
);

CREATE INDEX balance_queue_by_account ON ledger.balance_queue (account_id);

-- The one definition of how an entry moves the four balance fields, shared by the
-- write path and the invariant checker. A pending entry of a 'pending' transaction
-- places a hold; a pending entry of any other kind releases one.
-- +goose StatementBegin
CREATE FUNCTION ledger.entry_deltas(
    kind text, layer text, amount bigint,
    OUT posted_debits bigint, OUT posted_credits bigint,
    OUT pending_debits bigint, OUT pending_credits bigint
) LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT
        CASE WHEN layer = 'posted' AND amount > 0 THEN amount ELSE 0 END,
        CASE WHEN layer = 'posted' AND amount < 0 THEN -amount ELSE 0 END,
        CASE WHEN layer = 'pending' AND kind = 'pending' AND amount > 0 THEN amount
             WHEN layer = 'pending' AND kind <> 'pending' AND amount < 0 THEN amount
             ELSE 0 END,
        CASE WHEN layer = 'pending' AND kind = 'pending' AND amount < 0 THEN -amount
             WHEN layer = 'pending' AND kind <> 'pending' AND amount > 0 THEN -amount
             ELSE 0 END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger.apply_entry() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    d       record;
    batched boolean;
    drifted timestamptz;
BEGIN
    SELECT b.batched INTO batched FROM ledger.balances b WHERE b.account_id = NEW.account_id;
    IF batched THEN
        -- No lock: a hot account's row must not serialize its writers.
        SELECT b.drifted_at INTO drifted FROM ledger.balances b WHERE b.account_id = NEW.account_id;
    ELSE
        SELECT b.drifted_at INTO drifted FROM ledger.balances b WHERE b.account_id = NEW.account_id FOR UPDATE;
    END IF;
    IF drifted IS NOT NULL THEN
        RAISE EXCEPTION 'account % has a drifted balance and refuses entries', NEW.account_id
            USING ERRCODE = 'JL001';
    END IF;

    SELECT * INTO d FROM ledger.entry_deltas(NEW.transaction_kind, NEW.layer, NEW.amount);
    IF batched THEN
        INSERT INTO ledger.balance_queue
            (entry_seq, account_id, posted_debits, posted_credits, pending_debits, pending_credits)
        VALUES (NEW.seq, NEW.account_id, d.posted_debits, d.posted_credits, d.pending_debits, d.pending_credits);
    ELSE
        UPDATE ledger.balances
           SET posted_debits   = posted_debits + d.posted_debits,
               posted_credits  = posted_credits + d.posted_credits,
               pending_debits  = pending_debits + d.pending_debits,
               pending_credits = pending_credits + d.pending_credits
         WHERE account_id = NEW.account_id;
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER entries_apply AFTER INSERT ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.apply_entry();

-- Checked at commit for every entry, so entries added to an old transaction in a later
-- database transaction are caught too: they change its count.
-- +goose StatementBegin
CREATE FUNCTION ledger.check_transaction() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    declared integer;
    actual   integer;
    bad      record;
BEGIN
    SELECT entry_count INTO declared FROM ledger.transactions WHERE id = NEW.transaction_id;
    SELECT count(*) INTO actual FROM ledger.entries WHERE transaction_id = NEW.transaction_id;
    IF actual <> declared THEN
        RAISE EXCEPTION 'transaction % has % entries but declares %', NEW.transaction_id, actual, declared
            USING ERRCODE = 'JL002';
    END IF;

    SELECT book, currency, layer, sum(amount) AS total INTO bad
      FROM ledger.entries
     WHERE transaction_id = NEW.transaction_id
     GROUP BY book, currency, layer
    HAVING sum(amount) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'transaction % does not balance: % % % entries sum to %',
            NEW.transaction_id, bad.book, bad.currency, bad.layer, bad.total
            USING ERRCODE = 'JL002';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER entries_balanced AFTER INSERT ON ledger.entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger.check_transaction();

-- +goose StatementBegin
CREATE FUNCTION ledger.create_balance() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO ledger.balances (account_id, normal, non_negative, batched)
    VALUES (NEW.id, NEW.normal, NEW.non_negative, NEW.batched);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER accounts_create_balance AFTER INSERT ON ledger.accounts
    FOR EACH ROW EXECUTE FUNCTION ledger.create_balance();

-- +goose StatementBegin
CREATE FUNCTION ledger.forbid_change() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger.% is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'JL003';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER accounts_append_only BEFORE UPDATE OR DELETE ON ledger.accounts
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER accounts_no_truncate BEFORE TRUNCATE ON ledger.accounts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER transactions_append_only BEFORE UPDATE OR DELETE ON ledger.transactions
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER transactions_no_truncate BEFORE TRUNCATE ON ledger.transactions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER entries_append_only BEFORE UPDATE OR DELETE ON ledger.entries
    FOR EACH ROW EXECUTE FUNCTION ledger.forbid_change();
CREATE TRIGGER entries_no_truncate BEFORE TRUNCATE ON ledger.entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger.forbid_change();

-- +goose Down
DROP TABLE ledger.balance_queue;
DROP TABLE ledger.balances;
DROP TABLE ledger.entries;
DROP TABLE ledger.transactions;
DROP TABLE ledger.accounts;
DROP FUNCTION ledger.forbid_change();
DROP FUNCTION ledger.create_balance();
DROP FUNCTION ledger.check_transaction();
DROP FUNCTION ledger.apply_entry();
DROP FUNCTION ledger.entry_deltas(text, text, bigint);
