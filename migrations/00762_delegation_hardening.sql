-- +goose Up
-- Delegated agent authority, hardened after review 3 (internal/delegation; 00752, 00753; ADR-0020).
--
-- 00752 stored whatever action class the agent chose for a swap. The class decides what a capability lets an
-- agent do (a REDUCE_EXPOSURE-only grant), whether a RESTRICTED account (risk reduction only) may act and
-- whether Risk Kernel V2 treats the swap as risk-increasing -- yet nothing tied it to what the swap does, so a
-- purchase labelled REDUCE_EXPOSURE passed every one of them. The service now derives the class from the
-- direction of the swap's value and refuses a label that disagrees; this migration makes the database refuse it
-- too. It also binds a delegation venue to a house book of its own: 00752 let a venue be backed by any house
-- book, a treasury book included, whose controls the venue's swaps would bypass.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A delegated swap's action class is what the swap does, decided by the direction of its value: it is
--      REDUCE_EXPOSURE exactly when it pays a token (instrument class SPOT_TOKEN or SPECULATIVE_TOKEN) for a
--      USD-pegged stable unit (STABLE_VALUE) -- the quote side of every venue's pair -- and ACQUIRE_EXPOSURE
--      otherwise, an asset with no instrument class included. The class is checked when the swap is admitted
--      and again when it is signed, so neither a forged row nor an asset reclassified in between lets a
--      purchase pass as a reduction. A swap's usage row and intent carry its class (00752), so a capability's
--      action classes and its usage are measured against the direction too. cp_delegation_instrument_class
--      and cp_delegation_swap_action are internal/delegation ClassifyAsset and SwapAction, rule for rule.
--   2. A delegation venue's house book is its own: it is not a treasury book (whose actions, limits and
--      crossing disclosures the venue's swaps would bypass) and it backs no other venue (whose inventory would
--      then be promised twice), and a venue's house book never becomes a treasury book afterwards. The
--      registrations that claim one house book are serialized on a transaction-scoped advisory lock, as
--      internal/delegation RegisterVenue takes it. Rows registered before this migration are left as they are.
--
-- Custom SQLSTATEs, continuing 00752's DG001..DG006:
--   DG007 a delegated swap whose action class is not the direction of its value.
--   DG008 a delegation venue on a treasury book or on another venue's house book, or a treasury book on a
--         delegation venue's house book.

-- ---------------------------------------------------------------------------
-- 1. A swap's action class is the direction of its value
-- ---------------------------------------------------------------------------

-- The instrument class of an asset exactly as internal/delegation ClassifyAsset decides it; NULL for an asset
-- delegated authority never touches (not SIMULATED, or UNSUPPORTED and not a USD-pegged stable unit).
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_instrument_class(p_asset uuid) RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN a.kind IS DISTINCT FROM 'SIMULATED' THEN NULL
        WHEN a.is_stablecoin AND a.peg_currency = 'USD' THEN 'STABLE_VALUE'
        WHEN a.risk_class = 'SPECULATIVE' THEN 'SPECULATIVE_TOKEN'
        WHEN a.risk_class = 'UNSUPPORTED' THEN NULL
        ELSE 'SPOT_TOKEN'
    END
      FROM assets a WHERE a.id = p_asset;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_instrument_class(uuid) SET search_path = pg_catalog, public, pg_temp;

-- What a swap paying p_pay for p_receive does: internal/delegation SwapAction.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_action(p_pay uuid, p_receive uuid) RETURNS text
LANGUAGE sql STABLE AS $$
    SELECT CASE
        WHEN cp_delegation_instrument_class(p_receive) = 'STABLE_VALUE'
         AND cp_delegation_instrument_class(p_pay) IN ('SPOT_TOKEN','SPECULATIVE_TOKEN') THEN 'REDUCE_EXPOSURE'
        ELSE 'ACQUIRE_EXPOSURE'
    END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_action(uuid, uuid) SET search_path = pg_catalog, public, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_direction() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    derived text := cp_delegation_swap_action(NEW.pay_asset_id, NEW.receive_asset_id);
BEGIN
    IF NEW.action_class IS DISTINCT FROM derived THEN
        RAISE EXCEPTION 'DELEGATION_ACTION_MISMATCH: swap % pays asset % for asset %, which is %, not %',
            NEW.id, NEW.pay_asset_id, NEW.receive_asset_id, derived, NEW.action_class USING ERRCODE = 'DG007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_direction() SET search_path = pg_catalog, public, pg_temp;

-- Triggers of one event fire in name order: these are named to fire after delegated_swaps_admit_network (00760: a
-- row without its network is refused for that first) and before delegated_swaps_birth and delegated_swaps_guard,
-- so a mislabelled swap is refused as exactly that.
CREATE TRIGGER delegated_swaps_bind_direction BEFORE INSERT ON delegated_swaps
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_swap_direction();
CREATE TRIGGER delegated_swaps_bind_direction_at_signing BEFORE UPDATE OF status ON delegated_swaps
    FOR EACH ROW WHEN (NEW.status = 'SIGNED' AND OLD.status IS DISTINCT FROM 'SIGNED')
    EXECUTE FUNCTION cp_delegation_swap_direction();

-- ---------------------------------------------------------------------------
-- 2. A venue's house book is its own
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_venue_house_dedicated() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    other text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('delegation/venue-house/' || NEW.house_account_id::text, 0));
    IF EXISTS (SELECT 1 FROM treasury_books WHERE house_account_id = NEW.house_account_id) THEN
        RAISE EXCEPTION 'DELEGATION_VENUE_HOUSE_NOT_DEDICATED: venue % names treasury book %; a venue needs a house book of its own',
            NEW.name, NEW.house_account_id USING ERRCODE = 'DG008';
    END IF;
    SELECT name INTO other FROM delegation_venues WHERE house_account_id = NEW.house_account_id ORDER BY created_at, id LIMIT 1;
    IF other IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_VENUE_HOUSE_NOT_DEDICATED: house book % already backs venue %; venue % needs one of its own',
            NEW.house_account_id, other, NEW.name USING ERRCODE = 'DG008';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_venue_house_dedicated() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegation_venues_house_dedicated BEFORE INSERT ON delegation_venues
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_venue_house_dedicated();

-- A treasury book's house book is frozen once registered (TY001), so its registration is the one moment to check.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_treasury_book_not_a_venue() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    venue text;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('delegation/venue-house/' || NEW.house_account_id::text, 0));
    SELECT name INTO venue FROM delegation_venues WHERE house_account_id = NEW.house_account_id ORDER BY created_at, id LIMIT 1;
    IF venue IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_VENUE_HOUSE_NOT_DEDICATED: house book % backs delegation venue % and cannot become a treasury book',
            NEW.house_account_id, venue USING ERRCODE = 'DG008';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_treasury_book_not_a_venue() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_books_not_a_delegation_venue BEFORE INSERT ON treasury_books
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_treasury_book_not_a_venue();

-- +goose Down
SELECT 1; -- protected: the bindings guard delegated authority's swap and venue history
