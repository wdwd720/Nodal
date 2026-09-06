-- +goose Up
-- Price observations and asset policy history (PARTS 25, 26, 33).

CREATE TABLE asset_prices (
    id               uuid PRIMARY KEY,
    asset_id         uuid NOT NULL REFERENCES assets(id),
    quote_asset_id   uuid NOT NULL REFERENCES assets(id),
    mantissa         numeric(38,0) NOT NULL CHECK (mantissa >= 0),
    scale            integer NOT NULL CHECK (scale BETWEEN 0 AND 38),
    source           text NOT NULL,
    observed_at      timestamptz NOT NULL,                  -- provider/event time
    received_at      timestamptz NOT NULL DEFAULT now(),    -- our receipt time (knowledge time)
    raw_ref          text,
    UNIQUE (asset_id, quote_asset_id, source, observed_at)
);
CREATE INDEX asset_prices_latest_idx ON asset_prices (asset_id, quote_asset_id, received_at DESC);

-- Append-only policy history; the current policy is the latest row with effective_at <= now().
CREATE TABLE asset_policies (
    id                      uuid PRIMARY KEY,
    asset_id                uuid NOT NULL REFERENCES assets(id),
    status                  text NOT NULL CHECK (status IN ('ACTIVE','CLOSE_ONLY','RESTRICTED','HALTED','DELISTING','DELISTED')),
    collateral_factor_bps   integer NOT NULL CHECK (collateral_factor_bps BETWEEN 0 AND 10000),
    stablecoin_status       text CHECK (stablecoin_status IS NULL OR stablecoin_status IN ('NORMAL','DEGRADED','RESTRICTED','HALTED')),
    max_price_age_ms        integer NOT NULL CHECK (max_price_age_ms > 0),
    policy_version          text NOT NULL,
    effective_at            timestamptz NOT NULL,
    expires_at              timestamptz,
    created_by_actor_type   text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id     text NOT NULL,
    reason                  text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX asset_policies_current_idx ON asset_policies (asset_id, effective_at DESC);
CREATE TRIGGER asset_policies_immutable BEFORE UPDATE OR DELETE ON asset_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON asset_prices, asset_policies TO cp_app;
GRANT SELECT ON asset_prices, asset_policies TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: policy history is audit record
