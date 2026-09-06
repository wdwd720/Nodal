-- +goose Up
-- Asset registry. Identity is (chain, mint_address); symbol is display only (PART 32).
-- This is the first migration at or above db/migrate.ProtectedVersion: no destructive down migration.

CREATE TABLE assets (
    id                uuid PRIMARY KEY,
    chain             text NOT NULL,                       -- e.g. 'solana-mainnet', 'solana-devnet'
    mint_address      text NOT NULL,                       -- base58 mint; 'native' for the chain's native asset
    kind              text NOT NULL CHECK (kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022')),
    symbol            text NOT NULL,                       -- display only; never identity
    name              text NOT NULL,
    decimals          smallint NOT NULL CHECK (decimals BETWEEN 0 AND 18),
    is_stablecoin     boolean NOT NULL DEFAULT false,
    peg_currency      text,                                -- 'USD' for USD-pegged stablecoins
    risk_class        text NOT NULL CHECK (risk_class IN ('SETTLEMENT','MAJOR','STANDARD','SPECULATIVE','UNSUPPORTED')),
    status            text NOT NULL CHECK (status IN ('ACTIVE','CLOSE_ONLY','RESTRICTED','HALTED','DELISTING','DELISTED')),
    metadata_version  integer NOT NULL DEFAULT 1,
    policy_ref        text,
    token_extensions  jsonb NOT NULL DEFAULT '[]'::jsonb,  -- detected Token-2022 extensions; non-empty ⇒ signing rejects by default (PART 34)
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain, mint_address)
);
CREATE TRIGGER assets_updated_at BEFORE UPDATE ON assets FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE asset_status_transitions (
    id              uuid PRIMARY KEY,
    asset_id        uuid NOT NULL REFERENCES assets(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    policy_version  text,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX asset_status_transitions_asset_idx ON asset_status_transitions (asset_id, occurred_at);
CREATE TRIGGER asset_status_transitions_immutable BEFORE UPDATE OR DELETE ON asset_status_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON assets TO cp_app;
GRANT SELECT, INSERT ON asset_status_transitions TO cp_app;
GRANT SELECT ON assets, asset_status_transitions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: asset registry is referenced by ledger history and is never dropped by rollback
