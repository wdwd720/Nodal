-- +goose Up
-- Native positions and realised P&L (product goal §15 PORTFOLIO).
--
-- WHAT THIS IS. Per (account, native asset): how many units the account holds, what they cost in
-- Credits, what it has realised by selling, and what it has paid in fees. The portfolio page needs
-- all four and there was nowhere to read them: position_lots (migration 00104) is the hosted rail's
-- tax-quality lot ledger and holds its basis in `bigint` USD MINOR UNITS, which a Credit-denominated
-- native position cannot be expressed in without inventing an exchange rate for a Credit -- the one
-- thing the value-domain rules forbid. So this is a second read model for a second denomination,
-- deliberately, and it never touches position_lots.
--
-- WHY IT IS A DERIVED READ MODEL AND NOT A SECOND BOOK. Two units of every unit are already
-- authoritative elsewhere: the ledger's NATIVE_ASSET_BALANCE projection says what an account holds,
-- and native_market_fills says what it traded. This table restates them in the shape a portfolio
-- needs, and three things stop it from drifting:
--
--   1. Only a trigger writes it. cp_app has SELECT and nothing else, exactly as for
--      native_market_state (00712). There is no application path that can set a cost basis.
--   2. A table CHECK states the invariant in SQL:
--          quantity = allocation_units + units_bought_total - units_sold_total
--      "the quantity equals the sum of its fills", enforced on every write rather than asserted in
--      a comment. There are exactly two ways a customer acquires a native asset unit -- the creator
--      allocation minted at market creation, and a market fill -- so this sum is complete.
--   3. cp_native_positions_unreconciled() compares every row against the ledger balance projection,
--      which is the independent source. It returns the disagreements; it never corrects one.
--
-- COST BASIS IS AVERAGE, NOT FIFO. The hosted rail uses FIFO lots because a tax report needs lots.
-- A native asset is closed-loop, non-redeemable internal value with no tax event and no external
-- price, so what a holder needs is "what did my position cost me" -- one number that cannot be
-- gamed by the order in which sells are reported. Average cost also makes the invariant above a
-- single arithmetic identity rather than a join across lots. The full trade history remains in
-- native_market_fills for anything that needs the lots.
--
-- Custom SQLSTATEs raised here: NM005 (a sell that the position cannot support -- 00712's supply
-- conservation code, which is what that is).

CREATE TABLE native_positions (
    account_id            uuid NOT NULL REFERENCES accounts(id),
    asset_id              uuid NOT NULL REFERENCES native_assets(asset_id),

    -- What the account holds now.
    quantity              numeric(38,0) NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    -- What the open quantity cost, in Credit base units, INCLUDING the fees paid to acquire it.
    -- A basis that excluded fees would make a round trip at an unchanged price look like a profit.
    cost_basis_credits    numeric(38,0) NOT NULL DEFAULT 0 CHECK (cost_basis_credits >= 0),
    -- Proceeds less basis on everything sold. Signed: a loss is a real outcome, not an error.
    realized_pnl_credits  numeric(38,0) NOT NULL DEFAULT 0,
    -- Platform + creator fees this account has paid on this asset, both sides. Disclosure only:
    -- the buy-side fee is already inside cost_basis_credits and the sell-side fee is already out of
    -- the proceeds, so adding this to either would count it twice.
    fees_paid_credits     numeric(38,0) NOT NULL DEFAULT 0 CHECK (fees_paid_credits >= 0),

    -- The sums the invariant is stated over.
    allocation_units      numeric(38,0) NOT NULL DEFAULT 0 CHECK (allocation_units >= 0),
    units_bought_total    numeric(38,0) NOT NULL DEFAULT 0 CHECK (units_bought_total >= 0),
    units_sold_total      numeric(38,0) NOT NULL DEFAULT 0 CHECK (units_sold_total >= 0),
    credits_in_total      numeric(38,0) NOT NULL DEFAULT 0 CHECK (credits_in_total >= 0),
    credits_out_total     numeric(38,0) NOT NULL DEFAULT 0 CHECK (credits_out_total >= 0),
    fill_count            bigint NOT NULL DEFAULT 0 CHECK (fill_count >= 0),

    first_acquired_at     timestamptz,
    last_trade_at         timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (account_id, asset_id),

    -- The invariant, in SQL.
    CONSTRAINT native_positions_quantity_is_the_sum_of_its_fills
        CHECK (quantity = allocation_units + units_bought_total - units_sold_total),
    -- A closed position carries no basis. Without this, rounding in the disposal arithmetic could
    -- leave a few base units of cost attached to nothing.
    CONSTRAINT native_positions_closed_position_has_no_basis
        CHECK (quantity > 0 OR cost_basis_credits = 0)
);
CREATE INDEX native_positions_account_idx ON native_positions (account_id) WHERE quantity > 0;
CREATE INDEX native_positions_asset_idx ON native_positions (asset_id) WHERE quantity > 0;

