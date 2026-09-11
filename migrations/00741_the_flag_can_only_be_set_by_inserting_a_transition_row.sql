-- +goose Up
-- The flag can only be set by inserting a transition row.
--
-- 00603's header made that promise. F-42 recorded that the second half of it
-- was false and left it OPEN for four sessions, because every candidate fix was
-- tried and rejected with its evidence. This closes it, and the reason it can be
-- closed now is that the problem was never the one the earlier attempts were
-- solving.
--
-- ## The two forgeries, both reproduced as cp_app before this was written
--
-- **One: set the setting by hand.** `cp.transition.*` and `cp.edge.*` are custom
-- GUCs and PostgreSQL lets any role set one. Reproduced against this schema:
--
--     BEGIN;
--     SELECT set_config('cp.edge.accounts.x<id>', 'ACTIVE>FROZEN', true);
--     SELECT set_config('cp.transition.accounts.x<id>', 'FROZEN', true);
--     UPDATE accounts SET status = 'FROZEN' WHERE id = '<id>';
--     COMMIT;                                     -- succeeded
--
-- The account froze. `account_status_transitions` gained no row.
--
-- **Two, which F-42 does not record and which is worse.** `cp_app` holds
-- TEMPORARY on the database and the flag functions were EXECUTE-able by PUBLIC,
-- so it could attach the real setter to a table of its own:
--
--     CREATE TEMP TABLE forge (account_id uuid, from_status text, to_status text);
--     CREATE TRIGGER forge_flag AFTER INSERT ON forge FOR EACH ROW
--       EXECUTE FUNCTION cp_flag_transition_edge('account_id','from_status','to_status','accounts');
--     INSERT INTO forge VALUES ('<id>', 'ACTIVE', 'FROZEN');
--     UPDATE accounts SET status = 'FROZEN' WHERE id = '<id>';   -- succeeded
--
-- That one matters beyond itself: it defeats any scheme that makes the flag's
-- VALUE unforgeable, because it forges the value using the real setter. A fix
-- that only hardened the value would have looked complete and been bypassed by
-- three lines.
--
-- ## What this does
--
-- The flag stops being a bare string and becomes a keyed tag over three things
-- the caller cannot control together:
--
--     tag = sha256(secret || '|' || pg_current_xact_id() || '|' || setting || '|' || value)
--
-- and the stored setting becomes `<64 hex chars><value>`, accumulating with '|'
-- exactly as before.
--
--   - **secret** lives in `cp_transition_key`, which has no grant to any role
--     but the owner. The setters read it only because they are now SECURITY
--     DEFINER. `cp_app` cannot read it and therefore cannot compute a tag.
--   - **pg_current_xact_id()** binds the tag to one transaction. Without it a
--     caller could legitimately transition an entity, keep the string, and
--     replay it in a later transaction to change state again with no audit row.
--     That is a real forgery and it is why a keyed tag alone is not enough.
--   - **setting** carries the label and the entity id, so a tag minted for one
--     entity cannot be moved to another.
--   - **value** is the destination, or the `<from>><to>` edge, so a tag cannot
--     be moved to a different change of the same entity.
--
-- And EXECUTE on all five functions is revoked from PUBLIC, which closes the
-- temp-table route. Existing triggers are unaffected: PostgreSQL checks EXECUTE
-- on a trigger function at CREATE TRIGGER time, not when it fires. That was
-- verified here, not assumed -- the revoke was applied and a legitimate
-- transition was then driven through as cp_app.
--
-- ## Why pg_current_xact_id() works where three earlier attempts did not
--
-- F-42 records, with the probe output, that `xmin = pg_current_xact_id()` is
-- false for a row written inside a SAVEPOINT, and the admin executor runs every
-- action inside one. That rejection was correct and it is about a ROW's xmin.
-- `pg_current_xact_id()` is not a row's xmin: it returns the TOP-LEVEL
-- transaction id, and both triggers here run in the same top-level transaction
-- -- the setter on INSERT, the verifier as a DEFERRED constraint at COMMIT.
-- Probed on this project's PostgreSQL 16 before this migration was written:
--
--     top level                        3430236
--     inside a savepoint               3430236
--     after a write in a savepoint     3430236
--     after ROLLBACK TO SAVEPOINT      3430236
--
-- It also avoids the reason `pg_xact_status` was rejected. That rejection was
-- about wraparound: `xmin` is 32-bit and casting it to `xid8` invents an epoch,
-- so a control built on it works for years and then refuses everything at once.
-- Nothing here is stored or compared across time. The xid is a salt inside a
-- single transaction, `pg_current_xact_id()` already returns `xid8`, and it
-- never leaves the transaction that produced it.
--
-- And it avoids the fake-clock rock that sank `created_at >=
-- transaction_timestamp()` and "the latest transition": no ordering and no
-- timestamp is involved. The test suites' clocks cannot make a transaction id
-- non-monotonic.
--
-- ## What this deliberately does not do
--
-- F-42's recorded remedy is the `capability_gates` treatment -- revoke UPDATE on
-- the state column of sixteen tables and route state changes through SECURITY
-- DEFINER functions. That is still worth doing and is still the stronger answer,
-- because privilege beats detection. It is a change to every state machine's
-- call sites and it is not what this migration is.
--
-- This makes the flag unforgeable, which is precisely the claim 00603's header
-- made and could not keep. Four of the sixteen tables already have the
-- privilege treatment on their money columns (00733, F-109); the rest is
-- recorded as remaining.
--
-- Custom SQLSTATE: AU001, unchanged.

