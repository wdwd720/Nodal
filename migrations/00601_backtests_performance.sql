-- +goose Up
-- Backtests with the reproducibility manifest (PART 80), results with labelled assumptions (PART 83), point-in-time
-- validity (PART 82), counterfactual estimates (PART 84), performance snapshots keyed by mode (PARTS 81, 86, 159)
-- and cost accounting (PART 127). See docs/architecture/POINT_IN_TIME.md.
-- Custom SQLSTATEs: BT001 immutable backtest manifest field.

CREATE TABLE backtests (
    id                        uuid PRIMARY KEY,
    strategy_version_id       uuid NOT NULL REFERENCES strategy_versions(id),
    agent_id                  uuid REFERENCES agents(id),
    requested_by_user_id      uuid REFERENCES users(id),
    mode                      text NOT NULL DEFAULT 'BACKTEST' CHECK (mode = 'BACKTEST'),
    -- reproducibility manifest (frozen once run_at is set)
    ir_hash                   bytea NOT NULL CHECK (length(ir_hash) = 32),
    ir_schema_version         integer NOT NULL CHECK (ir_schema_version >= 1),
    simulator_version         text NOT NULL,
    evaluator_version         text NOT NULL,
    dataset_manifest          jsonb NOT NULL,                              -- [{source, stream, range, parts_hash, raw_object_count}]
    dataset_manifest_hash     bytea NOT NULL,
    data_cutoff_at            timestamptz NOT NULL,
    window_start              timestamptz NOT NULL,
    window_end                timestamptz NOT NULL,
    model_mode                text NOT NULL CHECK (model_mode IN ('NONE','REPLAY_RECORDED','FROZEN_SNAPSHOT','LIVE_MODEL_TEMPORALLY_IMPURE')),
    risk_policy_version       text NOT NULL,
    risk_policy_hash          bytea NOT NULL,
    fee_model_version         text NOT NULL,
    fee_model                 jsonb NOT NULL DEFAULT '{}'::jsonb,
    slippage_model_version    text NOT NULL,
    slippage_model            jsonb NOT NULL DEFAULT '{}'::jsonb,
    latency_model_version     text NOT NULL,
    latency_model             jsonb NOT NULL DEFAULT '{}'::jsonb,
    random_seed               bigint NOT NULL,                             -- always stored (PART 224)
    manifest_hash             bytea NOT NULL,
    -- lifecycle and outcome
    status                    text NOT NULL CHECK (status IN ('QUEUED','RUNNING','COMPLETED','FAILED','CANCELLED')),
    pit_validity              text CHECK (pit_validity IS NULL OR pit_validity IN ('POINT_IN_TIME_VALID','RESEARCH_ONLY_TEMPORALLY_IMPURE')),
    impurity_reasons          text[] NOT NULL DEFAULT '{}',
    leakage_test_passed       boolean,
    leakage_test_ref          text,
    result_hash               bytea,
    trace_ref                 text,                                        -- ClickHouse strategy_decisions / backtest_trades key
    error                     text,
    build_version             text,
    correlation_id            text,
    created_at                timestamptz NOT NULL DEFAULT now(),
    run_at                    timestamptz,
    finished_at               timestamptz,
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (window_start < window_end AND window_end <= data_cutoff_at),
    CHECK (status <> 'COMPLETED' OR (pit_validity IS NOT NULL AND leakage_test_passed IS NOT NULL AND result_hash IS NOT NULL AND finished_at IS NOT NULL)),
    -- POINT_IN_TIME_VALID is only possible when the leakage test passed, no live model was used and no impurity was recorded.
    CHECK (pit_validity IS DISTINCT FROM 'POINT_IN_TIME_VALID'
           OR (leakage_test_passed AND model_mode IN ('NONE','REPLAY_RECORDED') AND cardinality(impurity_reasons) = 0)),
    CHECK (pit_validity IS DISTINCT FROM 'RESEARCH_ONLY_TEMPORALLY_IMPURE' OR cardinality(impurity_reasons) > 0)
);
CREATE INDEX backtests_strategy_idx ON backtests (strategy_version_id, created_at DESC);
CREATE INDEX backtests_queue_idx ON backtests (status, created_at) WHERE status IN ('QUEUED','RUNNING');