-- The creator's allocation: minted at market creation, outside the curve, at zero cost.
--
-- It is part of the position because it is part of what the account HOLDS, and a portfolio that
-- omitted it would disagree with the ledger. Its basis is zero because nothing was paid for it;
-- showing it at a made-up basis would manufacture a gain or a loss out of a grant.
-- +goose StatementBegin
CREATE FUNCTION cp_native_position_allocate() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    a native_assets%ROWTYPE;
BEGIN
    SELECT * INTO a FROM native_assets WHERE asset_id = NEW.asset_id;
    IF NOT FOUND OR a.creator_allocation = 0 THEN
        RETURN NEW;
    END IF;
    INSERT INTO native_positions AS p
        (account_id, asset_id, quantity, allocation_units, first_acquired_at, updated_at)
    VALUES (a.creator_account_id, NEW.asset_id, a.creator_allocation, a.creator_allocation, now(), now())
    ON CONFLICT (account_id, asset_id) DO UPDATE
       SET quantity         = p.quantity + EXCLUDED.allocation_units,
           allocation_units = p.allocation_units + EXCLUDED.allocation_units,
           first_acquired_at = coalesce(p.first_acquired_at, EXCLUDED.first_acquired_at),
           updated_at       = now();
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_markets_allocate_position AFTER INSERT ON native_markets
    FOR EACH ROW EXECUTE FUNCTION cp_native_position_allocate();

-- Every fill moves the position, and only a fill does.
--
-- The trigger name sorts after native_market_fills_apply, so 00712's invariant check has already
-- accepted the fill by the time this runs: a fill that breaks the constant product never reaches a
-- position.
-- +goose StatementBegin
CREATE FUNCTION cp_native_position_apply_fill() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public AS $$
DECLARE
    m            native_markets%ROWTYPE;
    pos          native_positions%ROWTYPE;
    fees         numeric;
    cost_removed numeric;
