-- +goose Up
-- Supply-controlled assets: units of a platform stable asset or of a bridged twin come into being only as their
-- controller issues them (review 3, observation "the platform stable asset has no supply binding of its own kind";
-- internal/stableasset, internal/bridge, internal/sandboxfund; ADVANCED_FINANCIAL_MODEL.md §5; amends 00747 and
-- 00748 without editing them, on the model of 00756's rule for prediction-market outcome shares).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. An asset is SUPPLY-CONTROLLED when a controller owns its supply: a platform stable asset
--      (stable_assets.asset_id; its controller is the asset's STABLE_ISSUER facility, whose STABLE_OUTSTANDING is
--      what was issued) or a bridged twin (bridge_routes.destination_asset_id; its controller is the route's BRIDGE
--      facility, whose BRIDGE_OUTSTANDING is what was released). An asset becomes supply-controlled only while no
--      ledger account names it, so its holdings and its controller's outstanding both start at zero (SU003).
--   2. A journal entry on a supply-controlled asset belongs to a transaction of a kind that asset's controllers or
--      an ordinary customer movement uses (cp_supply_controlled_kind_allowed). The allowed kinds are exactly:
--        - the asset's own controller: STABLE_ISSUANCE and STABLE_REDEMPTION for a platform stable asset;
--          BRIDGE_DESTINATION_FINALIZED (the attested release that mints it) for a bridged twin;
--        - a facility that holds or locks the asset without issuing it: STABLE_ISSUANCE and STABLE_REDEMPTION when
--          it is a reserve asset of a stable asset (stable_reserve_assets: the reserve paid in and paid out);
--          BRIDGE_SOURCE_FINALIZED, BRIDGE_DESTINATION_FINALIZED and BRIDGE_REFUND when it is a route's source asset
--          (the lock, the release that consumes the customer's in-flight units, the refund); PREDICTION_SET_ISSUANCE,
--          PREDICTION_SET_REDEMPTION, PREDICTION_PAYOUT and PREDICTION_REFUND when it is a prediction market's
--          collateral (prediction_markets.collateral_asset_id: escrow in and out);
--        - an ordinary customer movement, for any supply-controlled asset: SIM_TRADE (a trade between two books --
--          customers on the internal order book, a house book's principal trade), P2P_TRANSFER (customer to
--          customer), COMPENSATION (the reversal of one of these), MARGIN_COLLATERAL_MOVE and
--          DERIVATIVE_MARGIN_MOVE (a customer's own units into and out of collateral).
--      Every other kind is refused before the entry is written: a faucet grant (SANDBOX_FUNDING), a facility's or a
--      house book's funding (FACILITY_FUNDING, HOUSE_FUNDING), a withdrawal (SANDBOX_WITHDRAWAL), a corporate action,
--      every margin, derivative, option, equity and performance-fee movement but the two collateral moves, and
--      every kind of the core ledger. Adding one is a migration (SU001).
--   3. A controller's issuance accounts of a supply-controlled asset -- STABLE_ISSUED and STABLE_OUTSTANDING of a
--      stable asset, BRIDGE_MINTED and BRIDGE_OUTSTANDING of a twin -- are booked only by that controller (its
--      facility) and only in that controller's own kinds (SU001).
--   4. At COMMIT, a journal transaction that touches a supply-controlled asset changes the asset's HOLDINGS -- the
--      sum, over every owner, of every account under a holding code (cp_supply_holding_codes, the Go twin of
--      ledger.HoldingCodes: what a supply reconciliation counts as outstanding) -- by exactly what it changes the
--      controller's outstanding by: an issuance and a redemption by the units they issue or retire, everything else
--      by nothing (SU002). A paper fill against no counterparty, a grant, or a trade that books a unit to a flow
--      account can therefore never create or destroy a unit, and the stable asset's supply identity -- holdings
--      equal STABLE_OUTSTANDING -- holds transaction by transaction, as 00756 makes customer share wallets equal
--      a market's outstanding shares.
--
-- Every journal entry pays two probes of unique indexes (stable_assets.asset_id, bridge_routes.destination_asset_id);
-- only entries on a supply-controlled asset go further. The checks are deferred like every other journal check, so
-- cp_upkeep_check_deferred (00760) runs them at the end of each upkeep item.
--
-- Custom SQLSTATEs (prefix SU): SU001 a journal entry on a supply-controlled asset in a kind its controllers and
-- customer movements do not use, or on its controller's issuance accounts by another party or in another kind;
-- SU002 a journal transaction that changes a supply-controlled asset's holdings by other than what it changes its
-- controller's outstanding by; SU003 an asset that ledger accounts already name registered as a stable asset or as
-- a route's twin.

-- ---------------------------------------------------------------------------
-- 1. What a supply-controlled asset is, and what may move it
-- ---------------------------------------------------------------------------

-- The holding codes of internal/ledger (ledger.HoldingCodes); a parity test in internal/sandboxfund compares them.
-- +goose StatementBegin
CREATE FUNCTION cp_supply_holding_codes() RETURNS text[]
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT ARRAY['WALLET','PLATFORM_FEE_RECEIVABLE','CREDIT_BALANCE','NATIVE_ASSET_BALANCE','PAYOUT_RESERVED',
                 'MARKET_RESERVE','MARKET_INVENTORY','MARGIN_COLLATERAL','DERIVATIVES_MARGIN','UNSETTLED_RECEIVABLE',
                 'HOUSE_INVENTORY','STABLE_RESERVE','POOL_CASH','INSURANCE_FUND_CASH','CLEARING_CASH','BRIDGE_LOCKED',
                 'PREDICTION_ESCROW']::text[];
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_supply_holding_codes() SET search_path = pg_catalog, public, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION cp_supply_controlled(p_asset uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM stable_assets WHERE asset_id = p_asset)
        OR EXISTS (SELECT 1 FROM bridge_routes WHERE destination_asset_id = p_asset);
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_supply_controlled(uuid) SET search_path = pg_catalog, public, pg_temp;

-- The allowed kinds of point 2, by the roles the asset has.
-- +goose StatementBegin
CREATE FUNCTION cp_supply_controlled_kind_allowed(p_asset uuid, p_kind text) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT p_kind IN ('SIM_TRADE','P2P_TRANSFER','COMPENSATION','MARGIN_COLLATERAL_MOVE','DERIVATIVE_MARGIN_MOVE')
        OR (p_kind IN ('STABLE_ISSUANCE','STABLE_REDEMPTION')
            AND (EXISTS (SELECT 1 FROM stable_assets WHERE asset_id = p_asset)
                 OR EXISTS (SELECT 1 FROM stable_reserve_assets WHERE reserve_asset_id = p_asset)))
        OR (p_kind = 'BRIDGE_DESTINATION_FINALIZED'
            AND EXISTS (SELECT 1 FROM bridge_routes WHERE destination_asset_id = p_asset))
        OR (p_kind IN ('BRIDGE_SOURCE_FINALIZED','BRIDGE_DESTINATION_FINALIZED','BRIDGE_REFUND')
            AND EXISTS (SELECT 1 FROM bridge_routes WHERE source_asset_id = p_asset))
        OR (p_kind IN ('PREDICTION_SET_ISSUANCE','PREDICTION_SET_REDEMPTION','PREDICTION_PAYOUT','PREDICTION_REFUND')
            AND EXISTS (SELECT 1 FROM prediction_markets WHERE collateral_asset_id = p_asset));
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_supply_controlled_kind_allowed(uuid, text) SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 2. Each entry: an allowed kind, and issuance accounts only for the controller (SU001)
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_supply_controlled_entry_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k  text;
    la record;
BEGIN
    IF NOT cp_supply_controlled(NEW.asset_id) THEN
        RETURN NEW;
    END IF;
    SELECT kind INTO k FROM journal_transactions WHERE id = NEW.transaction_id;
    IF k IS NULL OR NOT cp_supply_controlled_kind_allowed(NEW.asset_id, k) THEN
        RAISE EXCEPTION 'SUPPLY_CONTROLLED_FOREIGN_KIND: asset % is issued only by its controller and cannot move in a % transaction',
            NEW.asset_id, coalesce(k, 'unknown') USING ERRCODE = 'SU001';
    END IF;
    SELECT owner_type, owner_id, code INTO la FROM ledger_accounts WHERE id = NEW.ledger_account_id;
    IF la.code IN ('STABLE_ISSUED','STABLE_OUTSTANDING') AND NOT (
           k IN ('STABLE_ISSUANCE','STABLE_REDEMPTION') AND la.owner_type = 'FACILITY'
           AND EXISTS (SELECT 1 FROM stable_assets WHERE asset_id = NEW.asset_id AND facility_id = la.owner_id)) THEN
        RAISE EXCEPTION 'SUPPLY_CONTROLLED_FOREIGN_ISSUER: % of asset % may be booked only by its own stable issuer in its issuance or redemption, not by %/% in a % transaction',
            la.code, NEW.asset_id, la.owner_type, la.owner_id, k USING ERRCODE = 'SU001';
    END IF;
    IF la.code IN ('BRIDGE_MINTED','BRIDGE_OUTSTANDING') AND NOT (
           k = 'BRIDGE_DESTINATION_FINALIZED' AND la.owner_type = 'FACILITY'
           AND EXISTS (SELECT 1 FROM bridge_routes WHERE destination_asset_id = NEW.asset_id AND facility_id = la.owner_id)) THEN
        RAISE EXCEPTION 'SUPPLY_CONTROLLED_FOREIGN_ISSUER: % of asset % may be booked only by the bridge whose release mints it, not by %/% in a % transaction',
            la.code, NEW.asset_id, la.owner_type, la.owner_id, k USING ERRCODE = 'SU001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_supply_controlled_entry_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER journal_entries_supply_controlled_guard BEFORE INSERT ON journal_entries
    FOR EACH ROW EXECUTE FUNCTION cp_supply_controlled_entry_guard();

-- ---------------------------------------------------------------------------
-- 3. Each transaction, at COMMIT: holdings move exactly with the controller's outstanding (SU002)
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_check_supply_controlled_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    flag text;
    bad  record;
BEGIN
    IF NOT cp_supply_controlled(NEW.asset_id) THEN
        RETURN NULL;
    END IF;
    flag := 'cp.supply.controlled.x' || translate(NEW.transaction_id::text, '-', '_');
    IF current_setting(flag, true) = '1' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config(flag, '1', true);

    SELECT x.asset_id, x.holdings, x.outstanding INTO bad
      FROM (SELECT e.asset_id,
                   sum(CASE WHEN la.code = ANY (cp_supply_holding_codes())
                            THEN CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END) AS holdings,
                   sum(CASE WHEN la.code IN ('STABLE_OUTSTANDING','BRIDGE_OUTSTANDING')
                            THEN CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE -e.quantity END ELSE 0 END) AS outstanding
              FROM journal_entries e
              JOIN ledger_accounts la ON la.id = e.ledger_account_id
             WHERE e.transaction_id = NEW.transaction_id
             GROUP BY e.asset_id) x
     WHERE x.holdings <> x.outstanding AND cp_supply_controlled(x.asset_id)
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'SUPPLY_NOT_CONSERVED: transaction % moves the holdings of supply-controlled asset % by % and its controller''s outstanding by %',
            NEW.transaction_id, bad.asset_id, bad.holdings, bad.outstanding USING ERRCODE = 'SU002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_supply_controlled_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER journal_entries_supply_controlled_conserved AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_supply_controlled_journal();

-- ---------------------------------------------------------------------------
-- 4. An asset becomes supply-controlled only before any value moves in it (SU003)
-- ---------------------------------------------------------------------------

-- Named to fire after each table's consistency trigger (stable_assets_consistent, bridge_routes_consistent): a row
-- inconsistent with its asset is refused for that first (SV006, BR004).
-- +goose StatementBegin
CREATE FUNCTION cp_supply_controlled_registered_fresh() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    a uuid;
BEGIN
    IF TG_TABLE_NAME = 'stable_assets' THEN
        a := NEW.asset_id;
    ELSE
        a := NEW.destination_asset_id;
    END IF;
    IF EXISTS (SELECT 1 FROM ledger_accounts WHERE asset_id = a) THEN
        RAISE EXCEPTION 'SUPPLY_CONTROLLED_PREEXISTING: asset % already has ledger accounts and cannot become supply-controlled (%)',
            a, TG_TABLE_NAME USING ERRCODE = 'SU003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_supply_controlled_registered_fresh() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER stable_assets_supply_registered_fresh BEFORE INSERT ON stable_assets
    FOR EACH ROW EXECUTE FUNCTION cp_supply_controlled_registered_fresh();
CREATE TRIGGER bridge_routes_supply_registered_fresh BEFORE INSERT ON bridge_routes
    FOR EACH ROW EXECUTE FUNCTION cp_supply_controlled_registered_fresh();

-- +goose Down
SELECT 1; -- protected: the rules guard posted ledger history and are never dropped
