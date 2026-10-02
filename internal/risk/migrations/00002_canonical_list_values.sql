-- +goose Up

-- List entries are matched as attempts are now written: an address without IPv4 mapping
-- and with IPv6 compressed, a fingerprint in lower case. An entry that is not a valid
-- value of its kind could never match, and goes; one that becomes another's twin, too.
-- +goose StatementBegin
DO $$
DECLARE
    item record;
    canonical text;
BEGIN
    FOR item IN SELECT id, kind, value FROM risk.list_items WHERE kind IN ('ip', 'card_fingerprint') LOOP
        BEGIN
            IF item.kind = 'card_fingerprint' THEN
                canonical := lower(item.value);
            ELSE
                canonical := host(item.value::inet);
                IF canonical LIKE '::ffff:%.%' THEN
                    canonical := substr(canonical, 8);
                END IF;
            END IF;
        EXCEPTION WHEN others THEN
            DELETE FROM risk.list_items WHERE id = item.id;
            CONTINUE;
        END;
        IF canonical <> item.value THEN
            BEGIN
                UPDATE risk.list_items SET value = canonical WHERE id = item.id;
            EXCEPTION WHEN unique_violation THEN
                DELETE FROM risk.list_items WHERE id = item.id;
            END;
        END IF;
    END LOOP;
END
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1;