-- +goose StatementBegin
CREATE FUNCTION backtests_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'BACKTEST_IMMUTABLE: backtests are never deleted' USING ERRCODE = 'BT001';
    END IF;
    IF NEW.mode IS DISTINCT FROM OLD.mode OR NEW.strategy_version_id IS DISTINCT FROM OLD.strategy_version_id THEN
        RAISE EXCEPTION 'BACKTEST_IMMUTABLE: mode and strategy version of % cannot change', OLD.id USING ERRCODE = 'BT001';
    END IF;
    IF OLD.run_at IS NOT NULL AND (
        NEW.ir_hash IS DISTINCT FROM OLD.ir_hash OR NEW.ir_schema_version IS DISTINCT FROM OLD.ir_schema_version
        OR NEW.simulator_version IS DISTINCT FROM OLD.simulator_version OR NEW.evaluator_version IS DISTINCT FROM OLD.evaluator_version
        OR NEW.dataset_manifest IS DISTINCT FROM OLD.dataset_manifest OR NEW.dataset_manifest_hash IS DISTINCT FROM OLD.dataset_manifest_hash
        OR NEW.data_cutoff_at IS DISTINCT FROM OLD.data_cutoff_at OR NEW.window_start IS DISTINCT FROM OLD.window_start
        OR NEW.window_end IS DISTINCT FROM OLD.window_end OR NEW.model_mode IS DISTINCT FROM OLD.model_mode
        OR NEW.risk_policy_version IS DISTINCT FROM OLD.risk_policy_version OR NEW.risk_policy_hash IS DISTINCT FROM OLD.risk_policy_hash
        OR NEW.fee_model_version IS DISTINCT FROM OLD.fee_model_version OR NEW.fee_model IS DISTINCT FROM OLD.fee_model
        OR NEW.slippage_model_version IS DISTINCT FROM OLD.slippage_model_version OR NEW.slippage_model IS DISTINCT FROM OLD.slippage_model
        OR NEW.latency_model_version IS DISTINCT FROM OLD.latency_model_version OR NEW.latency_model IS DISTINCT FROM OLD.latency_model
        OR NEW.random_seed IS DISTINCT FROM OLD.random_seed OR NEW.manifest_hash IS DISTINCT FROM OLD.manifest_hash
        OR NEW.run_at IS DISTINCT FROM OLD.run_at) THEN
        RAISE EXCEPTION 'BACKTEST_IMMUTABLE: manifest of % is frozen once run; create a new backtest', OLD.id USING ERRCODE = 'BT001';
    END IF;
    IF OLD.status = 'COMPLETED' AND (NEW.result_hash IS DISTINCT FROM OLD.result_hash OR NEW.pit_validity IS DISTINCT FROM OLD.pit_validity
        OR NEW.impurity_reasons IS DISTINCT FROM OLD.impurity_reasons OR NEW.leakage_test_passed IS DISTINCT FROM OLD.leakage_test_passed) THEN
        RAISE EXCEPTION 'BACKTEST_IMMUTABLE: completed backtest % cannot change its result or label', OLD.id USING ERRCODE = 'BT001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER backtests_guard BEFORE UPDATE OR DELETE ON backtests FOR EACH ROW EXECUTE FUNCTION backtests_guard();
CREATE TRIGGER backtests_updated_at BEFORE UPDATE ON backtests FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE agent_lifecycle_transitions ADD CONSTRAINT agent_lifecycle_transitions_backtest_fk FOREIGN KEY (backtest_id) REFERENCES backtests(id);
ALTER TABLE tool_invocations ADD CONSTRAINT tool_invocations_backtest_fk FOREIGN KEY (backtest_id) REFERENCES backtests(id);

