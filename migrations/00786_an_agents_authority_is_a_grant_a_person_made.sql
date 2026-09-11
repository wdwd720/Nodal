-- +goose Up
-- An agent's authority is a grant a person made, and the limits are that grant's.
--
-- `agents` (00501) records what an agent IS: an account, a compiled strategy
-- version, a rung on the promotion ladder and a performance mode. It records
-- nothing about what the OWNER agreed to -- how much authority they granted,
-- how many Credits they are prepared to put at risk, which assets the agent may
-- touch, how often it may look. Those are the product's own questions (goal
-- §17, §18) and until now there was nowhere to put the answers.
--
-- They do not belong on `agents`, for the reason 00750 gives about `mode`: the
-- columns on that table describe authority the PLATFORM granted through the
-- promotion ladder, with an approval and evidence behind each rung. What this
-- table holds is authority the USER granted, in one act, when they read the
-- compiled strategy and pressed the button. Two different grantors, two
-- different revocation stories, two tables.
--
-- ## What is deliberately not here
--
-- **No Credits move.** `budget_credits` is a CEILING, not a reservation: no
-- ledger row is written when an agent is created, because nothing has been
-- spent and the Credits remain the user's to spend elsewhere. A reservation
-- that removed spendable Credits the moment an agent was created would be a
-- financial movement with no economic event behind it, and `internal/credit`
-- is the only thing that may move Credits at all. What consumes budget is a
-- filled intent, and the read model derives that from `agent_runs` (D-076).
--
-- **No state column, and therefore no F-42 binding.** A grant is not a state
-- machine: it is made once, it is archived once, and everything that happens in
-- between is the AGENT's lifecycle, which `agent_lifecycle_transitions` already
-- binds. `archived_at` is a timestamp on a row whose authority fields cannot
-- change, not a status somebody could walk backwards.
--
-- **No level above 3.** `authority_level` mirrors agentauthority.Level and the
-- CHECK stops at agentauthority.MaxSupportedLevel. Levels 4, 5 and 6 exist in
-- the Go matrix, are refused by it, and each requires its own capability gate
-- (AGENT_BOUNDED_DISCRETION, AGENT_AUTONOMOUS_SELECTION,
-- AGENT_AUTONOMOUS_PORTFOLIO). Widening this CHECK is therefore a migration
-- AND a gate ceremony, which is the right price for the step from "executes the
-- rule I read" to "chooses on my behalf".
--
-- Custom SQLSTATEs: AG005 immutable agent grant field.

