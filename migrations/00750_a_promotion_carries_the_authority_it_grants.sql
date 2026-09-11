-- +goose Up
-- A promotion carries the authority it grants, and an agent's state is not the
-- application's to write.
--
-- F-42's stronger remedy, table eight of eleven, and the one where granting the
-- obvious columns back would have undone another finding.
--
-- ## Why the obvious grant-back was refused
--
-- `updateAgentStateSQL` writes eight columns. Five come off the transition row
-- (`stage`, `state`, `strategy_version_id`, `risk_policy_version`, and
-- `failure_reason` from `reason`). Three do not: **`mode`, `envelope_id` and
-- `superseded_by_agent_id`.**
--
-- The pattern so far has been to grant such columns back. Here that would have
-- been wrong, and the reason is 00739. That migration refuses an agent BORN in a
-- real-capital mode or with a capital envelope, because "real capital is granted
-- by a promotion". Granting `cp_app` UPDATE on `mode` and `envelope_id` would
-- have left the application able to write LIVE and an envelope onto an existing
-- agent with no transition row and no approval -- reaching by UPDATE exactly
-- what 00739 refuses at INSERT.
--
-- So the columns move onto the transition instead. **A promotion is the thing
-- that grants authority, so the authority it grants belongs on it**, which is
-- the same argument 00748 made for `rejection_code` and is stronger here
-- because what is being granted is the right to move real money.
--
-- `superseded_by_agent_id` moves for the same reason: an agent is superseded BY
-- a transition to SUPERSEDED, and which agent replaced it is part of that
-- transition rather than a fact discovered later.
--
-- ## Why `to_failure_reason` is separate from `reason`
--
-- `reason` explains the transition. `failure_reason` is a property of the agent
-- afterwards. They coincide on the way INTO FAILED and diverge on the way out:
-- a transition out of FAILED has a reason and must CLEAR the failure. Deriving
-- one from the other, as the first draft did, would have left a recovered agent
-- carrying the explanation of the failure it recovered from.
--
-- ## What can be checked here, and what deliberately is not
--
-- One CHECK: a transition that grants a real-capital mode must also name an
-- envelope. That mirrors `NewAuthority`, which already refuses a real-capital
-- stage with no envelope, and 00739, which refuses both at birth.
--
-- The first draft said the mode could not be checked against the stage, because
-- BACKTEST_ELIGIBLE permits either BACKTEST or PAPER. That was wrong in the way
-- that matters: it is not a FUNCTION of the stage, but it is CONSTRAINED by it,
-- and `agents_check3` has said so since 00501. The mirrored CHECK below permits
-- both modes at that stage and refuses everything the agents table would.
--
-- ## Two audited columns, not one
--
-- This is the first table where the trigger writes two columns that are each
-- separately bound: `state` (`agents_require_transition`) and `stage`
-- (`agents_stage_requires_transition`), with two flag setters on the transitions
-- table. Both flags are set before this trigger runs, for the naming reason
-- below, so both bindings see what they need.
--
-- ## The grant-back is `name`, purely for the lock
--
-- Nothing in this repository updates `agents.name` -- checked, not assumed --
-- and `GetForUpdate` needs UPDATE privilege for its row lock (00744's rule). It
-- is the least consequential column on the table: an agent's identity is its
-- id, its authority is its stage, mode and envelope, and none of those is
-- reachable this way any more.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   agent_lifecycle_transitions_flag_state_edge
--   agent_lifecycle_transitions_flag_stage_edge
--   agent_lifecycle_transitions_writes_the_state   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001, as in 00743 through 00749.

-- Six columns, one for every mutable agent column the old UPDATE wrote that is
-- not already `to_state` or `to_stage`. All six, not the three that looked
-- interesting, because the old statement assigned DIRECTLY -- a nil strategy
-- version CLEARED the agent's -- and a trigger that coalesced instead would
-- quietly preserve values the code intended to drop.
--
-- The first draft of this migration did coalesce, and `agents_check3` caught it
-- immediately: that CHECK ties stage to mode (VALIDATED requires mode IS NULL,
-- SHADOW requires SHADOW, and so on), so a transition down to VALIDATED that
-- preserved a mode instead of clearing it produced a row the table refuses. The
-- constraint found the bug the same day the trigger was written, which is the
-- argument for having it.
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_mode text;
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_envelope_id uuid;
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_strategy_version_id uuid;
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_risk_policy_version text;
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_superseded_by_agent_id uuid;
ALTER TABLE agent_lifecycle_transitions ADD COLUMN to_failure_reason text;

-- The row must describe a LEGAL destination, not merely a destination.
--
-- Direct assignment has a consequence worth stating: a transition row that omits
-- `to_mode` clears the agent's mode, and for every stage but the first three
-- that produces a row `agents_check3` refuses. The first draft discovered this
-- through a hand-built test row and an error naming a constraint on a table the
-- caller had not touched.
--
-- So the three CHECKs that constrain an agent's authority are mirrored onto the
-- transition, against its `to_*` columns. An incomplete row is now refused where
-- it is written, naming the rule it broke, instead of producing a violation on
-- `agents` that reads as a bug in the trigger.
--
-- These are `agents_check1`, `_check2` and `_check3` restated. Three copies of a
-- rule is normally what this register complains about -- but these are not
-- copies that can drift apart unnoticed: the agent's own CHECKs still stand, so
-- a transition row that satisfied a stale copy here would still be refused
-- there. This layer exists to move the error to where the mistake was made.
--
-- NOT VALID for the reason 00748 gives: rows written before these columns
-- existed carry NULL in all of them and cannot be backfilled truthfully.
ALTER TABLE agent_lifecycle_transitions
    ADD CONSTRAINT agent_lifecycle_transitions_destination_is_legal
    CHECK (
        -- agents_check1: past DRAFT, a strategy version is named.
        (to_stage = 'DRAFT' OR to_strategy_version_id IS NOT NULL)
        -- agents_check2: a real-capital stage names an envelope.
        AND (to_stage <> ALL (ARRAY['CANARY'::text, 'LIMITED'::text, 'LIVE'::text])
             OR to_envelope_id IS NOT NULL)
        -- agents_check3: the mode a stage permits, and only that.
        AND ((to_stage = ANY (ARRAY['DRAFT'::text, 'COMPILED'::text, 'VALIDATED'::text]) AND to_mode IS NULL)
             OR (to_stage = 'BACKTEST_ELIGIBLE' AND to_mode = ANY (ARRAY['BACKTEST'::text, 'PAPER'::text]))
             OR (to_stage = 'SHADOW'  AND to_mode = 'SHADOW')
             OR (to_stage = 'CANARY'  AND to_mode = 'CANARY')
             OR (to_stage = 'LIMITED' AND to_mode = 'LIMITED')
             OR (to_stage = 'LIVE'    AND to_mode = 'LIVE'))
    ) NOT VALID;

COMMENT ON COLUMN agent_lifecycle_transitions.to_mode IS
    'The mode this transition grants. On the transition rather than written beside it, because a promotion is what grants real-capital authority and 00739 refuses an agent born holding it (00750).';
COMMENT ON COLUMN agent_lifecycle_transitions.to_envelope_id IS
    'The capital envelope this transition grants. Required whenever to_mode is a real-capital mode (00750).';
COMMENT ON COLUMN agent_lifecycle_transitions.to_failure_reason IS
    'The agent''s failure_reason after this transition. Separate from `reason`, which explains the transition itself: a transition OUT of FAILED has a reason and clears the failure (00750).';

-- +goose StatementBegin
CREATE FUNCTION cp_agent_apply_lifecycle_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    -- Direct assignment, not coalesce. The row states the agent's whole
    -- destination, so a NULL means "cleared" and not "unspecified" -- which is
    -- exactly what the UPDATE this replaces did, and what agents_check3
    -- requires when a stage change drops the mode.
    UPDATE agents
       SET stage = NEW.to_stage,
           state = NEW.to_state,
           mode = NEW.to_mode,
           envelope_id = NEW.to_envelope_id,
           strategy_version_id = NEW.to_strategy_version_id,
           risk_policy_version = NEW.to_risk_policy_version,
           superseded_by_agent_id = NEW.to_superseded_by_agent_id,
           failure_reason = NEW.to_failure_reason
     WHERE id = NEW.agent_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'AGENT_TRANSITION_ORPHANED: agent_lifecycle_transitions names agent % which does not exist',
            NEW.agent_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agent_lifecycle_transitions_writes_the_state
    AFTER INSERT ON agent_lifecycle_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_agent_apply_lifecycle_transition();

REVOKE EXECUTE ON FUNCTION cp_agent_apply_lifecycle_transition() FROM PUBLIC;

REVOKE UPDATE ON agents FROM cp_app;
-- Not for writing. See the header: a row lock needs UPDATE privilege, and this
-- is the least consequential column on the table -- nothing updates it, and an
-- agent's authority is its stage, mode and envelope, none of which is reachable
-- this way any more.
GRANT UPDATE (name) ON agents TO cp_app;

COMMENT ON FUNCTION cp_agent_apply_lifecycle_transition() IS
    'Writes every mutable agent column from the transition row, including the mode and envelope a promotion grants. The application holds UPDATE on name only, to permit a row lock rather than a write (00750, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting lets the application grant an agent real-capital mode by UPDATE