-- Summary result per backtest. Per-decision traces live in ClickHouse. Immutable.
CREATE TABLE backtest_results (
    id                         uuid PRIMARY KEY,
    backtest_id                uuid NOT NULL UNIQUE REFERENCES backtests(id),
    mode                       text NOT NULL DEFAULT 'BACKTEST' CHECK (mode = 'BACKTEST'),
    n_runs                     integer NOT NULL CHECK (n_runs >= 0),
    n_predictions              integer NOT NULL DEFAULT 0 CHECK (n_predictions >= 0),
    n_intents                  integer NOT NULL DEFAULT 0 CHECK (n_intents >= 0),
    n_trades                   integer NOT NULL DEFAULT 0 CHECK (n_trades >= 0),
    n_skipped_stale            integer NOT NULL DEFAULT 0 CHECK (n_skipped_stale >= 0),
    n_skipped_missing          integer NOT NULL DEFAULT 0 CHECK (n_skipped_missing >= 0),
    realized_return_bps        integer NOT NULL,
    max_drawdown_bps           integer NOT NULL CHECK (max_drawdown_bps >= 0),
    sharpe                     numeric(12,6),
    sortino                    numeric(12,6),
    sharpe_meaningful          boolean NOT NULL DEFAULT false,
    turnover_bps               bigint NOT NULL DEFAULT 0 CHECK (turnover_bps >= 0),
    hit_rate_bps               integer CHECK (hit_rate_bps IS NULL OR hit_rate_bps BETWEEN 0 AND 10000),
    avg_slippage_bps           integer,
    execution_quality_bps      integer,
    brier_mean                 numeric(12,8),
    log_loss_mean              numeric(12,8),
    gross_pnl_usd_minor        bigint NOT NULL,
    execution_fees_usd_minor   bigint NOT NULL DEFAULT 0 CHECK (execution_fees_usd_minor >= 0),
    network_fees_usd_minor     bigint NOT NULL DEFAULT 0 CHECK (network_fees_usd_minor >= 0),
    model_cost_usd_minor       bigint NOT NULL DEFAULT 0 CHECK (model_cost_usd_minor >= 0),
    data_cost_usd_minor        bigint NOT NULL DEFAULT 0 CHECK (data_cost_usd_minor >= 0),
    compute_cost_usd_minor     bigint NOT NULL DEFAULT 0 CHECK (compute_cost_usd_minor >= 0),
    net_pnl_usd_minor          bigint NOT NULL,
    assumptions                jsonb NOT NULL DEFAULT '[]'::jsonb,        -- [{name, model_version, source: RECORDED|MODELLED|UNAVAILABLE}]
    metrics                    jsonb NOT NULL DEFAULT '{}'::jsonb,
    result_hash                bytea NOT NULL,
    computed_at                timestamptz NOT NULL,
    created_at                 timestamptz NOT NULL DEFAULT now(),
    CHECK (sharpe_meaningful = false OR sharpe IS NOT NULL)
);
CREATE TRIGGER backtest_results_immutable BEFORE UPDATE OR DELETE ON backtest_results
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Shadow branches (PART 84). Always estimates; never part of realized performance.
CREATE TABLE counterfactuals (
    id                        uuid PRIMARY KEY,
    subject_kind              text NOT NULL CHECK (subject_kind IN ('AGENT_RUN','INTENT','BACKTEST_DECISION')),
    agent_run_id              uuid REFERENCES agent_runs(id),
    intent_id                 uuid REFERENCES trade_intents(id),
    backtest_id               uuid REFERENCES backtests(id),
    backtest_decision_ref     text,
    strategy_version_id       uuid NOT NULL REFERENCES strategy_versions(id),
    mode                      text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    branch                    text NOT NULL CHECK (branch IN ('NO_TRADE','ENTRY_PLUS_5S','ENTRY_PLUS_30S','HALF_SIZE','DOUBLE_SIZE','ALTERNATIVE_ROUTE')),
    is_estimate               boolean NOT NULL DEFAULT true CHECK (is_estimate),
    estimated_pnl_usd_minor   bigint NOT NULL,
    estimated_return_bps      integer NOT NULL,
    estimate_basis            jsonb NOT NULL,
    assumptions               jsonb NOT NULL DEFAULT '[]'::jsonb,
    simulator_version         text NOT NULL,
    computed_at               timestamptz NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (
        (subject_kind = 'AGENT_RUN' AND agent_run_id IS NOT NULL) OR
        (subject_kind = 'INTENT' AND intent_id IS NOT NULL) OR
        (subject_kind = 'BACKTEST_DECISION' AND backtest_id IS NOT NULL AND backtest_decision_ref IS NOT NULL)
    )
);
CREATE INDEX counterfactuals_run_idx ON counterfactuals (agent_run_id) WHERE agent_run_id IS NOT NULL;
CREATE INDEX counterfactuals_backtest_idx ON counterfactuals (backtest_id) WHERE backtest_id IS NOT NULL;
CREATE TRIGGER counterfactuals_immutable BEFORE UPDATE OR DELETE ON counterfactuals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Performance snapshots (PART 86). Mode is part of the identity; no cross-mode aggregation exists (PART 81, 159).
CREATE TABLE performance_snapshots (
    id                            uuid PRIMARY KEY,
    scope_kind                    text NOT NULL CHECK (scope_kind IN ('STRATEGY_VERSION','AGENT','ACCOUNT','ENVELOPE')),
    scope_id                      uuid NOT NULL,
    strategy_version_id           uuid REFERENCES strategy_versions(id),
    agent_id                      uuid REFERENCES agents(id),
    account_id                    uuid REFERENCES accounts(id),
    envelope_id                   uuid REFERENCES capital_envelopes(id),
    mode                          text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    window_start                  timestamptz NOT NULL,
    window_end                    timestamptz NOT NULL,
    as_of                         timestamptz NOT NULL,
    realized_return_bps           integer NOT NULL,
    unrealized_return_bps         integer NOT NULL,
    realized_pnl_usd_minor        bigint NOT NULL,
    unrealized_pnl_usd_minor      bigint NOT NULL,
    max_drawdown_bps              integer NOT NULL CHECK (max_drawdown_bps >= 0),
    current_drawdown_bps          integer NOT NULL CHECK (current_drawdown_bps >= 0),
    sharpe                        numeric(12,6),
    sortino                       numeric(12,6),
    sharpe_meaningful             boolean NOT NULL DEFAULT false,
    n_observations                integer NOT NULL CHECK (n_observations >= 0),
    turnover_bps                  bigint NOT NULL DEFAULT 0 CHECK (turnover_bps >= 0),
    hit_rate_bps                  integer CHECK (hit_rate_bps IS NULL OR hit_rate_bps BETWEEN 0 AND 10000),
    n_trades                      integer NOT NULL DEFAULT 0 CHECK (n_trades >= 0),
    n_predictions                 integer NOT NULL DEFAULT 0 CHECK (n_predictions >= 0),
    avg_slippage_bps              integer,
    execution_quality_bps         integer,
    brier_mean                    numeric(12,8),
    log_loss_mean                 numeric(12,8),
    strategy_age_seconds          bigint NOT NULL CHECK (strategy_age_seconds >= 0),
    live_capital_history          jsonb NOT NULL DEFAULT '[]'::jsonb,      -- [{at, allocation_usd_minor, deployed_usd_minor}]
    model_cost_usd_minor          bigint NOT NULL DEFAULT 0 CHECK (model_cost_usd_minor >= 0),
    data_cost_usd_minor           bigint NOT NULL DEFAULT 0 CHECK (data_cost_usd_minor >= 0),
    compute_cost_usd_minor        bigint NOT NULL DEFAULT 0 CHECK (compute_cost_usd_minor >= 0),
    execution_fees_usd_minor      bigint NOT NULL DEFAULT 0 CHECK (execution_fees_usd_minor >= 0),
    network_fees_usd_minor        bigint NOT NULL DEFAULT 0 CHECK (network_fees_usd_minor >= 0),
    net_economic_alpha_usd_minor  bigint NOT NULL,
    valuation_policy_version      text,
    computer_version              text NOT NULL,
    input_hash                    bytea,
    snapshot_hash                 bytea NOT NULL,
    computed_at                   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (scope_kind, scope_id, mode, window_start, window_end, as_of),
    CHECK (window_start <= window_end AND window_end <= as_of),
    CHECK (sharpe_meaningful = false OR sharpe IS NOT NULL),
    CHECK (
        (scope_kind = 'STRATEGY_VERSION' AND strategy_version_id = scope_id) OR
        (scope_kind = 'AGENT' AND agent_id = scope_id) OR
        (scope_kind = 'ACCOUNT' AND account_id = scope_id) OR
        (scope_kind = 'ENVELOPE' AND envelope_id = scope_id)
    )
);
CREATE INDEX performance_snapshots_scope_idx ON performance_snapshots (scope_kind, scope_id, mode, as_of DESC);
CREATE TRIGGER performance_snapshots_immutable BEFORE UPDATE OR DELETE ON performance_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
ALTER TABLE agent_lifecycle_transitions ADD CONSTRAINT agent_lifecycle_transitions_performance_fk
    FOREIGN KEY (performance_snapshot_id) REFERENCES performance_snapshots(id);