CREATE TABLE agent_grants (
    id                          uuid PRIMARY KEY,
    agent_id                    uuid NOT NULL UNIQUE REFERENCES agents(id),
    account_id                  uuid NOT NULL REFERENCES accounts(id),
    strategy_version_id         uuid NOT NULL REFERENCES strategy_versions(id),

    -- agentauthority.Level. 0 RESEARCH_ONLY, 1 RECOMMENDATION,
    -- 2 PREPARE_TRANSACTION, 3 USER_APPROVED_RULE.
    authority_level             integer NOT NULL CHECK (authority_level BETWEEN 0 AND 3),

    -- Credits, in base units, as exact integers. Never a float, never a
    -- currency amount: Credits are a closed-loop internal unit and this is a
    -- ceiling on how many of them the agent may put at risk.
    budget_credits              numeric(38,0) NOT NULL CHECK (budget_credits > 0),
    per_trade_cap_credits       numeric(38,0) NOT NULL CHECK (per_trade_cap_credits > 0),
    daily_loss_stop_credits     numeric(38,0) NOT NULL CHECK (daily_loss_stop_credits > 0),
    max_position_share_bps      integer NOT NULL CHECK (max_position_share_bps BETWEEN 1 AND 10000),

    -- The universe. Empty is not "everything": the service refuses a grant with
    -- no assets, because an agent whose universe is unstated is an agent whose
    -- universe is whatever the strategy happens to name.
    allowed_asset_ids           uuid[] NOT NULL CHECK (cardinality(allowed_asset_ids) BETWEEN 1 AND 64),

    schedule_kind               text NOT NULL CHECK (schedule_kind IN ('MANUAL','INTERVAL')),
    schedule_interval_minutes   integer CHECK (schedule_interval_minutes IS NULL OR schedule_interval_minutes BETWEEN 5 AND 10080),

    granted_by_user_id          uuid NOT NULL REFERENCES users(id),
    granted_at                  timestamptz NOT NULL DEFAULT now(),
    archived_at                 timestamptz,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),

    -- A cap larger than the budget it caps is not a cap.
    CHECK (per_trade_cap_credits <= budget_credits),
    CHECK (daily_loss_stop_credits <= budget_credits),
    -- The schedule states its own frequency, or says it has none.
    CHECK ((schedule_kind = 'INTERVAL') = (schedule_interval_minutes IS NOT NULL))
);
CREATE INDEX agent_grants_account_idx ON agent_grants (account_id, granted_at DESC);
CREATE INDEX agent_grants_live_idx ON agent_grants (account_id) WHERE archived_at IS NULL;
CREATE TRIGGER agent_grants_updated_at BEFORE UPDATE ON agent_grants FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The authority a person granted is not editable in place. Raising a budget or
-- a level is a new decision by that person, and a row that could be edited
-- would make every audit of "what did the user agree to" a question about when
-- you looked. Only `archived_at` moves, once, forwards.
-- +goose StatementBegin
CREATE FUNCTION agent_grants_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AGENT_GRANT_IMMUTABLE: a grant of authority is archived, never deleted' USING ERRCODE = 'AG005';
    END IF;
    IF NEW.agent_id IS DISTINCT FROM OLD.agent_id
       OR NEW.account_id IS DISTINCT FROM OLD.account_id
       OR NEW.strategy_version_id IS DISTINCT FROM OLD.strategy_version_id
       OR NEW.authority_level IS DISTINCT FROM OLD.authority_level
       OR NEW.budget_credits IS DISTINCT FROM OLD.budget_credits
       OR NEW.per_trade_cap_credits IS DISTINCT FROM OLD.per_trade_cap_credits
       OR NEW.daily_loss_stop_credits IS DISTINCT FROM OLD.daily_loss_stop_credits
       OR NEW.max_position_share_bps IS DISTINCT FROM OLD.max_position_share_bps
       OR NEW.allowed_asset_ids IS DISTINCT FROM OLD.allowed_asset_ids
       OR NEW.schedule_kind IS DISTINCT FROM OLD.schedule_kind
       OR NEW.schedule_interval_minutes IS DISTINCT FROM OLD.schedule_interval_minutes
       OR NEW.granted_by_user_id IS DISTINCT FROM OLD.granted_by_user_id
       OR NEW.granted_at IS DISTINCT FROM OLD.granted_at THEN
        RAISE EXCEPTION 'AGENT_GRANT_IMMUTABLE: the authority granted to agent % cannot change; archive this grant and make another',
            OLD.agent_id USING ERRCODE = 'AG005';
    END IF;
    IF OLD.archived_at IS NOT NULL AND NEW.archived_at IS DISTINCT FROM OLD.archived_at THEN
        RAISE EXCEPTION 'AGENT_GRANT_IMMUTABLE: agent % is already archived' , OLD.agent_id USING ERRCODE = 'AG005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER agent_grants_guard BEFORE UPDATE OR DELETE ON agent_grants
    FOR EACH ROW EXECUTE FUNCTION agent_grants_guard();

COMMENT ON TABLE agent_grants IS
    'What the OWNER granted an agent: authority level, Credit ceilings, universe and frequency. Distinct from the columns on `agents`, which record what the promotion ladder granted. Immutable but for archived_at (00786).';
COMMENT ON COLUMN agent_grants.budget_credits IS
    'A ceiling on Credits at risk, not a reservation. No ledger row is written when a grant is made; internal/credit remains the only thing that moves Credits (D-076).';
COMMENT ON CONSTRAINT agent_grants_authority_level_check ON agent_grants IS
    'Mirrors agentauthority.MaxSupportedLevel. Levels 4-6 are declared in Go, refused there, and each needs its own capability gate; widening this is a migration and a gate ceremony.';

GRANT SELECT, INSERT, UPDATE (archived_at) ON agent_grants TO cp_app;
GRANT SELECT ON agent_grants TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: an agent grant is the record of what a person authorised, and agent runs and lifecycle history reference the agents it binds
