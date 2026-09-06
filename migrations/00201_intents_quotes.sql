-- +goose Up
-- Universal financial intent (PART 35), quotes (PART 42), performance mode stored on every intent (PART 159).

CREATE TABLE trade_intents (
    id                          uuid PRIMARY KEY,
    account_id                  uuid NOT NULL REFERENCES accounts(id),
    actor_type                  text NOT NULL CHECK (actor_type IN ('USER','AGENT','OPERATOR')),
    actor_id                    text NOT NULL,
    agent_id                    uuid,
    strategy_version_id         uuid,
    prediction_id               uuid,
    action                      text NOT NULL CHECK (action IN ('ACQUIRE_NOTIONAL','REDUCE_NOTIONAL','CLOSE_POSITION','TARGET_EXPOSURE')),
    instrument_id               uuid NOT NULL REFERENCES instruments(id),
    notional_usd_minor          bigint CHECK (notional_usd_minor IS NULL OR notional_usd_minor > 0),
    target_exposure_usd_minor   bigint CHECK (target_exposure_usd_minor IS NULL OR target_exposure_usd_minor >= 0),
    quantity                    numeric(38,0) CHECK (quantity IS NULL OR quantity > 0),
    constraints                 jsonb NOT NULL,
    deadline                    timestamptz,
    requested_at                timestamptz NOT NULL,
    received_at                 timestamptz NOT NULL DEFAULT now(),
    idempotency_key             text NOT NULL,
    correlation_id              text NOT NULL,
    mode                        text NOT NULL CHECK (mode IN ('BACKTEST','PAPER','SHADOW','CANARY','LIMITED','LIVE')),
    status                      text NOT NULL CHECK (status IN (
                                    'RECEIVED','ELIGIBILITY_CHECKED','RISK_CHECKED','RESERVED','PLANNED','EXECUTING',
                                    'COMPLETED','REJECTED','EXPIRED','CANCELLED','FAILED','NO_VALID_PLAN')),
    rejection_code              text,
    eligibility_decision_id     uuid REFERENCES eligibility_decisions(id),
    risk_decision_id            uuid REFERENCES risk_decisions(id),
    reservation_id              uuid REFERENCES asset_reservations(id),
    plan_id                     uuid,
    order_id                    uuid,
    terminal_at                 timestamptz,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key),
    CHECK (
        (action = 'ACQUIRE_NOTIONAL' AND notional_usd_minor IS NOT NULL) OR
        (action = 'REDUCE_NOTIONAL' AND (notional_usd_minor IS NOT NULL OR quantity IS NOT NULL)) OR
        (action = 'CLOSE_POSITION') OR
        (action = 'TARGET_EXPOSURE' AND target_exposure_usd_minor IS NOT NULL)
    )
);
CREATE INDEX trade_intents_account_idx ON trade_intents (account_id, requested_at DESC);
CREATE INDEX trade_intents_agent_idx ON trade_intents (agent_id, requested_at DESC) WHERE agent_id IS NOT NULL;
CREATE INDEX trade_intents_open_idx ON trade_intents (status) WHERE status NOT IN ('COMPLETED','REJECTED','EXPIRED','CANCELLED','FAILED','NO_VALID_PLAN');
CREATE TRIGGER trade_intents_updated_at BEFORE UPDATE ON trade_intents FOR EACH ROW EXECUTE FUNCTION set_updated_at();

ALTER TABLE eligibility_decisions ADD CONSTRAINT eligibility_decisions_intent_fk FOREIGN KEY (intent_id) REFERENCES trade_intents(id);
ALTER TABLE risk_decisions ADD CONSTRAINT risk_decisions_intent_fk FOREIGN KEY (intent_id) REFERENCES trade_intents(id);
ALTER TABLE asset_reservations ADD CONSTRAINT asset_reservations_intent_fk FOREIGN KEY (intent_id) REFERENCES trade_intents(id);

CREATE TABLE intent_transitions (
    id              uuid PRIMARY KEY,
    intent_id       uuid NOT NULL REFERENCES trade_intents(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text,
    evidence_ref    text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX intent_transitions_intent_idx ON intent_transitions (intent_id, occurred_at);
CREATE TRIGGER intent_transitions_immutable BEFORE UPDATE OR DELETE ON intent_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE quotes (
    id                        uuid PRIMARY KEY,
    intent_id                 uuid REFERENCES trade_intents(id),
    provider                  text NOT NULL,
    provider_request_id       text,
    instrument_id             uuid NOT NULL REFERENCES instruments(id),
    venue_listing_id          uuid NOT NULL REFERENCES venue_listings(id),
    side                      text NOT NULL CHECK (side IN ('BUY','SELL')),
    input_asset_id            uuid NOT NULL REFERENCES assets(id),
    input_quantity            numeric(38,0) NOT NULL CHECK (input_quantity > 0),
    output_asset_id           uuid NOT NULL REFERENCES assets(id),
    expected_output           numeric(38,0) NOT NULL CHECK (expected_output >= 0),
    minimum_output            numeric(38,0) NOT NULL CHECK (minimum_output >= 0 AND minimum_output <= expected_output),
    effective_price_mantissa  numeric(38,0) NOT NULL,
    effective_price_scale     integer NOT NULL CHECK (effective_price_scale BETWEEN 0 AND 38),
    price_impact_bps          integer NOT NULL CHECK (price_impact_bps >= 0),
    slippage_bps              integer NOT NULL CHECK (slippage_bps >= 0),
    est_network_cost          numeric(38,0) NOT NULL DEFAULT 0,
    est_network_cost_asset_id uuid REFERENCES assets(id),
    est_venue_fee             numeric(38,0) NOT NULL DEFAULT 0,
    est_venue_fee_asset_id    uuid REFERENCES assets(id),
    platform_fee              numeric(38,0) NOT NULL DEFAULT 0,
    platform_fee_asset_id     uuid REFERENCES assets(id),
    platform_fee_bps          integer NOT NULL DEFAULT 0 CHECK (platform_fee_bps >= 0),
    fee_policy_version        text,
    received_at               timestamptz NOT NULL,
    expires_at                timestamptz NOT NULL,
    route_hash                bytea NOT NULL,
    route_summary             jsonb NOT NULL DEFAULT '[]'::jsonb,
    raw_response_ref          text,
    raw_response_hash         bytea NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX quotes_intent_idx ON quotes (intent_id) WHERE intent_id IS NOT NULL;
CREATE INDEX quotes_received_idx ON quotes (received_at);
CREATE TRIGGER quotes_immutable BEFORE UPDATE OR DELETE ON quotes FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON trade_intents TO cp_app;
GRANT SELECT, INSERT ON intent_transitions, quotes TO cp_app;
GRANT SELECT ON trade_intents, intent_transitions, quotes TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: intents and quotes are financial evidence
