-- +goose Up
-- The register of sandbox demo data (product goal §51 DEMO / STAGING DATA, §12 "do not show fake
-- market data in live staging without labeling it appropriately").
--
-- WHAT THIS IS. One row per object a demo seeder created, keyed by the seeder's own deterministic
-- key. It does two jobs and nothing else:
--
--   1. IDEMPOTENCE. The key is the primary key, so a second run of the seeder finds its own rows and
--      creates nothing. The seeder never needs to guess whether an asset called DEMO-something is
--      the one it made.
--   2. LABELLING. Everything the product shows about a native asset or market can be joined to this
--      table, so the API can mark a demo object as a demo object without the label depending on a
--      naming convention somebody could imitate.
--
-- WHAT IT IS NOT. It is not a route around any invariant. The seeder creates its assets, markets and
-- trades through the same domain services a person's request goes through -- content screening, the
-- moderation verdict, the mint, the ledger posting, the risk kernel, the constant-product trigger --
-- and then records what it made here. Nothing in this table can bring an object into existence.
--
-- A CHECK refuses a PROD row, the same shape migration 00755 uses for a SANDBOX gate: the seeder
-- refuses to run outside a sandbox tier in Go, and the database refuses to hold the evidence of a
-- run that somehow happened in PROD.

CREATE TABLE demo_seed_rows (
    seed_key      text PRIMARY KEY CHECK (seed_key <> ''),
    kind          text NOT NULL CHECK (kind IN ('ACCOUNT','NATIVE_ASSET','NATIVE_MARKET','NATIVE_TRADE')),
    ref_id        uuid NOT NULL,
    environment   text NOT NULL CHECK (environment <> 'PROD' AND environment <> ''),
    label         text NOT NULL DEFAULT '',
    seeded_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX demo_seed_rows_ref_idx ON demo_seed_rows (kind, ref_id);

CREATE TRIGGER demo_seed_rows_immutable BEFORE UPDATE OR DELETE ON demo_seed_rows
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON demo_seed_rows TO cp_app;
GRANT SELECT ON demo_seed_rows TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the label on a demo object is what keeps it distinguishable from real data
