-- +goose Up
-- Capital envelopes (PART 24), asset reservations (PART 22), withdrawal holds (PART 27).

CREATE TABLE capital_envelopes (
    id                            uuid PRIMARY KEY,
    account_id                    uuid NOT NULL REFERENCES accounts(id),
    agent_id                      uuid NOT NULL,             -- FK to agents added in the agent-runtime migration range
    strategy_version_id           uuid NOT NULL,
    settlement_asset_id           uuid NOT NULL REFERENCES assets(id),
    allocation_usd_minor          bigint NOT NULL CHECK (allocation_usd_minor >= 0),
    available_usd_minor           bigint NOT NULL CHECK (available_usd_minor >= 0),
    reserved_usd_minor            bigint NOT NULL DEFAULT 0 CHECK (reserved_usd_minor >= 0),
    deployed_usd_minor            bigint NOT NULL DEFAULT 0 CHECK (deployed_usd_minor >= 0),
    realized_pnl_usd_minor        bigint NOT NULL DEFAULT 0,
    realized_loss_usd_minor       bigint NOT NULL DEFAULT 0 CHECK (realized_loss_usd_minor >= 0),
    current_drawdown_usd_minor    bigint NOT NULL DEFAULT 0 CHECK (current_drawdown_usd_minor >= 0),
    daily_loss_usd_minor          bigint NOT NULL DEFAULT 0 CHECK (daily_loss_usd_minor >= 0),
    daily_loss_reset_at           timestamptz NOT NULL,
    max_daily_loss_usd_minor      bigint NOT NULL CHECK (max_daily_loss_usd_minor >= 0),
    max_drawdown_usd_minor        bigint NOT NULL CHECK (max_drawdown_usd_minor >= 0),
    max_single_trade_usd_minor    bigint NOT NULL CHECK (max_single_trade_usd_minor >= 0),
    max_position_usd_minor        bigint NOT NULL CHECK (max_position_usd_minor >= 0),
    allowed_instruments           uuid[] NOT NULL DEFAULT '{}',
    allowed_asset_classes         text[] NOT NULL DEFAULT '{}',
    allowed_venues                text[] NOT NULL DEFAULT '{}',
    max_model_spend_usd_minor     bigint NOT NULL DEFAULT 0 CHECK (max_model_spend_usd_minor >= 0),
    max_data_spend_usd_minor      bigint NOT NULL DEFAULT 0 CHECK (max_data_spend_usd_minor >= 0),
    max_order_rate_per_hour       integer NOT NULL DEFAULT 0 CHECK (max_order_rate_per_hour >= 0),
    policy_version                text NOT NULL,
    status                        text NOT NULL CHECK (status IN ('DRAFT','ACTIVE','PAUSED','EXHAUSTED','EXPIRED','REVOKED')),
    effective_at                  timestamptz NOT NULL,
    expires_at                    timestamptz,
    version                       bigint NOT NULL DEFAULT 1,
    created_by_actor_type         text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id           text NOT NULL,
    created_at                    timestamptz NOT NULL DEFAULT now(),
    updated_at                    timestamptz NOT NULL DEFAULT now(),
    -- An agent can never end up controlling more than its allocation (PART 21: no agent can increase its own capital).
    CHECK (available_usd_minor + reserved_usd_minor + deployed_usd_minor <= allocation_usd_minor)
);
CREATE INDEX capital_envelopes_account_idx ON capital_envelopes (account_id);
CREATE INDEX capital_envelopes_agent_idx ON capital_envelopes (agent_id) WHERE status = 'ACTIVE';
CREATE TRIGGER capital_envelopes_updated_at BEFORE UPDATE ON capital_envelopes FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Audit of authority-field changes (allocation, limits, allowlists, policy, status, validity window).
CREATE TABLE capital_envelope_changes (
    id            uuid PRIMARY KEY,
    envelope_id   uuid NOT NULL REFERENCES capital_envelopes(id),
    changes       jsonb NOT NULL,                             -- {field: {from, to}}
    actor_type    text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id      text NOT NULL,
    reason        text NOT NULL,
    approval_id   uuid,
    occurred_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX capital_envelope_changes_env_idx ON capital_envelope_changes (envelope_id, occurred_at);
CREATE TRIGGER capital_envelope_changes_immutable BEFORE UPDATE OR DELETE ON capital_envelope_changes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- One row per (account, asset); locked FOR UPDATE to serialize reservations.
CREATE TABLE asset_reservation_totals (
    account_id  uuid NOT NULL REFERENCES accounts(id),
    asset_id    uuid NOT NULL REFERENCES assets(id),
    reserved    numeric(38,0) NOT NULL DEFAULT 0 CHECK (reserved >= 0),
    version     bigint NOT NULL DEFAULT 0,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, asset_id)
);

CREATE TABLE asset_reservations (
    id                   uuid PRIMARY KEY,
    account_id           uuid NOT NULL REFERENCES accounts(id),
    asset_id             uuid NOT NULL REFERENCES assets(id),
    envelope_id          uuid REFERENCES capital_envelopes(id),
    intent_id            uuid,
    locked_by_order_id   uuid,
    actor_type           text NOT NULL,
    actor_id             text NOT NULL,
    quantity             numeric(38,0) NOT NULL CHECK (quantity > 0),
    consumed_quantity    numeric(38,0) NOT NULL DEFAULT 0 CHECK (consumed_quantity >= 0 AND consumed_quantity <= quantity),
    usd_minor            bigint NOT NULL CHECK (usd_minor >= 0),
    consumed_usd_minor   bigint NOT NULL DEFAULT 0 CHECK (consumed_usd_minor >= 0 AND consumed_usd_minor <= usd_minor),
    status               text NOT NULL CHECK (status IN ('ACTIVE','CONSUMED','RELEASED','EXPIRED')),
    reason               text NOT NULL DEFAULT '',
    release_reason       text,
    idempotency_key      text NOT NULL UNIQUE,
    correlation_id       text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    expires_at           timestamptz NOT NULL,
    consumed_at          timestamptz,
    released_at          timestamptz
);
CREATE INDEX asset_reservations_active_idx ON asset_reservations (account_id, asset_id) WHERE status = 'ACTIVE';
CREATE INDEX asset_reservations_expiry_idx ON asset_reservations (expires_at) WHERE status = 'ACTIVE' AND locked_by_order_id IS NULL;
CREATE INDEX asset_reservations_intent_idx ON asset_reservations (intent_id) WHERE intent_id IS NOT NULL;

CREATE TABLE withdrawal_holds (
    id           uuid PRIMARY KEY,
    account_id   uuid NOT NULL REFERENCES accounts(id),
    asset_id     uuid NOT NULL REFERENCES assets(id),
    quantity     numeric(38,0) NOT NULL CHECK (quantity > 0),
    reason       text NOT NULL,
    deposit_id   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz,
    released_at  timestamptz,
    released_by  text
);
CREATE INDEX withdrawal_holds_active_idx ON withdrawal_holds (account_id, asset_id) WHERE released_at IS NULL;

GRANT SELECT, INSERT, UPDATE ON capital_envelopes, asset_reservation_totals, asset_reservations, withdrawal_holds TO cp_app;
GRANT SELECT, INSERT ON capital_envelope_changes TO cp_app;
GRANT SELECT ON capital_envelopes, capital_envelope_changes, asset_reservation_totals, asset_reservations, withdrawal_holds TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: reservation and envelope history is financial record
