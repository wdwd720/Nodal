-- +goose Up
-- A positive price is never printed as zero (F-193).
--
-- The marginal price of a native market is (V+R)·10^18 / Y, TRUNCATED, and it
-- is computed three times: by `internal/nativemarket.ratioScaled` when a fill is
-- priced, by the ordering in `discovery.go` when the markets page is sorted, and
-- by `cp_native_market_check_print` below, which re-derives every price from the
-- fill so that a print cannot say the market traded somewhere it did not. Those
-- three statements have to agree to the unit or the third refuses the first with
-- NM001.
--
-- They agreed, including on a wrong answer. A ratio smaller than one unit of
-- price scale truncates to ZERO, and zero is not a small price: it is "free",
-- and it was read as free by the price-impact ceiling, the slippage ceiling, the
-- circuit breaker and the portfolio mark. `nativemarket.MinSpotUnits` now stops
-- such a market from being opened at all, and `ratioScaled` reports the smallest
-- price the scale can express -- one unit -- rather than nothing. This migration
-- moves the database's copy of the arithmetic the same distance, so the three
-- statements still agree to the unit and the agreement is provable rather than
-- merely likely.
--
-- Nothing else in the function changes: same checks, same error codes, same
-- ERRCODE NM001, same trigger. The body is restated in full because PostgreSQL
-- has no way to replace part of one.
--
-- Prints already written are untouched. The trigger is BEFORE INSERT, existing
-- rows are never re-checked, and no market that could have produced a zero price
-- can exist on a deployment that has run this build's `nativemarket.Create`.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_native_market_price(p_num numeric, p_den numeric) RETURNS numeric
LANGUAGE sql IMMUTABLE STRICT AS $$
    -- div() is PostgreSQL's truncating integer division of numerics, which is
    -- what math/big's Quo does for non-negative operands. The GREATEST is the
    -- one departure from truncation and it applies only to a POSITIVE ratio:
    -- a numerator of zero is genuinely a price of zero and says so.
    SELECT CASE
             WHEN p_den <= 0 THEN 0::numeric
             WHEN p_num > 0 THEN GREATEST(div(p_num * (10::numeric ^ 18), p_den), 1::numeric)
             ELSE div(p_num * (10::numeric ^ 18), p_den)
           END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_native_market_price(numeric, numeric) IS
    'The marginal/effective price of a native market at price scale 18: truncating division, except that a positive ratio is never reported as zero. Mirrors internal/nativemarket.ratioScaled (F-193).';

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_native_market_check_print() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    f          native_market_fills%ROWTYPE;
    m          native_markets%ROWTYPE;
    r_before   numeric;
    y_before   numeric;
    want_spot_before   numeric;
    want_spot_after    numeric;
    want_effective     numeric;
    want_credit_volume numeric;
    want_asset_volume  numeric;
BEGIN
    SELECT * INTO f FROM native_market_fills WHERE id = NEW.fill_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_MARKET_PRINT_ORPHAN: no fill %', NEW.fill_id USING ERRCODE = 'NM001';
    END IF;
    SELECT * INTO m FROM native_markets WHERE id = f.market_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_MARKET_MISSING: %', f.market_id USING ERRCODE = 'NM001';
    END IF;

    IF NEW.market_id <> f.market_id OR NEW.seq <> f.seq OR NEW.side <> f.side OR NEW.asset_id <> m.asset_id THEN
        RAISE EXCEPTION 'NATIVE_MARKET_PRINT_MISMATCH: print for fill % names market %/seq %/side %/asset %, the fill says %/%/%/%',
            NEW.fill_id, NEW.market_id, NEW.seq, NEW.side, NEW.asset_id,
            f.market_id, f.seq, f.side, m.asset_id USING ERRCODE = 'NM001';
    END IF;

    IF f.side = 'BUY' THEN
        r_before := f.real_credit_reserve_after - f.credits_to_pool;
        y_before := f.asset_reserve_after + f.assets_out;
        want_effective     := cp_native_market_price(f.credits_in, f.assets_out);
        want_credit_volume := f.credits_in;
        want_asset_volume  := f.assets_out;
    ELSE
        r_before := f.real_credit_reserve_after + f.credits_to_pool;
        y_before := f.asset_reserve_after - f.assets_in;
        want_effective     := cp_native_market_price(f.credits_out, f.assets_in);
        want_credit_volume := f.credits_to_pool;
        want_asset_volume  := f.assets_in;
    END IF;

    want_spot_before := cp_native_market_price(m.virtual_credit_reserve + r_before, y_before);
    want_spot_after  := cp_native_market_price(m.virtual_credit_reserve + f.real_credit_reserve_after,
                                               f.asset_reserve_after);

    IF NEW.spot_price_before <> want_spot_before
    OR NEW.spot_price_after  <> want_spot_after
    OR NEW.effective_price   <> want_effective
    OR NEW.credit_volume     <> want_credit_volume
    OR NEW.asset_volume      <> want_asset_volume THEN
        RAISE EXCEPTION 'NATIVE_MARKET_PRINT_DISAGREES: print for fill % says (%, %, %, %, %) but the fill gives (%, %, %, %, %)',
            NEW.fill_id, NEW.spot_price_before, NEW.spot_price_after, NEW.effective_price,
            NEW.credit_volume, NEW.asset_volume,
            want_spot_before, want_spot_after, want_effective, want_credit_volume, want_asset_volume
            USING ERRCODE = 'NM001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose Down
SELECT 1; -- protected: a market's price history is financial history and the check that guards it is never dropped by rollback
