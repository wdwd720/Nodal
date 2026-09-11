-- +goose Up
-- A provenance the database cannot compute is not a provenance to guess at
-- (F-283, D-137 amended).
--
-- 00819 computes a lot's provenance roots with `WITH RECURSIVE ... UNION`, and
-- the UNION is deliberate: a cycle terminates instead of recursing. What it
-- terminates WITH is nothing. `cp_credit_lot_root_origins` selects the reachable
-- nodes that are nobody's child, and in a cycle every node is somebody's child,
-- so `array_agg` over an empty set answers NULL.
--
-- 00819's own backfill then read that NULL as "this lot has no parents":
--
--     SET root_origins = coalesce(cp_credit_lot_root_origins(st.lot_id),
--                                 ARRAY[(SELECT l.origin FROM credit_lots l
--                                         WHERE l.id = st.lot_id)])
--
-- The fallback is the mint-time rule -- a lot with no parents is its own root --
-- applied to a lot that HAS parents and whose provenance is unknowable. For a
-- lot of MARKET_TRADING_PROCEEDS with a promotional grant somewhere in a cycle
-- behind it, that writes root_origins = {MARKET_TRADING_PROCEEDS}, floors it
-- there, and `SandboxPolicy` releases it. It is the permissive direction, in the
-- migration whose entire subject is that a floor must be conservative.
--
-- The trigger path has always failed closed on the same input:
-- `cp_credit_lot_apply_origin_floor` raises CREDIT_PARENT_ROOTLESS when the
-- recursion answers nothing. Only the backfill substituted, and only a
-- backfill can meet the case at all: a cycle is unwritable through 00819's
-- triggers, so the only way one exists is in data restored from a deployment
-- that ran on 00816 to 00818, where `cp_credit_lot_parent_has_no_descendant_yet`
-- did not exist and two lots minted in one transaction could name each other.
--
-- So the schema refuses to move. A migration that cannot compute a provenance
-- has exactly two honest options -- stop, or write down a guess -- and a guess
-- about where value came from is the one thing this model may never make. The
-- repair belongs to whoever restored the data: break the cycle as the migration
-- role, so that every lot with parents has a reachable root, and run the
-- migration again. There is no automatic repair here on purpose, because every
-- automatic one would have to choose which edge of the cycle is the lie.
--
-- The refusal is a migration-time check rather than a constraint: cycles are
-- already unwritable, and a CHECK that re-ran the recursion on every parent row
-- would price the ordinary path for a case only a restore can produce.
--
-- Custom SQLSTATE: CR005, internal/credit's provenance family (00816, 00819).

-- The lots a restore can leave behind, as a set an operator can read before
-- deciding what to repair. Not SECURITY DEFINER and not revoked from PUBLIC:
-- it reads exactly the two tables `cp_credit_lot_root_origins` already reads,
-- and every role that can call that can call this.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_lots_without_computable_roots() RETURNS SETOF uuid
LANGUAGE sql STABLE SET search_path = pg_catalog, public, pg_temp AS $$
    SELECT l.id
      FROM credit_lots l
     WHERE EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = l.id)
       AND cp_credit_lot_root_origins(l.id) IS NULL
     ORDER BY l.id;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_credit_lots_without_computable_roots() IS
    'Every lot that has parent rows and no reachable provenance root, which on this schema means a cycle in credit_lot_parents. It is empty on any database this build wrote; a restore from 00816-00818 can make it non-empty, and 00825 refuses to migrate while it is (00825, F-283, D-137).';

-- +goose StatementBegin
DO $$
DECLARE
    n        bigint;
    offender uuid;
BEGIN
    SELECT count(*) INTO n FROM cp_credit_lots_without_computable_roots();
    IF n > 0 THEN
        SELECT lot INTO offender FROM cp_credit_lots_without_computable_roots() AS lot LIMIT 1;
        RAISE EXCEPTION 'CREDIT_PROVENANCE_UNKNOWABLE: % lot(s) have parent rows and no computable provenance root, the first being %. Their provenance is unknowable, so the floor that decides whether their value may leave cannot be computed and must not be guessed: 00819''s backfill would write each lot''s own origin, which is the permissive answer. Repair the data as the migration role -- SELECT * FROM cp_credit_lots_without_computable_roots() names them, and a cycle in credit_lot_parents is the only way this schema can hold one -- before the schema moves.',
            n, offender USING ERRCODE = 'CR005';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
SELECT 1; -- protected: the check is a one-time refusal and the function it leaves behind is how an operator finds the lots it refused; removing that reader is not a rollback of anything
