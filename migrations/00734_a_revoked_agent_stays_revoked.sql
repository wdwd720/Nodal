-- +goose Up
-- A revoked agent stays revoked.
--
-- `agent.State.IsTerminal` returns true for REVOKED and SUPERSEDED, and
-- `CanTransition` gives both an empty destination list: in Go, a terminal state
-- has no outgoing edge. The schema said nothing about it, and the two promotion
-- CHECKs on `agent_lifecycle_transitions` both open with the same clause:
--
--     CHECK (from_stage = to_stage OR to_stage NOT IN (...) OR <evidence>)
--     CHECK (from_stage = to_stage OR to_stage NOT IN (...) OR approval_id IS NOT NULL)
--
-- The short-circuit is right for what it was written for: a side transition
-- (pause, resume, revoke) does not move the STAGE, and demanding promotion
-- evidence for one would demand evidence for nothing happening.
--
-- It is wrong for coming BACK. `Lifecycle.Revoke` goes through `sideTransition`,
-- which sets `ToStage: a.Stage` -- so a revoked agent keeps `stage = 'LIVE'`,
-- `mode = 'LIVE'` and its envelope. A row saying
--
--     from_state='REVOKED', to_state='LIVE', from_stage='LIVE', to_stage='LIVE'
--
-- satisfies both CHECKs through that first clause, so it needs **no
-- approval_id, no ir_hash, no risk_policy_hash and no evidence_hash**. The
-- entity's own `state = stage` CHECK is satisfied, the stage binding
-- short-circuits because the stage did not move, and the state binding sees the
-- edge `REVOKED>LIVE` which the row itself flagged. It commits, and an agent
-- that was revoked is trading live capital again with nothing recorded about
-- why (F-114).
--
-- 00726 closed the *promotion* form of `from_stage = to_stage`. This is the
-- *resurrection* form, and it survived because coming back from a side state
-- does not move the stage either.
--
-- The fix is the rule Go already states, moved into the schema: a transition
-- row may not claim to leave a terminal state. That is narrower than requiring
-- evidence on every entry into an operating state -- which would also demand an
-- approval to resume a paused agent, a different decision that is not this
-- finding's to make -- and it is exactly what `CanTransition` enforces.
--
-- Stated on the transitions table rather than on `agents`, because the AU001
-- binding means no state change commits without a row here: refusing the row
-- refuses the change, and it refuses it with a message that says why.

ALTER TABLE agent_lifecycle_transitions
    ADD CONSTRAINT agent_lifecycle_transitions_terminal_is_terminal
    CHECK (from_state <> ALL (ARRAY['REVOKED'::text, 'SUPERSEDED'::text]));

COMMENT ON CONSTRAINT agent_lifecycle_transitions_terminal_is_terminal ON agent_lifecycle_transitions IS
    'A terminal state has no outgoing edge, as agent.State.IsTerminal and CanTransition already say. Without this a revoked agent could return to LIVE with no approval and no evidence, because it keeps its stage and both promotion CHECKs short-circuit when the stage does not move (F-114).';

-- +goose Down
SELECT 1; -- protected: reverting lets a revoked agent return to live capital with no approval and no evidence
