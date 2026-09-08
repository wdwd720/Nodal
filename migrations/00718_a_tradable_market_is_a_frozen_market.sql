-- +goose Up
-- A tradable market's economics are frozen, by the schema and not only by the
-- service (F-53).
--
-- Two freezes guard PART XIII's "do not allow a creator to silently change
-- economics after buyers enter":
--
--   cp_native_market_curve_frozen   keys on native_markets.activated_at
--   cp_native_asset_economics_frozen keys on native_assets.economics_locked_at
--
-- and both return early when their column is NULL. Meanwhile what decides
-- whether a fill is accepted is a DIFFERENT column: cp_native_market_apply_fill
-- gates on `m.status IN ('ACTIVE','CLOSE_ONLY')`.
--
-- Nothing tied the two together. A row with status = 'ACTIVE' and activated_at
-- NULL would trade normally while virtual_credit_reserve, initial_asset_reserve
-- and both fee rates stayed mutable -- so the constant product k and the NM005
-- supply ceiling could be rewritten between fills, which is the exact harm the
-- freeze exists to prevent.
--
-- The application never produces that row. `nativemarket.SetStatus` sets
-- activated_at in the same UPDATE that sets ACTIVE, and `nativeasset.Activate`
-- sets economics_locked_at in the same UPDATE as its status, both with a
-- comment saying why. This is the same shape as F-49: an invariant that holds
-- because the service is careful, in a database that is supposed to hold it
-- whatever the caller is.
--
-- Stated so the expression can never evaluate to NULL, which is F-50's lesson:
-- `status` is NOT NULL, so the left operand is always true or false, and the
-- right is an IS NOT NULL test. A CHECK that can be NULL is a CHECK that
-- accepts.

ALTER TABLE native_markets
    ADD CONSTRAINT native_markets_tradable_is_frozen
    CHECK (status NOT IN ('ACTIVE','CLOSE_ONLY') OR activated_at IS NOT NULL);

ALTER TABLE native_assets
    ADD CONSTRAINT native_assets_tradable_is_frozen
    CHECK (status NOT IN ('ACTIVE','CLOSE_ONLY') OR economics_locked_at IS NOT NULL);

-- Existing rows are validated rather than grandfathered. Every market and asset
-- that reached ACTIVE through the service carries its timestamp, and one that
-- does not would be a market trading on economics somebody could still edit --
-- a finding, not an inconvenience.

-- +goose Down
SELECT 1; -- protected: reverting would let a live market's curve become editable again