CREATE TABLE cp_transition_key (
    only_row boolean PRIMARY KEY DEFAULT true CHECK (only_row),
    secret   text NOT NULL CHECK (length(secret) >= 32),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- No role but the owner may read this table. That is the entire basis of the
-- control: a role that cannot read the secret cannot compute a tag, and a tag
-- is the only thing the verifier believes.
--
-- The REVOKE is not belt and braces. `ALTER DEFAULT PRIVILEGES` in this schema
-- grants SELECT on every table cp_migrate creates to cp_readonly and cp_ops, so
-- this table arrived readable by both without a GRANT being written anywhere.
-- Found by the test that asserts it, not by reading the migration -- writing no
-- GRANT is not the same as granting nothing.
REVOKE ALL ON cp_transition_key FROM PUBLIC;
REVOKE ALL ON cp_transition_key FROM cp_app, cp_readonly, cp_ops;

INSERT INTO cp_transition_key (only_row, secret)
VALUES (true, encode(gen_random_bytes(32), 'hex'));

COMMENT ON TABLE cp_transition_key IS
    'The key the transition flag is tagged with. Readable only by the owner; the flag setters read it as SECURITY DEFINER. Nothing else may be granted on this table -- see 00741 (F-42).';

-- +goose StatementBegin
CREATE FUNCTION cp_transition_tag(setting text, value text) RETURNS text
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT encode(sha256((
        (SELECT secret FROM cp_transition_key WHERE only_row) || '|' ||
        pg_current_xact_id()::text || '|' || setting || '|' || value
    )::bytea), 'hex');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_flag_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    entity_col text := TG_ARGV[0];
    state_col  text := TG_ARGV[1];
    entity_id  text;
    new_state  text;
    setting    text;
    current    text;
    tagged     text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text', entity_col, state_col) INTO entity_id, new_state USING NEW;
    setting := 'cp.transition.' || TG_ARGV[2] || '.x' || translate(entity_id, '-', '_');
    tagged  := cp_transition_tag(setting, new_state) || new_state;
    current := current_setting(setting, true);
    IF current IS NULL OR current = '' THEN
        PERFORM set_config(setting, tagged, true);
    ELSE
        PERFORM set_config(setting, current || '|' || tagged, true);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_flag_transition_edge() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    entity_col text := TG_ARGV[0];
    from_col   text := TG_ARGV[1];
    to_col     text := TG_ARGV[2];
    label      text := TG_ARGV[3];
    entity_id  text;
    from_val   text;
    to_val     text;
    edge       text;
    setting    text;
    current    text;
    tagged     text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text, ($1).%I::text', entity_col, from_col, to_col)
        INTO entity_id, from_val, to_val USING NEW;
    -- Refused here, where the flag is built, rather than parsed for later
    -- (00732, F-101).
    IF coalesce(from_val, '') ~ '[|>]' OR coalesce(to_val, '') ~ '[|>]' THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % cannot record the state % -> %: a state may not contain ''|'' or ''>'', which encode the edge itself',
            TG_TABLE_NAME, from_val, to_val USING ERRCODE = 'AU001';
    END IF;
    edge    := from_val || '>' || to_val;
    setting := 'cp.edge.' || label || '.x' || translate(entity_id, '-', '_');
    tagged  := cp_transition_tag(setting, edge) || edge;
    current := current_setting(setting, true);
    IF current IS NULL OR current = '' THEN
        PERFORM set_config(setting, tagged, true);
    ELSE
        PERFORM set_config(setting, current || '|' || tagged, true);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_require_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    state_col text := TG_ARGV[0];
    id_col    text := coalesce(TG_ARGV[1], 'id');
    old_state text;
    new_state text;
    entity_id text;
    setting   text;
    flagged   text;
    entry     text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', state_col, state_col) INTO old_state, new_state USING OLD, NEW;
    IF old_state IS NOT DISTINCT FROM new_state THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT ($1).%I::text', id_col) INTO entity_id USING NEW;
    setting := 'cp.transition.' || TG_TABLE_NAME || '.x' || translate(entity_id, '-', '_');
    flagged := current_setting(setting, true);
    IF flagged IS NOT NULL THEN
        FOREACH entry IN ARRAY string_to_array(flagged, '|') LOOP
            IF length(entry) > 64
               AND substr(entry, 65) = new_state
               AND substr(entry, 1, 64) = cp_transition_tag(setting, new_state) THEN
                RETURN NULL;
            END IF;
        END LOOP;
    END IF;
    RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % -> % without a matching transition row in this transaction',
        TG_TABLE_NAME, entity_id, old_state, new_state USING ERRCODE = 'AU001';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_require_transition_edge() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    col       text := TG_ARGV[0];
    label     text := TG_ARGV[1];
    id_col    text := coalesce(TG_ARGV[2], 'id');
    old_val   text;
    new_val   text;
    entity_id text;
    setting   text;
    flagged   text;
    entry     text;
    edge      text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', col, col) INTO old_val, new_val USING OLD, NEW;
    IF old_val IS NOT DISTINCT FROM new_val THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT ($1).%I::text', id_col) INTO entity_id USING NEW;
    IF coalesce(old_val, '') ~ '[|>]' OR coalesce(new_val, '') ~ '[|>]' THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % cannot change % % -> %: a state may not contain ''|'' or ''>'', which encode the edge itself',
            TG_TABLE_NAME, entity_id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    edge    := old_val || '>' || new_val;
    setting := 'cp.edge.' || label || '.x' || translate(entity_id, '-', '_');
    flagged := current_setting(setting, true);
    IF flagged IS NOT NULL THEN
        FOREACH entry IN ARRAY string_to_array(flagged, '|') LOOP
            IF length(entry) > 64
               AND substr(entry, 65) = edge
               AND substr(entry, 1, 64) = cp_transition_tag(setting, edge) THEN
                RETURN NULL;
            END IF;
        END LOOP;
    END IF;
    RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % % -> % without a transition row describing that change',
        TG_TABLE_NAME, entity_id, col, old_val, new_val USING ERRCODE = 'AU001';
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_require_transition_on_insert() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    state_col text := TG_ARGV[0];
    new_state text;
    setting   text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text', state_col) INTO new_state USING NEW;
    setting := 'cp.transition.' || TG_TABLE_NAME || '.x' || translate(NEW.id::text, '-', '_');
    flagged := current_setting(setting, true);
    -- Exact equality, as in 00724: a row created in a state must have exactly
    -- one transition row behind it, not one among several.
    IF flagged IS NULL
       OR length(flagged) <= 64
       OR substr(flagged, 65) <> new_state
       OR substr(flagged, 1, 64) <> cp_transition_tag(setting, new_state) THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % was created as % without a matching transition row in this transaction',
            TG_TABLE_NAME, NEW.id, new_state USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- The temp-table route. PostgreSQL checks EXECUTE on a trigger function when
-- the trigger is CREATED, not when it fires, so every trigger already attached
-- keeps working -- verified against this schema before this was written.
REVOKE EXECUTE ON FUNCTION cp_transition_tag(text, text) FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_flag_transition() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_flag_transition_edge() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_require_transition() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_require_transition_edge() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_require_transition_on_insert() FROM PUBLIC;

COMMENT ON FUNCTION cp_transition_tag(text, text) IS
    'The keyed tag the transition flag carries: sha256 over the secret, the top-level transaction id, the setting name and the value. SECURITY DEFINER because the secret is readable only by the owner; EXECUTE revoked from PUBLIC so the application cannot mint one (00741, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the flag to a bare string any role can set, which is the finding
