-- +goose Up
-- A run refused because its agent is no longer runnable says so (F-73).
--
-- `Runner.Run` re-reads the pause state at the start of a run and again before
-- every broker call, before the prediction and before the intent, so a PAUSE
-- reaches a run already in flight at five independent points. That is the
-- correct shape and it is well built.
--
-- The agent's LIFECYCLE state is not read at all. `prepare` loads the agent row
-- and takes `stage` and `risk_policy_version` from it, and discards the rest;
-- `ListOpenRuns` selects on the run's status with no predicate on the agent.
-- So REVOKED -- which agent.go calls terminal, "an operator or security
-- decision" -- FAILED and SUPERSEDED all leave every run already open running
-- to completion, including the intent at the end of it.
--
-- Revocation is the control an operator reaches for when an agent is doing
-- something wrong RIGHT NOW, and it was the one lifecycle control that did not
-- reach in-flight work.
--
-- The refusal needs a reason of its own, because the existing ones would each
-- be a lie: AGENT_PAUSED names a different control, and MISSING_DEPENDENCY (the
-- fallback for an undeclared reason) would make an operator go looking at data
-- feeds. AGENT_NOT_RUNNABLE covers all three states with the state itself
-- recorded in agent_runs.error, rather than three near-identical enum values
-- that every reader would have to know to treat alike.

ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_skip_reason_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_skip_reason_check
    CHECK (skip_reason IS NULL OR skip_reason IN (
        'STALE_DATA','MISSING_DEPENDENCY','BUDGET_EXHAUSTED','MODEL_UNAVAILABLE','RATE_LIMITED',
        'CONDITION_FALSE','AGENT_PAUSED','KILL_SWITCH','EFFECT_MISMATCH','ENVELOPE_UNAVAILABLE',
        'AGENT_NOT_RUNNABLE'));

COMMENT ON CONSTRAINT agent_runs_skip_reason_check ON agent_runs IS
    'Mirrors internal/agent.allSkipReasons. TestIntegration_TheSkipReasonsMatchTheDatabase compares the two, because a list kept in two languages diverges.';

-- +goose Down
SELECT 1; -- protected: narrowing the CHECK again would refuse the rows already written for runs a revoked agent was stopped from finishing
