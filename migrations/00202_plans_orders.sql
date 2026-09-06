-- +goose Up
-- Execution plans (PARTS 38-40), plan steps, orders (PART 47).

CREATE TABLE execution_plans (
    id                            uuid PRIMARY KEY,
    intent_id                     uuid NOT NULL REFERENCES trade_intents(id),
    version                       integer NOT NULL CHECK (version >= 1),
    planner_version               text NOT NULL,
    status                        text NOT NULL CHECK (status IN ('DRAFT','APPROVED','EXECUTING','COMPLETED','FAILED','SUPERSEDED','NO_VALID_PLAN')),
    no_plan_reason_codes          text[] NOT NULL DEFAULT '{}',
    hard_constraints              jsonb NOT NULL,
    estimated_costs               jsonb NOT NULL DEFAULT '{}'::jsonb,
    selected_venue_listing_id     uuid REFERENCES venue_listings(id),
    selected_settlement_asset_id  uuid REFERENCES assets(id),
    instrument_version            integer,
    policy_versions               jsonb NOT NULL DEFAULT '{}'::jsonb,
    risk_decision_id              uuid REFERENCES risk_decisions(id),
    quote_id                      uuid REFERENCES quotes(id),
    plan_hash                     bytea NOT NULL,
    dry_run                       boolean NOT NULL DEFAULT false,
    created_at                    timestamptz NOT NULL DEFAULT now(),
    approved_at                   timestamptz,
    finished_at                   timestamptz,
    UNIQUE (intent_id, version)
);
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_plan_fk FOREIGN KEY (plan_id) REFERENCES execution_plans(id);

-- +goose StatementBegin
-- Plans are immutable after approval except for lifecycle columns.
CREATE FUNCTION execution_plans_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'PLAN_IMMUTABLE: execution plans are never deleted' USING ERRCODE = 'LG003';
    END IF;
    IF OLD.approved_at IS NOT NULL AND (
        NEW.plan_hash IS DISTINCT FROM OLD.plan_hash OR
        NEW.hard_constraints IS DISTINCT FROM OLD.hard_constraints OR
        NEW.estimated_costs IS DISTINCT FROM OLD.estimated_costs OR
        NEW.selected_venue_listing_id IS DISTINCT FROM OLD.selected_venue_listing_id OR
        NEW.selected_settlement_asset_id IS DISTINCT FROM OLD.selected_settlement_asset_id OR
        NEW.policy_versions IS DISTINCT FROM OLD.policy_versions OR
        NEW.risk_decision_id IS DISTINCT FROM OLD.risk_decision_id OR
        NEW.quote_id IS DISTINCT FROM OLD.quote_id OR
        NEW.intent_id IS DISTINCT FROM OLD.intent_id OR
        NEW.version IS DISTINCT FROM OLD.version OR
        NEW.planner_version IS DISTINCT FROM OLD.planner_version OR
        NEW.dry_run IS DISTINCT FROM OLD.dry_run OR
        NEW.approved_at IS DISTINCT FROM OLD.approved_at) THEN
        RAISE EXCEPTION 'PLAN_IMMUTABLE: approved plan % cannot change; create a new version', OLD.id USING ERRCODE = 'LG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER execution_plans_guard BEFORE UPDATE OR DELETE ON execution_plans FOR EACH ROW EXECUTE FUNCTION execution_plans_guard();

