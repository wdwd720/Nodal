-- +goose Up
-- Agents (deployable strategy bindings), lifecycle with promotion evidence (PARTS 68-70), runs carrying the
-- performance mode (PARTS 159, 178), pauses (PART 71). See docs/architecture/AGENT_RUNTIME.md.
-- Custom SQLSTATEs: AG004 immutable agent run field.

CREATE TABLE agents (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES accounts(id),
    strategy_id             uuid NOT NULL REFERENCES strategies(id),
    strategy_version_id     uuid REFERENCES strategy_versions(id),        -- NULL only while DRAFT
    name                    text NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    stage                   text NOT NULL CHECK (stage IN ('DRAFT','COMPILED','VALIDATED','BACKTEST_ELIGIBLE','SHADOW','CANARY','LIMITED','LIVE')),
    state                   text NOT NULL CHECK (state IN ('DRAFT','COMPILED','VALIDATED','BACKTEST_ELIGIBLE','SHADOW','CANARY','LIMITED','LIVE',
                                                           'PAUSED','FAILED','REVOKED','SUPERSEDED')),
    mode                    text CHECK (mode IS NULL OR mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    envelope_id             uuid REFERENCES capital_envelopes(id),
    risk_policy_version     text,
    superseded_by_agent_id  uuid REFERENCES agents(id),
    failure_reason          text,
    version                 bigint NOT NULL DEFAULT 1,                     -- "agent version" recorded on predictions/runs; bumps on rebind
    created_by_actor_type   text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id     text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (state = stage OR state IN ('PAUSED','FAILED','REVOKED','SUPERSEDED')),
    CHECK (stage = 'DRAFT' OR strategy_version_id IS NOT NULL),
    CHECK (stage NOT IN ('CANARY','LIMITED','LIVE') OR envelope_id IS NOT NULL),
    CHECK (
        (stage IN ('DRAFT','COMPILED','VALIDATED') AND mode IS NULL) OR
        (stage = 'BACKTEST_ELIGIBLE' AND mode IN ('BACKTEST','PAPER')) OR
        (stage = 'SHADOW'  AND mode = 'SHADOW')  OR
        (stage = 'CANARY'  AND mode = 'CANARY')  OR
        (stage = 'LIMITED' AND mode = 'LIMITED') OR
        (stage = 'LIVE'    AND mode = 'LIVE')
    )
);
CREATE INDEX agents_account_idx ON agents (account_id);
CREATE INDEX agents_strategy_version_idx ON agents (strategy_version_id) WHERE strategy_version_id IS NOT NULL;
CREATE INDEX agents_active_idx ON agents (state) WHERE state IN ('BACKTEST_ELIGIBLE','SHADOW','CANARY','LIMITED','LIVE');
CREATE TRIGGER agents_updated_at BEFORE UPDATE ON agents FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Close the loop with the capital and risk migrations (agent_id / strategy_version_id were unconstrained uuids).
ALTER TABLE capital_envelopes ADD CONSTRAINT capital_envelopes_agent_fk FOREIGN KEY (agent_id) REFERENCES agents(id);
ALTER TABLE capital_envelopes ADD CONSTRAINT capital_envelopes_strategy_version_fk FOREIGN KEY (strategy_version_id) REFERENCES strategy_versions(id);
ALTER TABLE risk_decisions ADD CONSTRAINT risk_decisions_agent_fk FOREIGN KEY (agent_id) REFERENCES agents(id);

-- One bounded evaluation. Mode is stored directly and can never change (PART 159).
CREATE TABLE agent_runs (
    id                       uuid PRIMARY KEY,
    agent_id                 uuid NOT NULL REFERENCES agents(id),
    agent_version            bigint NOT NULL,
    strategy_version_id      uuid NOT NULL REFERENCES strategy_versions(id),
    account_id               uuid NOT NULL REFERENCES accounts(id),
    envelope_id              uuid REFERENCES capital_envelopes(id),
    mode                     text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    trigger_name             text NOT NULL,
    trigger_kind             text NOT NULL CHECK (trigger_kind IN ('ON_EVENT','ON_INTERVAL')),
    trigger_dedup_key        bytea NOT NULL,
    trigger_event_id         text,
    trigger_source_event_at  timestamptz,
    decision_time            timestamptz NOT NULL,                        -- EvalInput.Now
    status                   text NOT NULL CHECK (status IN ('STARTED','GATHERING','EVALUATED','SKIPPED','PREDICTED','INTENT_CREATED','FAILED')),
    skip_reason              text CHECK (skip_reason IS NULL OR skip_reason IN (
                                 'STALE_DATA','MISSING_DEPENDENCY','BUDGET_EXHAUSTED','MODEL_UNAVAILABLE','RATE_LIMITED',
                                 'CONDITION_FALSE','AGENT_PAUSED','KILL_SWITCH','EFFECT_MISMATCH','ENVELOPE_UNAVAILABLE')),
    evaluator_version        text,
    eval_input_hash          bytea,
    eval_output_hash         bytea,                                       -- EvalOutput.TraceHash
    information_set_hash     bytea,
    signals                  jsonb NOT NULL DEFAULT '[]'::jsonb,
    condition_results        jsonb NOT NULL DEFAULT '[]'::jsonb,
    rationale                jsonb NOT NULL DEFAULT '{}'::jsonb,           -- structured; never chain-of-thought (PART 178)
    evidence                 jsonb NOT NULL DEFAULT '[]'::jsonb,           -- tool invocation ids, model call ids, freshness checks
    tool_calls               integer NOT NULL DEFAULT 0 CHECK (tool_calls >= 0),
    model_calls              integer NOT NULL DEFAULT 0 CHECK (model_calls >= 0),
    data_cost_usd_minor      bigint NOT NULL DEFAULT 0 CHECK (data_cost_usd_minor >= 0),
    model_cost_usd_minor     bigint NOT NULL DEFAULT 0 CHECK (model_cost_usd_minor >= 0),
    prediction_id            uuid,                                        -- FK added in 00502
    intent_id                uuid REFERENCES trade_intents(id),
    error                    text,
    started_at               timestamptz NOT NULL DEFAULT now(),
    finished_at              timestamptz,
    correlation_id           text NOT NULL,
    build_version            text,
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (agent_id, trigger_dedup_key),
    CHECK (status <> 'SKIPPED' OR skip_reason IS NOT NULL),
    CHECK (status <> 'INTENT_CREATED' OR (intent_id IS NOT NULL AND prediction_id IS NOT NULL))
);
CREATE INDEX agent_runs_agent_idx ON agent_runs (agent_id, started_at DESC);
CREATE INDEX agent_runs_open_idx ON agent_runs (status) WHERE status IN ('STARTED','GATHERING','EVALUATED','PREDICTED');
CREATE INDEX agent_runs_strategy_mode_idx ON agent_runs (strategy_version_id, mode, decision_time);

-- +goose StatementBegin
CREATE FUNCTION agent_runs_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'AGENT_RUN_IMMUTABLE: runs are never deleted' USING ERRCODE = 'AG004';
    END IF;
    IF NEW.mode IS DISTINCT FROM OLD.mode OR NEW.agent_id IS DISTINCT FROM OLD.agent_id
       OR NEW.agent_version IS DISTINCT FROM OLD.agent_version OR NEW.strategy_version_id IS DISTINCT FROM OLD.strategy_version_id
       OR NEW.account_id IS DISTINCT FROM OLD.account_id OR NEW.envelope_id IS DISTINCT FROM OLD.envelope_id
       OR NEW.trigger_dedup_key IS DISTINCT FROM OLD.trigger_dedup_key OR NEW.decision_time IS DISTINCT FROM OLD.decision_time
       OR NEW.started_at IS DISTINCT FROM OLD.started_at
       OR (OLD.prediction_id IS NOT NULL AND NEW.prediction_id IS DISTINCT FROM OLD.prediction_id)
       OR (OLD.intent_id IS NOT NULL AND NEW.intent_id IS DISTINCT FROM OLD.intent_id) THEN
        RAISE EXCEPTION 'AGENT_RUN_IMMUTABLE: identity, mode and linkage of run % cannot change', OLD.id USING ERRCODE = 'AG004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER agent_runs_guard BEFORE UPDATE OR DELETE ON agent_runs FOR EACH ROW EXECUTE FUNCTION agent_runs_guard();
CREATE TRIGGER agent_runs_updated_at BEFORE UPDATE ON agent_runs FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Immutable lifecycle history. Promotions must carry evidence (PARTS 69, 70).
CREATE TABLE agent_lifecycle_transitions (
    id                        uuid PRIMARY KEY,
    agent_id                  uuid NOT NULL REFERENCES agents(id),
    from_state                text NOT NULL,
    to_state                  text NOT NULL,
    from_stage                text NOT NULL,
    to_stage                  text NOT NULL,
    actor_type                text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SYSTEM')),
    actor_id                  text NOT NULL,
    reason                    text NOT NULL,
    approval_id               uuid REFERENCES admin_actions(id),
    strategy_version_id       uuid REFERENCES strategy_versions(id),
    ir_hash                   bytea,
    evaluation_dataset_ref    text,
    evaluation_dataset_hash   bytea,
    risk_policy_version       text,
    risk_policy_hash          bytea,
    backtest_id               uuid,                                       -- FK added in 00601
    performance_snapshot_id   uuid,                                       -- FK added in 00601
    calibration_snapshot_id   uuid,                                       -- FK added in 00502
    error_rate_bps            integer CHECK (error_rate_bps IS NULL OR error_rate_bps BETWEEN 0 AND 10000),
    operational_health        jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence                  jsonb NOT NULL DEFAULT '[]'::jsonb,           -- [{kind, ref, hash}]
    evidence_hash             bytea,
    build_version             text,
    correlation_id            text,
    occurred_at               timestamptz NOT NULL DEFAULT now(),
    -- Ladder promotions (stage changes into SHADOW or beyond) require hashed evidence; pauses/resumes keep the stage.
    CHECK (from_stage = to_stage OR to_stage NOT IN ('SHADOW','CANARY','LIMITED','LIVE')
           OR (strategy_version_id IS NOT NULL AND ir_hash IS NOT NULL AND risk_policy_version IS NOT NULL
               AND risk_policy_hash IS NOT NULL AND evidence_hash IS NOT NULL)),
    CHECK (from_stage = to_stage OR to_stage NOT IN ('CANARY','LIMITED','LIVE') OR approval_id IS NOT NULL)
);
CREATE INDEX agent_lifecycle_transitions_agent_idx ON agent_lifecycle_transitions (agent_id, occurred_at);
CREATE TRIGGER agent_lifecycle_transitions_immutable BEFORE UPDATE OR DELETE ON agent_lifecycle_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Pause records (PART 71). At most one open pause per agent; resume always by a person.
CREATE TABLE agent_pauses (
    id                       uuid PRIMARY KEY,
    agent_id                 uuid NOT NULL REFERENCES agents(id),
    reason_code              text NOT NULL CHECK (reason_code IN (
                                 'OWNER_REQUEST','OPERATOR','KILL_SWITCH','BUDGET_EXHAUSTED','MODEL_UNAVAILABLE','DATA_GAP',
                                 'RISK_VIOLATION','RECONCILIATION_MISMATCH','SECURITY')),
    reason                   text NOT NULL,
    kill_switch_id           uuid REFERENCES kill_switches(id),
    open_orders_policy       text NOT NULL CHECK (open_orders_policy IN ('LEAVE','CANCEL_CANCELABLE')),
    cancel_workflow_id       text,
    paused_by_actor_type     text NOT NULL CHECK (paused_by_actor_type IN ('USER','OPERATOR','SYSTEM')),
    paused_by_actor_id       text NOT NULL,
    paused_at                timestamptz NOT NULL DEFAULT now(),
    resumed_at               timestamptz,
    resumed_by_actor_type    text CHECK (resumed_by_actor_type IS NULL OR resumed_by_actor_type IN ('USER','OPERATOR')),
    resumed_by_actor_id      text,
    resume_reason            text,
    correlation_id           text,
    updated_at               timestamptz NOT NULL DEFAULT now(),
    CHECK ((resumed_at IS NULL) = (resumed_by_actor_id IS NULL)),
    CHECK (reason_code <> 'KILL_SWITCH' OR kill_switch_id IS NOT NULL)
);
CREATE UNIQUE INDEX agent_pauses_open_idx ON agent_pauses (agent_id) WHERE resumed_at IS NULL;
CREATE INDEX agent_pauses_agent_idx ON agent_pauses (agent_id, paused_at DESC);
CREATE TRIGGER agent_pauses_updated_at BEFORE UPDATE ON agent_pauses FOR EACH ROW EXECUTE FUNCTION set_updated_at();

GRANT SELECT, INSERT, UPDATE ON agents, agent_runs, agent_pauses TO cp_app;
GRANT SELECT, INSERT ON agent_lifecycle_transitions TO cp_app;
GRANT SELECT ON agents, agent_runs, agent_lifecycle_transitions, agent_pauses TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: agent runs and lifecycle history are audit record referenced by intents and predictions
