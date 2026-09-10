-- +goose Up
-- A kill switch is released by its transition, not beside it.
--
-- F-42's stronger remedy, table seventeen of seventeen, and the last. 00743
-- named this one as deliberately-not-next and said why: its single write sets
-- `active` together with six other columns **under optimistic concurrency**, so
-- moving one column into a trigger breaks the version check and the returned row
-- at once.
--
-- That is still true. It is not a reason to leave the most consequential switch
-- in the system writable by the application; it is a reason to move the version
-- check as well.
--
-- ## What a forged write here would be
--
-- `kill_switches.active` is what stops trading, withdrawals or the whole
-- platform in an incident. A write with no transition row is a switch RELEASED
-- with nothing on the record saying who released it or under what approval --
-- which is the one direction that matters, because releasing is the dangerous
-- half.
--
-- ## The version check moves, and becomes stronger
--
-- `saveSwitch` carried `version = version + 1 WHERE id = $1 AND version = $11`,
-- with `pgx.ErrNoRows` mapped to CodeConflict. The transition row now carries
-- `from_version`, the trigger performs the same compare-and-swap and the same
-- increment, and raises when the switch has moved underneath.
--
-- Moving it means it applies to every writer rather than to one function, and
-- it stops being possible to write the switch without bumping the version --
-- which was previously a convention of a single statement.
--
-- ## `to_severity`, and what it is not
--
-- The transition row had no severity. It is added rather than derived: severity
-- comes from `kind.Severity()` on activation and is left alone on release, so
-- deriving it from `to_active` would be wrong on exactly the transition that
-- matters. It is `to_severity` and not `severity` for the reason 00750 gives
-- about `to_failure_reason`: this is the switch's severity AFTER the transition,
-- not a property of the transition itself.
--
-- ## The activated/released columns follow to_active
--
-- Activation sets `activated_by_actor_id` and `activated_at` and clears the
-- release fields; release sets the release fields and leaves the activation
-- ones. That was a Go-side convention in two functions; it is now one branch,
-- and the row it reads from is the same row the audit trail keeps.
--
-- ## No birth exemption is needed, and here is why
--
-- Unlike `reconciliation_records`, a kill switch is INSERTed already active by
-- `insertActive` with `version = 1`, and `record` runs after it. So the
-- transition fires against a switch that exists and is already in its
-- destination state: the compare-and-swap below sees `from_version = 1` against
-- a row at version 1 and applies a no-op update that bumps it to 2. That is
-- correct and is what the old code did too -- `insertActive` then `record` never
-- called `saveSwitch`, so the version stayed at 1; it now becomes 2. Nothing
-- reads an absolute version, only equality against the one it last saw.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   kill_switch_transitions_flag        (destination-only, 00603 -- this table
--                                        has no from_active, which is the
--                                        schema's one deliberate exception)
--   kill_switch_transitions_writes_the_switch   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001, as in 00743 through 00752.

ALTER TABLE kill_switch_transitions ADD COLUMN to_severity text;
ALTER TABLE kill_switch_transitions ADD COLUMN from_version integer;
ALTER TABLE kill_switch_transitions ADD COLUMN release_reason text;

COMMENT ON COLUMN kill_switch_transitions.from_version IS
    'The version this transition expected the switch to be at. The compare-and-swap that used to live in saveSwitch, moved so it applies to every writer (00753).';
COMMENT ON COLUMN kill_switch_transitions.to_severity IS
    'The switch''s severity after this transition. Added rather than derived: severity comes from kind.Severity() on activation and is left alone on release (00753).';

-- +goose StatementBegin
CREATE FUNCTION cp_kill_switch_apply_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    current_version integer;
BEGIN
    SELECT version INTO current_version FROM kill_switches WHERE id = NEW.switch_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'KILL_SWITCH_TRANSITION_ORPHANED: kill_switch_transitions names switch % which does not exist',
            NEW.switch_id USING ERRCODE = 'AD001';
    END IF;
    -- A row that does not state a version is not applied. That is how the birth
    -- row written by `record` right after `insertActive` behaves: the switch is
    -- already in its destination state and there is nothing to move.
    IF NEW.from_version IS NULL THEN
        RETURN NULL;
    END IF;
    IF current_version <> NEW.from_version THEN
        RAISE EXCEPTION 'KILL_SWITCH_CHANGED_CONCURRENTLY: switch % is at version % and this transition expected %',
            NEW.switch_id, current_version, NEW.from_version USING ERRCODE = 'AD001';
    END IF;

    UPDATE kill_switches
       SET active = NEW.to_active,
           severity = coalesce(NEW.to_severity, severity),
           reason = coalesce(NEW.reason, reason),
           -- Activation stamps the activation and clears the release; release
           -- stamps the release and leaves the activation where it was.
           activated_by_actor_id = CASE WHEN NEW.to_active THEN NEW.actor_id ELSE activated_by_actor_id END,
           activated_at          = CASE WHEN NEW.to_active THEN NEW.occurred_at ELSE activated_at END,
           released_by_actor_id  = CASE WHEN NEW.to_active THEN NULL ELSE NEW.actor_id END,
           released_at           = CASE WHEN NEW.to_active THEN NULL ELSE NEW.occurred_at END,
           release_approval_id   = CASE WHEN NEW.to_active THEN NULL ELSE NEW.approval_id END,
           release_reason        = CASE WHEN NEW.to_active THEN NULL ELSE NEW.release_reason END,
           version = version + 1
     WHERE id = NEW.switch_id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER kill_switch_transitions_writes_the_switch
    AFTER INSERT ON kill_switch_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_kill_switch_apply_transition();

REVOKE EXECUTE ON FUNCTION cp_kill_switch_apply_transition() FROM PUBLIC;

REVOKE UPDATE ON kill_switches FROM cp_app;
-- Not for writing. See 00744's rule: a row lock needs UPDATE privilege, and
-- `loadSwitch(forUpdate)` takes one. `scope_id` is the narrowest column that
-- grants it -- it is part of the switch's identity and nothing updates it.
GRANT UPDATE (scope_id) ON kill_switches TO cp_app;

COMMENT ON FUNCTION cp_kill_switch_apply_transition() IS
    'Writes the whole kill switch from its transition row, including the optimistic-concurrency check and the version bump that used to live in saveSwitch. The application holds UPDATE on scope_id only, to permit a row lock rather than a write (00753, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting lets the application release a kill switch with no transition row
