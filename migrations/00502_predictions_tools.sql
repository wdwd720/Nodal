-- +goose Up
-- Tool registry and invocation provenance (PART 66), runtime model provenance (PART 65), prediction ledger and
-- calibration (PARTS 72, 73). Adds the FKs from trade_intents / agent_runs into the agent subsystem.
-- Custom SQLSTATEs: AG001 agent intent without prediction, AG002 prediction after intent, AG003 prediction/intent mismatch.

CREATE TABLE tools (
    id                       uuid PRIMARY KEY,
    code                     text NOT NULL,
    version                  integer NOT NULL CHECK (version >= 1),
    effect                   text NOT NULL CHECK (effect IN ('READ_MARKET_DATA','READ_ONCHAIN_DATA','READ_APPROVED_SOCIAL_DATA','READ_WALLET_INTELLIGENCE','CALL_MODEL')),
    provider                 text NOT NULL,
    data_source_id           uuid,                                       -- FK added in 00600
    description              text NOT NULL DEFAULT '',
    egress_hosts             text[] NOT NULL DEFAULT '{}',               -- the only hosts the adapter may dial
    input_schema_version     integer NOT NULL DEFAULT 1 CHECK (input_schema_version >= 1),
    output_schema_version    integer NOT NULL DEFAULT 1 CHECK (output_schema_version >= 1),
    output_schema            jsonb NOT NULL,
    cost_per_call_usd_minor  bigint NOT NULL DEFAULT 0 CHECK (cost_per_call_usd_minor >= 0),
    max_calls_per_minute     integer NOT NULL CHECK (max_calls_per_minute > 0),
    max_calls_per_run        integer NOT NULL CHECK (max_calls_per_run > 0),
    timeout_ms               integer NOT NULL CHECK (timeout_ms > 0),
    pipeline_latency_ms      integer NOT NULL DEFAULT 0 CHECK (pipeline_latency_ms >= 0),   -- added when deriving decision_available_at
    status                   text NOT NULL CHECK (status IN ('ACTIVE','DEGRADED','DISABLED')),
    environments             text[] NOT NULL DEFAULT '{}',
    created_by_actor_type    text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id      text NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (code, version)
);
CREATE TRIGGER tools_updated_at BEFORE UPDATE ON tools FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE strategy_dependencies ADD CONSTRAINT strategy_dependencies_tool_fk
    FOREIGN KEY (tool_code, tool_version) REFERENCES tools(code, version);