-- Cost accounting (PART 127): model, data, execution fee, network fee and compute cost per strategy/agent/account/mode.
-- Written at the source of each cost; the unique key prevents double counting.
CREATE TABLE cost_accounting (
    id                    uuid PRIMARY KEY,
    kind                  text NOT NULL CHECK (kind IN ('MODEL','DATA','EXECUTION_FEE','NETWORK_FEE','COMPUTE')),
    strategy_version_id   uuid REFERENCES strategy_versions(id),
    agent_id              uuid REFERENCES agents(id),
    account_id            uuid REFERENCES accounts(id),
    backtest_id           uuid REFERENCES backtests(id),
    mode                  text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    reference_type        text NOT NULL,                                  -- 'tool_invocation' | 'model_call' | 'fill' | 'backtest' | 'agent_run'
    reference_id          text NOT NULL,
    amount_usd_minor      bigint NOT NULL CHECK (amount_usd_minor >= 0),
    asset_id              uuid REFERENCES assets(id),
    quantity              numeric(38,0) CHECK (quantity IS NULL OR quantity >= 0),
    price_ref             text,
    provider              text,
    occurred_at           timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, reference_type, reference_id),
    CHECK (strategy_version_id IS NOT NULL OR agent_id IS NOT NULL OR account_id IS NOT NULL OR backtest_id IS NOT NULL)
);
CREATE INDEX cost_accounting_agent_idx ON cost_accounting (agent_id, mode, occurred_at) WHERE agent_id IS NOT NULL;
CREATE INDEX cost_accounting_strategy_idx ON cost_accounting (strategy_version_id, mode, occurred_at) WHERE strategy_version_id IS NOT NULL;
CREATE INDEX cost_accounting_account_idx ON cost_accounting (account_id, mode, occurred_at) WHERE account_id IS NOT NULL;
CREATE TRIGGER cost_accounting_immutable BEFORE UPDATE OR DELETE ON cost_accounting
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON backtests TO cp_app;
GRANT SELECT, INSERT ON backtest_results, counterfactuals, performance_snapshots, cost_accounting TO cp_app;
GRANT SELECT ON backtests, backtest_results, counterfactuals, performance_snapshots, cost_accounting TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: backtest manifests, performance snapshots and cost rows are promotion evidence and financial record