BEGIN
    SELECT * INTO m FROM native_markets WHERE id = NEW.market_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_MARKET_MISSING: %', NEW.market_id USING ERRCODE = 'NM004';
    END IF;
    fees := NEW.platform_fee + NEW.creator_fee;

    IF NEW.side = 'BUY' THEN
        INSERT INTO native_positions AS p
            (account_id, asset_id, quantity, cost_basis_credits, fees_paid_credits,
             units_bought_total, credits_in_total, fill_count, first_acquired_at, last_trade_at, updated_at)
        VALUES (NEW.account_id, m.asset_id, NEW.assets_out, NEW.credits_in, fees,
                NEW.assets_out, NEW.credits_in, 1, NEW.created_at, NEW.created_at, now())
        ON CONFLICT (account_id, asset_id) DO UPDATE
           SET quantity           = p.quantity + EXCLUDED.quantity,
               cost_basis_credits = p.cost_basis_credits + EXCLUDED.cost_basis_credits,
               fees_paid_credits  = p.fees_paid_credits + EXCLUDED.fees_paid_credits,
               units_bought_total = p.units_bought_total + EXCLUDED.units_bought_total,
               credits_in_total   = p.credits_in_total + EXCLUDED.credits_in_total,
               fill_count         = p.fill_count + 1,
               first_acquired_at  = coalesce(p.first_acquired_at, EXCLUDED.first_acquired_at),
               last_trade_at      = EXCLUDED.last_trade_at,
               updated_at         = now();
        RETURN NEW;
    END IF;

    SELECT * INTO pos FROM native_positions
     WHERE account_id = NEW.account_id AND asset_id = m.asset_id FOR UPDATE;
    IF NOT FOUND OR pos.quantity < NEW.assets_in THEN
        -- The ledger's negative-balance guard should already have refused this, so reaching here
        -- means the position read model and the ledger disagree. Refusing is the only safe answer:
        -- writing a negative quantity would put the disagreement inside the read model where the
        -- reconciliation query could no longer see it.
        RAISE EXCEPTION 'NATIVE_POSITION_SHORT: account % holds % of asset % and cannot deliver %',
            NEW.account_id, coalesce(pos.quantity, 0), m.asset_id, NEW.assets_in USING ERRCODE = 'NM005';
    END IF;

    -- Average cost. A full exit removes the whole basis exactly; a partial exit removes the
    -- truncated share, so the basis left behind is never short of the units left behind.
    IF NEW.assets_in = pos.quantity THEN
        cost_removed := pos.cost_basis_credits;
    ELSE
        cost_removed := div(pos.cost_basis_credits * NEW.assets_in, pos.quantity);
    END IF;

    UPDATE native_positions
       SET quantity             = quantity - NEW.assets_in,
           cost_basis_credits   = cost_basis_credits - cost_removed,
           realized_pnl_credits = realized_pnl_credits + (NEW.credits_out - cost_removed),
           fees_paid_credits    = fees_paid_credits + fees,
           units_sold_total     = units_sold_total + NEW.assets_in,
           credits_out_total    = credits_out_total + NEW.credits_out,
           fill_count           = fill_count + 1,
           last_trade_at        = NEW.created_at,
           updated_at           = now()
     WHERE account_id = NEW.account_id AND asset_id = m.asset_id;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER native_market_fills_position AFTER INSERT ON native_market_fills
    FOR EACH ROW EXECUTE FUNCTION cp_native_position_apply_fill();

-- The independent check. It reads the ledger's own balance projection -- the source neither this
-- table nor its triggers write -- and returns every position that disagrees with it. An empty
-- result is the proof; a non-empty one is a reconciliation incident and is never repaired by
-- writing one side to match the other.
-- +goose StatementBegin
CREATE FUNCTION cp_native_positions_unreconciled()
RETURNS TABLE (account_id uuid, asset_id uuid, position_quantity numeric, ledger_balance numeric)
LANGUAGE sql STABLE SET search_path = public AS $$
    SELECT COALESCE(p.account_id, la.owner_id) AS account_id,
           COALESCE(p.asset_id, la.asset_id)   AS asset_id,
           COALESCE(p.quantity, 0)             AS position_quantity,
           COALESCE(b.balance, 0)              AS ledger_balance
      FROM native_positions p
      FULL OUTER JOIN (
             SELECT la.owner_id, la.asset_id, la.id
               FROM ledger_accounts la
              WHERE la.owner_type = 'CUSTOMER' AND la.code = 'NATIVE_ASSET_BALANCE'
           ) la ON la.owner_id = p.account_id AND la.asset_id = p.asset_id
      LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
     WHERE COALESCE(p.quantity, 0) <> COALESCE(b.balance, 0);
$$;
-- +goose StatementEnd

-- A read model, written only by the triggers above.
GRANT SELECT ON native_positions TO cp_app, cp_readonly, cp_ops;
GRANT EXECUTE ON FUNCTION cp_native_positions_unreconciled() TO cp_app, cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a position's cost basis and realised P&L are financial history and are never dropped by rollback
