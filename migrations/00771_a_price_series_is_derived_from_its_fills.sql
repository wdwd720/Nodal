-- +goose Up
-- The native market's public price series (product goal §14 CHARTS, §13 ASSET DETAIL).
--
-- WHAT THIS IS. A chart needs a bounded, indexable series of prints: one row per fill, carrying the
-- prices that fill traded at and the volume it moved. Today the market publishes its post-trade spot
-- price into asset_prices (migration 00105) and the fill's own amounts into native_market_fills
-- (00712), and neither alone can answer "what were the open, high, low and close of this market
-- between 09:00 and 09:01": asset_prices holds no volume and does not know which fill produced a
-- row except through a text raw_ref, and native_market_fills holds no price at all.
--
-- WHY IT IS NOT A SECOND SOURCE OF TRUTH. Every number in a print is derivable from the fill it
-- names plus the market's frozen curve, and this migration derives them again in SQL before it
-- accepts the row. internal/nativemarket computes the prices in Go with exact big.Int arithmetic;
-- the trigger below recomputes the same three quotients from the fill's own amounts and refuses any
-- print that disagrees. So a print cannot say the market traded somewhere it did not, whoever
-- inserts it -- the same treatment migration 00712 gives market state, for the same reason.
--
-- WHY THE APPLICATION SUPPLIES printed_at RATHER THAN A TRIGGER READING now(). The instant a price
-- carries is a Point-in-Time Reality question, not a bookkeeping one. internal/nativemarket already
-- stamps its asset_prices row with an instant it computes (see reality.go: the deployment clock,
-- nudged forward to the smallest free microsecond so two prices on one market cannot collide), and
-- a print stamped with the database's now() would disagree with the asset_prices row describing the
-- same trade by whatever the transaction took. They are the same observation and they carry the same
-- instant.
--
-- Custom SQLSTATEs: NM001 (the print disagrees with its fill) is the one this file raises. It is
-- 00712's code for "this row claims arithmetic its own amounts do not support", which is exactly
-- what a wrong print is.

CREATE TABLE native_market_prints (
    fill_id              uuid PRIMARY KEY REFERENCES native_market_fills(id),
    market_id            uuid NOT NULL REFERENCES native_markets(id),
    asset_id             uuid NOT NULL REFERENCES native_assets(asset_id),
    seq                  bigint NOT NULL,
    side                 text NOT NULL CHECK (side IN ('BUY','SELL')),

    -- Prices are credit base units per asset base unit, scaled by 10^price_scale.
    -- nativemarket.PriceScale is 18 and is a compile-time constant; the CHECK holds the column to
    -- it so a future change has to arrive as a migration rather than as rows in two scales.
    price_scale          integer NOT NULL CHECK (price_scale = 18),
    spot_price_before    numeric(78,0) NOT NULL CHECK (spot_price_before >= 0),
    spot_price_after     numeric(78,0) NOT NULL CHECK (spot_price_after >= 0),
    effective_price      numeric(78,0) NOT NULL CHECK (effective_price >= 0),

    -- What moved. credit_volume is the GROSS Credit side of the trade -- what the buyer paid, or
    -- what the pool released before fees -- so a market's volume is the same number whichever side
    -- of it a trade was on.
    credit_volume        numeric(38,0) NOT NULL CHECK (credit_volume > 0),
    asset_volume         numeric(38,0) NOT NULL CHECK (asset_volume > 0),

    printed_at           timestamptz NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),

    UNIQUE (market_id, seq)
);
-- The candle query's access path: one market, a bounded time range, in order.
CREATE INDEX native_market_prints_series_idx ON native_market_prints (market_id, printed_at, seq);
-- The public tape ("recent prints"), newest first.
CREATE INDEX native_market_prints_recent_idx ON native_market_prints (market_id, printed_at DESC, seq DESC);

CREATE TRIGGER native_market_prints_immutable BEFORE UPDATE OR DELETE ON native_market_prints
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Re-derive every price from the fill rather than trusting the print.
--
-- The reserves before the trade are recoverable from the fill: it carries the reserves AFTER and the
-- amounts that moved, and 00712's apply trigger has already refused it unless those agree with the
-- market state it was applied to. So the check below is against the same numbers the invariant was
-- proved on.
--
-- div() is PostgreSQL's truncating integer division of numerics, which is what math/big's Quo does
-- for non-negative operands -- the operation internal/nativemarket.ratioScaled performs. Anything
-- else here would produce off-by-one disagreements on exactly the trades where the price matters.
-- +goose StatementBegin
CREATE FUNCTION cp_native_market_check_print() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    f          native_market_fills%ROWTYPE;
    m          native_markets%ROWTYPE;
    factor     numeric := 10::numeric ^ 18;
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
        want_effective     := div(f.credits_in * factor, f.assets_out);
        want_credit_volume := f.credits_in;
        want_asset_volume  := f.assets_out;
    ELSE
        r_before := f.real_credit_reserve_after + f.credits_to_pool;
        y_before := f.asset_reserve_after - f.assets_in;
        -- The seller's effective price is what they RECEIVED per unit, net of fees: that is the
        -- price they actually got, and the number the fill's own EffectivePrice carries.
        want_effective     := CASE WHEN f.credits_out = 0 THEN 0 ELSE div(f.credits_out * factor, f.assets_in) END;
        want_credit_volume := f.credits_to_pool;
        want_asset_volume  := f.assets_in;
    END IF;

    want_spot_before := div((m.virtual_credit_reserve + r_before) * factor, y_before);
    want_spot_after  := div((m.virtual_credit_reserve + f.real_credit_reserve_after) * factor, f.asset_reserve_after);

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
CREATE TRIGGER native_market_prints_check BEFORE INSERT ON native_market_prints
    FOR EACH ROW EXECUTE FUNCTION cp_native_market_check_print();

-- INSERT and SELECT only. A print that could be updated is a chart that could be rewritten.
GRANT SELECT, INSERT ON native_market_prints TO cp_app;
GRANT SELECT ON native_market_prints TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a market's price history is financial history and is never dropped by rollback
