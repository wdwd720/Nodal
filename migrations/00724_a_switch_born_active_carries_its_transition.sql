-- +goose Up
-- A kill switch created already active carries its transition row, like every
-- other change to it (F-70).
--
-- Migration 00603's header says it binds "every state change of an audited
-- entity to a transition row written in the SAME transaction". For
-- kill_switches it binds one:
--
--   CREATE CONSTRAINT TRIGGER kill_switches_require_transition
--       AFTER UPDATE OF active ON kill_switches
--
-- AFTER UPDATE, not INSERT. A switch's FIRST activation does not update
-- anything: `killswitch.Controller.Activate` calls `insertActive`, which writes
-- `active = true` in the INSERT itself, and the trigger never sees it. `cp_app`
-- holds INSERT, so a first activation with no audit row is a legal write as far
-- as the database is concerned.
--
-- Go always writes the row -- Activate calls c.record() in the same
-- transaction -- so the trail is complete in practice. That is the shape of
-- F-49, F-53 and F-61: an invariant a careful service keeps, in a database that
-- was supposed to keep it whatever the caller is. It matters more here than in
-- most of those, because this table is the record of who stopped the platform
-- and why, and the first activation is the one an incident review reads.
--
-- The rule is only applied to a row born ACTIVE. A row born inactive is not a
-- state change from anything -- it is the absence of a switch, written down --
-- and requiring an audit row for it would be requiring an audit row for
-- nothing happening.

-- +goose StatementBegin
CREATE FUNCTION cp_require_transition_on_insert() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    state_col text := TG_ARGV[0];
    new_state text;
    flagged   text;
BEGIN
    EXECUTE format('SELECT ($1).%I::text', state_col) INTO new_state USING NEW;
    flagged := current_setting('cp.transition.' || TG_TABLE_NAME || '.x' || translate(NEW.id::text, '-', '_'), true);
    IF flagged IS NULL OR flagged <> new_state THEN
        RAISE EXCEPTION 'AUDIT_TRANSITION_REQUIRED: % % was created as % without a matching transition row in this transaction',
            TG_TABLE_NAME, NEW.id, new_state USING ERRCODE = 'AU001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- WHEN (NEW.active) keeps the rule to rows born active. A constraint trigger's
-- WHEN is evaluated at the statement, not at COMMIT, which is correct here: the
-- column cannot change between the two without firing the UPDATE trigger that
-- 00603 already installed.
CREATE CONSTRAINT TRIGGER kill_switches_insert_requires_transition
    AFTER INSERT ON kill_switches
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW WHEN (NEW.active)
    EXECUTE FUNCTION cp_require_transition_on_insert('active');

-- +goose Down
SELECT 1; -- protected: reverting would let the first activation of a kill switch go unrecorded
