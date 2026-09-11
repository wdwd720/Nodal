-- +goose Up
-- Four functions written after F-48 was fixed repeated F-48 (F-194).
--
-- Migration 00717 pinned `pg_catalog, public, pg_temp` on the five SECURITY
-- DEFINER functions that had `SET search_path = public`, and said exactly why:
-- TEMP on a database is granted to PUBLIC by default and is revoked nowhere in
-- this tree, so any caller may create a temp relation; with pg_temp unpinned it
-- is searched FIRST for relation names, and a temp table shadowing a real one is
-- read by the definer with the definer's privileges.
--
-- 00771 and 00772 then wrote four more functions with `SET search_path = public`:
--
--   cp_native_market_check_print()    -- the NM001 price check on every print
--   cp_native_position_allocate()     -- writes a position from native_assets
--   cp_native_position_apply_fill()   -- writes a position from native_markets
--   cp_native_positions_unreconciled()-- the reconciliation query
--
-- The first two are the ones that bite. ADR-0027 §1 and D-063 state that "cp_app
-- holds SELECT on native_positions and nothing else ... there is no application
-- path by which a cost basis can be set to something the trades do not support",
-- and cp_app really does lack INSERT on the table -- but it may open a market,
-- and cp_native_position_allocate reads `native_assets` to decide whose creator
-- allocation to write and how large it is. Shadow that relation in pg_temp and
-- the trigger writes a position, with an owner and a quantity of the caller's
-- choosing, from a row the caller wrote. The foreign keys resolve by OID and
-- cannot be shadowed, so the market still points at the real asset; only the
-- trigger's view of it is forged. The same shape applies to the NM001 print
-- check, which re-derives its expected prices from `native_market_fills`: the
-- fill's foreign key still refuses the row, so the check is not the control that
-- refuses it, and a claim that "a print cannot say the market traded somewhere
-- it did not" rests on a key rather than on the check written to enforce it.
--
-- The last two are not exploitable in the same way -- one is STABLE sql that
-- cp_app calls as itself, the other reads native_markets under the caller's own
-- privileges -- and they are pinned regardless. A rule with exceptions is a rule
-- nobody can check, and `test/integration/migrations` now asserts the rule for
-- EVERY prosecdef function in `public`, which is the guard 00717 did not leave
-- behind and is why this defect could be introduced twice.
--
-- ALTER FUNCTION ... SET search_path replaces the setting without touching the
-- body, so this is the whole fix and none of the logic moves (00717's shape).
ALTER FUNCTION cp_native_market_check_print()     SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_native_position_allocate()      SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_native_position_apply_fill()    SET search_path = pg_catalog, public, pg_temp;
ALTER FUNCTION cp_native_positions_unreconciled() SET search_path = pg_catalog, public, pg_temp;

-- +goose Down
SELECT 1; -- protected: unpinning a definer's search path would re-open F-48 on the position and print triggers
