-- +goose Up
-- A transition row licenses only the change it describes, and the agent's
-- STAGE is bound to one as well as its state (F-78).
--
-- 00690 bound agents.state to a row in agent_lifecycle_transitions written in
-- the same transaction. Its header says why, and it is right: "without this, a
-- bare UPDATE agents SET state = 'LIVE' by the application role would move an
-- agent onto real customer capital leaving no evidence of the promotion, its
-- approval or its gate evidence".
--
-- It compares one thing. cp_flag_transition records the row's to_state, and
-- cp_require_transition raises AU001 unless that equals the NEW state. Nothing
-- looks at from_state, and nothing looks at `stage` at all.
--
-- Both promotion CHECKs on agent_lifecycle_transitions open with
-- `from_stage = to_stage OR ...`, and they have to: a pause or a resume keeps
-- the stage and must not be made to carry promotion evidence.
--
-- Those two facts compose into a promotion with no approval and no evidence,
-- available to the application role in one transaction:
--
--     INSERT INTO agent_lifecycle_transitions
--         (from_state, to_state, from_stage, to_stage, ...)
--     VALUES ('CANARY', 'LIVE', 'LIVE', 'LIVE', ...);   -- the lie is from_stage
--     UPDATE agents SET state = 'LIVE', stage = 'LIVE', mode = 'LIVE' WHERE id = ...;
--
-- from_stage = to_stage satisfies the first clause of both CHECKs, so no
-- approval_id, no ir_hash, no risk policy version or hash and no evidence_hash
-- are required; to_state = 'LIVE' is exactly what the state binding demands.
-- Observed committing against the schema before this migration, from the
-- application role, on an agent at CANARY.
--
-- The second route needs no lie at all. agents_check permits any stage while
-- the state is a side state ("state = stage OR state IN ('PAUSED','FAILED',
-- 'REVOKED','SUPERSEDED')"), so a PAUSED agent's stage can be moved with no
-- transition row whatsoever -- and Resume, which needs only agent:pause, then
-- sets the state to whatever stage it finds.
--
-- The fix binds the EDGE rather than the destination. The flag records
-- '<from>><to>', and the constraint trigger compares it against
-- OLD.col || '>' || NEW.col, so a row can license a change only if it says
-- where that change started. A row claiming the stage did not move no longer
-- licenses moving it.
--
-- Scope: agents only. The other ten bindings 00603 established compare the
-- destination alone and have the same shape; whether that is reachable there
-- depends on each table's own CHECKs, and widening this is a migration per
-- table with its own exploit test. It is recorded in AUDIT_FINDINGS.md as
-- F-78 rather than done here on the strength of the analogy.
--
-- `mode` is not bound: agent_lifecycle_transitions has no to_mode column to
-- bind it to, and agents_check3 pins mode to stage for every rung except
-- BACKTEST_ELIGIBLE, whose two modes (BACKTEST, PAPER) both move simulated
-- money only.
--
-- Custom SQLSTATE: AU001, as in 00603 and 00690.

-- +goose StatementBegin
CREATE FUNCTION cp_flag_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    entity_col text := TG_ARGV[0];   -- column on the transitions table holding the entity id
    from_col   text := TG_ARGV[1];
    to_col     text := TG_ARGV[2];
    label      text := TG_ARGV[3];   -- keeps one entity's several bound columns apart
    entity_id  text;
    edge       text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($1).%I::text || ''>'' || ($1).%I::text', entity_col, from_col, to_col)
        INTO entity_id, edge USING NEW;
    -- Setting-name components must be identifiers, so UUIDs are keyed as
    -- x<uuid with underscores>, exactly as in 00603.
    PERFORM set_config('cp.edge.' || label || '.x' || translate(entity_id, '-', '_'), edge, true);
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_require_transition_edge() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    col     text := TG_ARGV[0];
    label   text := TG_ARGV[1];
    old_val text;
    new_val text;
    flagged text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text, ($2).%I::text', col, col) INTO old_val, new_val USING OLD, NEW;
    IF old_val IS NOT DISTINCT FROM new_val THEN
        RETURN NULL;
    END IF;
    flagged := current_setting('cp.edge.' || label || '.x' || translate(NEW.id::text, '-', '_'), true);
    IF flagged IS NULL OR flagged <> old_val || '>' || new_val THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % changed % % -> % without a transition row describing that change',
            TG_TABLE_NAME, NEW.id, col, old_val, new_val USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_lifecycle_transitions_flag_state_edge AFTER INSERT ON agent_lifecycle_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('agent_id', 'from_state', 'to_state', 'agents_state');
CREATE TRIGGER agent_lifecycle_transitions_flag_stage_edge AFTER INSERT ON agent_lifecycle_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('agent_id', 'from_stage', 'to_stage', 'agents_stage');

-- The destination-only binding is replaced rather than kept beside the edge
-- binding: two triggers raising the same SQLSTATE for the same write would make
-- every failure ambiguous about which rule fired, which is F-53's lesson.
DROP TRIGGER agents_require_transition ON agents;
CREATE CONSTRAINT TRIGGER agents_require_transition AFTER UPDATE OF state ON agents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('state', 'agents_state');
CREATE CONSTRAINT TRIGGER agents_stage_requires_transition AFTER UPDATE OF stage ON agents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('stage', 'agents_stage');

COMMENT ON FUNCTION cp_require_transition_edge() IS
    'Binds a column change to a transition row that names both endpoints. cp_require_transition (00603) compares the destination only, which lets a row claiming the stage did not move license moving it (F-78).';

-- +goose Down
SELECT 1; -- protected: this binding is what keeps an agent''s promotion evidence unavoidable