CREATE TABLE execution_plan_steps (
    id                        uuid PRIMARY KEY,
    plan_id                   uuid NOT NULL REFERENCES execution_plans(id),
    seq                       integer NOT NULL CHECK (seq >= 0),
    type                      text NOT NULL CHECK (type IN (
                                  'VALIDATE_ELIGIBILITY','EVALUATE_RISK','RESERVE_CAPITAL','RESOLVE_VENUE_LISTING','LOCATE_SETTLEMENT_ASSET',
                                  'ACQUIRE_QUOTE','VALIDATE_QUOTE','FINAL_RISK_CHECK','BUILD_TRANSACTION','INSPECT_TRANSACTION',
                                  'REQUEST_SIGNATURE','SUBMIT','OBSERVE_FINALITY','RECONCILE','POST_LEDGER','UPDATE_POSITION','RELEASE_RESERVATION',
                                  'CONVERT','TRANSFER','WAIT_FINALITY','TRADE')),
    depends_on                uuid[] NOT NULL DEFAULT '{}',
    state                     text NOT NULL CHECK (state IN ('PENDING','RUNNING','SUCCEEDED','FAILED','SKIPPED','UNKNOWN','COMPENSATED')),
    semantic_idempotency_key  text NOT NULL UNIQUE,
    retry_class               text NOT NULL CHECK (retry_class IN ('SAFE_RETRY','IDEMPOTENT_WRITE','UNKNOWN_EFFECT_WRITE')),
    timeout_ms                integer NOT NULL CHECK (timeout_ms > 0),
    finality_policy           text NOT NULL,
    compensation_policy       text NOT NULL,
    evidence_inputs           jsonb NOT NULL DEFAULT '{}'::jsonb,
    evidence_output           jsonb,
    attempts                  integer NOT NULL DEFAULT 0,
    started_at                timestamptz,
    finished_at               timestamptz,
    last_error                text,
    UNIQUE (plan_id, seq)
);

CREATE TABLE orders (
    id                       uuid PRIMARY KEY,
    intent_id                uuid NOT NULL REFERENCES trade_intents(id),
    plan_id                  uuid NOT NULL REFERENCES execution_plans(id),
    account_id               uuid NOT NULL REFERENCES accounts(id),
    instrument_id            uuid NOT NULL REFERENCES instruments(id),
    venue_listing_id         uuid NOT NULL REFERENCES venue_listings(id),
    side                     text NOT NULL CHECK (side IN ('BUY','SELL')),
    mode                     text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    status                   text NOT NULL CHECK (status IN (
                                 'CREATED','VALIDATED','CAPITAL_RESERVED','PLANNED','SUBMITTING','SUBMITTED','ACKNOWLEDGED',
                                 'PARTIALLY_FILLED','FILLED','SETTLING','SETTLED',
                                 'REJECTED','EXPIRED','CANCEL_REQUESTED','CANCELLED','SUBMISSION_UNKNOWN','RECONCILIATION_REQUIRED','FAILED_FINAL')),
    input_asset_id           uuid NOT NULL REFERENCES assets(id),
    input_quantity           numeric(38,0) NOT NULL CHECK (input_quantity > 0),
    output_asset_id          uuid NOT NULL REFERENCES assets(id),
    min_output_quantity      numeric(38,0) NOT NULL CHECK (min_output_quantity >= 0),
    filled_input_quantity    numeric(38,0) NOT NULL DEFAULT 0 CHECK (filled_input_quantity >= 0 AND filled_input_quantity <= input_quantity),
    filled_output_quantity   numeric(38,0) NOT NULL DEFAULT 0 CHECK (filled_output_quantity >= 0),
    reservation_id           uuid NOT NULL REFERENCES asset_reservations(id),
    quote_id                 uuid NOT NULL REFERENCES quotes(id),
    rejection_code           text,
    correlation_id           text NOT NULL,
    terminal_at              timestamptz,
    created_at               timestamptz NOT NULL DEFAULT now(),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (intent_id)
);
ALTER TABLE trade_intents ADD CONSTRAINT trade_intents_order_fk FOREIGN KEY (order_id) REFERENCES orders(id);
CREATE INDEX orders_account_idx ON orders (account_id, created_at DESC);
CREATE INDEX orders_open_idx ON orders (status) WHERE status NOT IN ('SETTLED','REJECTED','EXPIRED','CANCELLED','FAILED_FINAL');
CREATE TRIGGER orders_updated_at BEFORE UPDATE ON orders FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE order_transitions (
    id            uuid PRIMARY KEY,
    order_id      uuid NOT NULL REFERENCES orders(id),
    from_status   text NOT NULL,
    to_status     text NOT NULL,
    actor_type    text NOT NULL,
    actor_id      text NOT NULL,
    reason        text,
    evidence_ref  text,
    occurred_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX order_transitions_order_idx ON order_transitions (order_id, occurred_at);
CREATE TRIGGER order_transitions_immutable BEFORE UPDATE OR DELETE ON order_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON execution_plans, execution_plan_steps, orders TO cp_app;
GRANT SELECT, INSERT ON order_transitions TO cp_app;
GRANT SELECT ON execution_plans, execution_plan_steps, orders, order_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: plans and orders are financial evidence