-- One row per broker call (PART 66): request/output hashes, source, receipt time, cost, latency, success.
CREATE TABLE tool_invocations (
    id                      uuid PRIMARY KEY,
    tool_id                 uuid NOT NULL REFERENCES tools(id),
    tool_code               text NOT NULL,
    tool_version            integer NOT NULL,
    effect                  text NOT NULL,
    agent_id                uuid REFERENCES agents(id),
    agent_run_id            uuid REFERENCES agent_runs(id),
    strategy_version_id     uuid REFERENCES strategy_versions(id),
    compile_attempt_id      uuid REFERENCES compile_attempts(id),
    backtest_id             uuid,                                        -- FK added in 00601
    dependency_name         text,
    mode                    text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    request_hash            bytea NOT NULL,
    request_ref             text,
    output_hash             bytea,
    output_ref              text,                                        -- archive URI of the typed output
    source                  text NOT NULL,                               -- provider / data source that answered
    source_event_at         timestamptz,
    provider_published_at   timestamptz,
    received_at             timestamptz NOT NULL,
    decision_available_at   timestamptz,
    cost_usd_minor          bigint NOT NULL DEFAULT 0 CHECK (cost_usd_minor >= 0),
    latency_ms              integer NOT NULL CHECK (latency_ms >= 0),
    success                 boolean NOT NULL,
    error_code              text,
    rate_limited            boolean NOT NULL DEFAULT false,
    budget_refused          boolean NOT NULL DEFAULT false,
    correlation_id          text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (NOT success OR output_hash IS NOT NULL),
    CHECK (agent_run_id IS NOT NULL OR compile_attempt_id IS NOT NULL OR backtest_id IS NOT NULL)
);
CREATE INDEX tool_invocations_run_idx ON tool_invocations (agent_run_id) WHERE agent_run_id IS NOT NULL;
CREATE INDEX tool_invocations_agent_day_idx ON tool_invocations (agent_id, created_at) WHERE agent_id IS NOT NULL;   -- budgets and rate limits
CREATE INDEX tool_invocations_tool_idx ON tool_invocations (tool_id, created_at);
CREATE TRIGGER tool_invocations_immutable BEFORE UPDATE OR DELETE ON tool_invocations
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Model provenance for runtime and compile-time calls (PART 65). No chain-of-thought is ever stored.
CREATE TABLE model_calls (
    id                       uuid PRIMARY KEY,
    tool_invocation_id       uuid REFERENCES tool_invocations(id),
    compile_attempt_id       uuid REFERENCES compile_attempts(id),
    agent_run_id             uuid REFERENCES agent_runs(id),
    strategy_version_id      uuid REFERENCES strategy_versions(id),
    purpose                  text NOT NULL CHECK (purpose IN ('COMPILE','RUNTIME','BACKTEST')),
    mode                     text CHECK (mode IS NULL OR mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    provider                 text NOT NULL,
    model_id                 text NOT NULL,
    prompt_template_version  text NOT NULL,
    request_at               timestamptz NOT NULL,
    response_at              timestamptz,
    input_hash               bytea NOT NULL,
    output_hash              bytea,
    prompt_ref               text,                                       -- archive URI, retention class MODEL_IO
    response_ref             text,
    structured_output        jsonb,
    parse_result             text NOT NULL CHECK (parse_result IN ('OK','INVALID_JSON','SCHEMA_VIOLATION','TOO_LARGE','NOT_ATTEMPTED')),
    input_tokens             bigint NOT NULL DEFAULT 0 CHECK (input_tokens >= 0),
    output_tokens            bigint NOT NULL DEFAULT 0 CHECK (output_tokens >= 0),
    cost_usd_minor           bigint NOT NULL DEFAULT 0 CHECK (cost_usd_minor >= 0),
    success                  boolean NOT NULL,
    error_code               text,
    stop_reason              text,
    created_at               timestamptz NOT NULL DEFAULT now(),
    CHECK (tool_invocation_id IS NOT NULL OR compile_attempt_id IS NOT NULL)
);
CREATE INDEX model_calls_run_idx ON model_calls (agent_run_id) WHERE agent_run_id IS NOT NULL;
CREATE INDEX model_calls_strategy_day_idx ON model_calls (strategy_version_id, request_at) WHERE strategy_version_id IS NOT NULL;
CREATE TRIGGER model_calls_immutable BEFORE UPDATE OR DELETE ON model_calls
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Prediction ledger (PART 72). Probabilities are exact decimals, never floats. Immutable.
CREATE TABLE predictions (
    id                       uuid PRIMARY KEY,
    agent_id                 uuid NOT NULL REFERENCES agents(id),
    agent_version            bigint NOT NULL,
    agent_run_id             uuid NOT NULL REFERENCES agent_runs(id),
    action_name              text NOT NULL,
    strategy_version_id      uuid NOT NULL REFERENCES strategy_versions(id),
    account_id               uuid NOT NULL REFERENCES accounts(id),
    mode                     text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    instrument_id            uuid NOT NULL REFERENCES instruments(id),
    horizon_ms               bigint NOT NULL CHECK (horizon_ms > 0),
    direction                text CHECK (direction IS NULL OR direction IN ('UP','DOWN','FLAT')),
    probability_direction    numeric(7,6) CHECK (probability_direction IS NULL OR (probability_direction >= 0 AND probability_direction <= 1)),
    expected_return_bps      integer NOT NULL,
    downside_probability     numeric(7,6) NOT NULL CHECK (downside_probability >= 0 AND downside_probability <= 1),
    max_downside_bps         integer NOT NULL CHECK (max_downside_bps >= 0),
    confidence               numeric(7,6) NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
    information_set_hash     bytea NOT NULL CHECK (length(information_set_hash) = 32),
    decision_available_at    timestamptz NOT NULL,                       -- max decision_available_at over the information set
    committed_at             timestamptz NOT NULL DEFAULT now(),
    model_call_id            uuid REFERENCES model_calls(id),
    template_version         text,
    rationale                jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence_refs            jsonb NOT NULL DEFAULT '[]'::jsonb,
    signals                  jsonb NOT NULL DEFAULT '[]'::jsonb,
    correlation_id           text,
    UNIQUE (agent_run_id, action_name),
    CHECK (decision_available_at <= committed_at),
    CHECK ((direction IS NULL) = (probability_direction IS NULL))
);
CREATE INDEX predictions_strategy_mode_idx ON predictions (strategy_version_id, mode, committed_at);
CREATE INDEX predictions_agent_idx ON predictions (agent_id, committed_at DESC);
CREATE INDEX predictions_unresolved_idx ON predictions (committed_at);
CREATE TRIGGER predictions_immutable BEFORE UPDATE OR DELETE ON predictions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_prediction_fk FOREIGN KEY (prediction_id) REFERENCES predictions(id);
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_agent_fk FOREIGN KEY (agent_id) REFERENCES agents(id);
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_strategy_version_fk FOREIGN KEY (strategy_version_id) REFERENCES strategy_versions(id);
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_prediction_fk FOREIGN KEY (prediction_id) REFERENCES predictions(id);

-- +goose StatementBegin
-- Prediction must predate execution (PART 72) and belong to the same agent, strategy version and mode.
CREATE FUNCTION trade_intents_prediction_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE p predictions%ROWTYPE;
BEGIN
    IF NEW.actor_type = 'AGENT' AND (NEW.prediction_id IS NULL OR NEW.strategy_version_id IS NULL OR NEW.agent_id IS NULL) THEN
        RAISE EXCEPTION 'AGENT_INTENT_REQUIRES_PREDICTION: intent % lacks prediction/strategy/agent linkage', NEW.id
            USING ERRCODE = 'AG001';
    END IF;
    IF NEW.prediction_id IS NOT NULL THEN
        SELECT * INTO p FROM predictions WHERE id = NEW.prediction_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'PREDICTION_NOT_FOUND: %', NEW.prediction_id USING ERRCODE = 'AG003';
        END IF;
        IF p.committed_at > NEW.requested_at THEN
            RAISE EXCEPTION 'PREDICTION_MUST_PREDATE_INTENT: prediction % committed at % after intent requested at %',
                p.id, p.committed_at, NEW.requested_at USING ERRCODE = 'AG002';
        END IF;
        IF p.agent_id IS DISTINCT FROM NEW.agent_id OR p.strategy_version_id IS DISTINCT FROM NEW.strategy_version_id
           OR p.mode <> NEW.mode OR p.account_id <> NEW.account_id THEN
            RAISE EXCEPTION 'PREDICTION_INTENT_MISMATCH: prediction % does not match intent % (agent/strategy/mode/account)',
                p.id, NEW.id USING ERRCODE = 'AG003';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER trade_intents_prediction_guard BEFORE INSERT ON trade_intents
    FOR EACH ROW EXECUTE FUNCTION trade_intents_prediction_guard();

-- Realized outcome per prediction, computed after the horizon (PART 73). Scores are exact decimals.
CREATE TABLE prediction_outcomes (
    id                          uuid PRIMARY KEY,
    prediction_id               uuid NOT NULL UNIQUE REFERENCES predictions(id),
    strategy_version_id         uuid NOT NULL REFERENCES strategy_versions(id),
    mode                        text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    horizon_end_at              timestamptz NOT NULL,
    resolved_at                 timestamptz NOT NULL,
    realized_direction          text CHECK (realized_direction IS NULL OR realized_direction IN ('UP','DOWN','FLAT')),
    realized_return_bps         integer NOT NULL,
    realized_max_drawdown_bps   integer NOT NULL CHECK (realized_max_drawdown_bps >= 0),
    direction_hit               boolean,
    brier                       numeric(12,8) CHECK (brier IS NULL OR (brier >= 0 AND brier <= 1)),
    log_loss                    numeric(12,8) CHECK (log_loss IS NULL OR log_loss >= 0),
    abs_return_error_bps        integer NOT NULL CHECK (abs_return_error_bps >= 0),
    regime_label                text NOT NULL DEFAULT 'UNLABELLED',
    valuation_source            text NOT NULL,
    price_ref_start             text NOT NULL,
    price_ref_end               text NOT NULL,
    resolver_version            text NOT NULL,
    created_at                  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX prediction_outcomes_strategy_idx ON prediction_outcomes (strategy_version_id, mode, resolved_at);
CREATE TRIGGER prediction_outcomes_immutable BEFORE UPDATE OR DELETE ON prediction_outcomes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Calibration aggregates per (strategy version, mode, window, regime, probability bucket) (PART 73).
CREATE TABLE calibration_snapshots (
    id                          uuid PRIMARY KEY,
    strategy_version_id         uuid NOT NULL REFERENCES strategy_versions(id),
    agent_id                    uuid REFERENCES agents(id),
    mode                        text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    window_start                timestamptz NOT NULL,
    window_end                  timestamptz NOT NULL,
    regime_label                text NOT NULL DEFAULT 'ALL',
    bucket_lower                numeric(7,6) NOT NULL CHECK (bucket_lower >= 0 AND bucket_lower <= 1),
    bucket_upper                numeric(7,6) NOT NULL CHECK (bucket_upper >= 0 AND bucket_upper <= 1),
    n_predictions               integer NOT NULL CHECK (n_predictions >= 0),
    mean_predicted              numeric(12,8),
    realized_frequency          numeric(12,8),
    brier_mean                  numeric(12,8),
    log_loss_mean               numeric(12,8),
    expected_return_bps_mean    integer,
    realized_return_bps_mean    integer,
    confidence_mean             numeric(12,8),
    abs_error_bps_mean          integer,
    computer_version            text NOT NULL,
    computed_at                 timestamptz NOT NULL DEFAULT now(),
    UNIQUE (strategy_version_id, mode, window_start, window_end, regime_label, bucket_lower, bucket_upper, computed_at),
    CHECK (bucket_lower < bucket_upper),
    CHECK (window_start < window_end)
);
CREATE INDEX calibration_snapshots_strategy_idx ON calibration_snapshots (strategy_version_id, mode, computed_at DESC);
CREATE TRIGGER calibration_snapshots_immutable BEFORE UPDATE OR DELETE ON calibration_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
ALTER TABLE agent_lifecycle_transitions ADD CONSTRAINT agent_lifecycle_transitions_calibration_fk
    FOREIGN KEY (calibration_snapshot_id) REFERENCES calibration_snapshots(id);

GRANT SELECT, INSERT, UPDATE ON tools TO cp_app;
GRANT SELECT, INSERT ON tool_invocations, model_calls, predictions, prediction_outcomes, calibration_snapshots TO cp_app;
GRANT SELECT ON tools, tool_invocations, model_calls, predictions, prediction_outcomes, calibration_snapshots TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: predictions and tool/model provenance are audit record referenced by intents
