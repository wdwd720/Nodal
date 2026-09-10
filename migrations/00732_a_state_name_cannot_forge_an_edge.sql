-- +goose Up
-- A state name cannot forge an edge.
--
-- 00731 bound every audited change to a transition row describing BOTH of its
-- endpoints. It does that by flagging the edge as `<from>` `>` `<to>` and
-- joining the edges written in one transaction with `|`, then checking exact
-- membership over the split. That closed the forged origin, and it introduced a
-- defect of its own: both delimiters are in band.
--
-- `from_*` and `to_*` are unconstrained `text` on fourteen of the fifteen
-- tables 00731 converted -- only wallet_status_transitions.to_status carries a
-- CHECK, and no table constrains its origin. So a single row whose destination
-- CONTAINS the delimiters splits the flag into more elements than the row
-- describes, and one of those elements can be an edge nobody wrote down:
--
--     INSERT INTO account_status_transitions (..., from_status, to_status, ...)
--     VALUES (..., 'RESTRICTED', 'FROZEN|ACTIVE>FROZEN', ...);
--     UPDATE accounts SET status = 'FROZEN' WHERE id = $1;      -- licensed
--
--     flagged = 'RESTRICTED>FROZEN|ACTIVE>FROZEN'
--     split   = {'RESTRICTED>FROZEN', 'ACTIVE>FROZEN'}
--
-- The account moves ACTIVE -> FROZEN under a row that says it moved from
-- RESTRICTED to a string that is not a status. Observed committing as cp_app
-- against migrations through 00731. The same forgery WITHOUT the delimiters is
-- refused with AU001 in the same session, which is what makes this the
-- encoding's defect rather than the binding's: the check is sound and the
-- string it reads is attacker-shaped. (F-101.)
--
-- It needs one row, not two. 00731's header reasoned about how many rows it
-- takes to license an extra edge and answered "more than one, and each must
-- describe its own step". That is true of honest rows and was never true of
-- this one.
--
-- The fix refuses the delimiters where the flag is BUILT, so no forged flag
-- exists to be read. Refused rather than escaped, deliberately: escaping keeps
-- the encoding ambiguous and hands the question to whoever writes the next
-- reader, while refusing makes `|` and `>` impossible inside a state, which is
-- a smaller thing to know and a thing the schema can state. Every state name in
-- this schema is SCREAMING_SNAKE and neither character has ever appeared in
-- one; this turns an assumption the encoding was already depending on into a
-- constraint it enforces.
--
-- The guard lives in the shared functions rather than in fifteen CHECK
-- constraints because a list duplicated fifteen times is a list that diverges
-- -- and because the sixteenth table to be bound then gets the guard by
-- construction rather than by whoever remembers.
--
-- The check on the ENTITY side is belt and braces: state columns carry
-- membership CHECKs, so a delimiter cannot reach one today. It is there so that
-- an entity whose states are not enumerated cannot silently weaken the binding
-- by being bound.
--
-- Custom SQLSTATE: AU001, as in 00603, 00690, 00726 and 00731.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_flag_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
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
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text, ($1).%I::text', entity_col, from_col, to_col)
        INTO entity_id, from_val, to_val USING NEW;
    -- Refused here, where the flag is built, rather than parsed for later.
    IF coalesce(from_val, '') ~ '[|>]' OR coalesce(to_val, '') ~ '[|>]' THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % cannot record the state % -> %: a state may not contain ''|'' or ''>'', which encode the edge itself',
            TG_TABLE_NAME, from_val, to_val USING ERRCODE = 'AU001';
    END IF;
    edge := from_val || '>' || to_val;
    -- Setting-name components must be identifiers, so UUIDs are keyed as
    -- x<uuid with underscores>, exactly as in 00603.
    setting := 'cp.edge.' || label || '.x' || translate(entity_id, '-', '_');
    current := current_setting(setting, true);
    IF current IS NULL OR current = '' THEN
        PERFORM set_config(setting, edge, true);
    ELSE
        PERFORM set_config(setting, current || '|' || edge, true);
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_require_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    col       text := TG_ARGV[0];
    label     text := TG_ARGV[1];
    id_col    text := coalesce(TG_ARGV[2], 'id');
    old_val   text;
    new_val   text;
    entity_id text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', col, col) INTO old_val, new_val USING OLD, NEW;
    IF old_val IS NOT DISTINCT FROM new_val THEN
        RETURN NULL;
    END IF;
    EXECUTE format('SELECT ($1).%I::text', id_col) INTO entity_id USING NEW;
    -- An endpoint carrying a delimiter cannot be described unambiguously by the
    -- flag, so it is refused rather than compared.
    IF coalesce(old_val, '') ~ '[|>]' OR coalesce(new_val, '') ~ '[|>]' THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % cannot change % % -> %: a state may not contain ''|'' or ''>'', which encode the edge itself',
            TG_TABLE_NAME, entity_id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    flagged := current_setting('cp.edge.' || label || '.x' || translate(entity_id, '-', '_'), true);
    IF flagged IS NULL OR NOT (old_val || '>' || new_val = ANY (string_to_array(flagged, '|'))) THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % % -> % without a transition row describing that change',
            TG_TABLE_NAME, entity_id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_flag_transition_edge() IS
    'Flags the edge a transition row describes, refusing a state containing the delimiters the edge encoding uses. Without the refusal one row licenses a second, unrelated edge (F-101).';

-- +goose Down
SELECT 1; -- protected: reverting lets one transition row license an edge it does not describe
