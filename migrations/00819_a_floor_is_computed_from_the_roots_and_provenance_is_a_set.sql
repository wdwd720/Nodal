-- +goose Up
-- A floor is computed from the provenance ROOTS, and provenance is carried as
-- a SET rather than as a rank (F-271, F-275, D-137, D-138).
--
-- ## 1. The floor depended on the order parent rows were inserted (F-271)
--
-- 00816 opens a lot's floor at its own origin and lowers it, as each parent row
-- lands, to the most restricted floor among its parents' CURRENT floors. That
-- reads one level deep. `cp_credit_lot_parent_is_written_at_mint` permits a
-- parent row for any lot created in THIS transaction -- so a transaction that
-- mints a child and a grandchild, and writes the GRANDCHILD's parent row first,
-- computes the grandchild's floor from a parent floor that has not fallen yet.
-- Nothing revisits it. The grandchild of a promotional grant then carries a
-- MARKET_TRADING_PROCEEDS floor and SandboxPolicy releases it.
--
-- 00816's own backfill states the correct rule: a lot's floor is the most
-- restricted origin among the lots its provenance BOTTOMS OUT IN, computed with
-- `WITH RECURSIVE`. The trigger implemented a different one. There were two ways
-- to make them the same:
--
--   a. recompute recursively on every parent row, and add a DEFERRED constraint
--      trigger that at COMMIT recomputes the floor of every lot whose ancestry
--      changed in the transaction; or
--   b. refuse a parent row for a lot that is ALREADY somebody's parent, and
--      recompute recursively.
--
-- (b) is taken, because its failure mode is a refusal and (a)'s is a wrong
-- floor. Under (a) the wrong floor exists for the length of the transaction and
-- is corrected at COMMIT by a second computation that has to find every affected
-- descendant; a defect in that search leaves a floor that is wrong and looks
-- settled. Under (b) the ordering that produces a stale read cannot be written
-- at all: `cp_credit_lot_parent_has_no_descendant_yet` raises CR005, the INSERT
-- fails, and the mint fails with it. A refusal is a mint that did not happen; a
-- wrong floor is money that may leave.
--
-- (b) also makes the incremental rule and the recursive rule provably identical,
-- which is why the recursion below is a statement of intent rather than a
-- correction. If a row (L,P) may only be written while nothing names L as a
-- parent, then every row (P,Q) precedes every row (L,P) -- because (L,P) names
-- P, and C would have refused (P,Q) after it. So when L's floor is computed, P's
-- provenance is complete and P's floor is final. By induction over the chain,
-- every ancestor's floor is final before it is read.
--
-- What (b) costs: a mint site that writes a chain of derived lots in one
-- transaction must write each lot's own provenance before using it as a parent.
-- No mint site in this build writes such a chain (internal/commerce mints one
-- earning, internal/nativemarket mints proceeds and fees out of one draw-down,
-- and none of them is a parent of another), so the constraint refuses nothing
-- that happens today and refuses the shape the audit drove.
--
-- ## 2. One origin cannot be conservative for a policy it was not ranked by
--    (F-275)
--
-- `cp_credit_origin_floor_rank` is `valuedomain.CreditOrigin.Restriction()`: how
-- restricted an origin is across the two policies THIS BUILD ships, with the
-- origin's own name breaking a tie. Both PURCHASED and MARKET_TRADING_PROCEEDS
-- rank 1, so a lot funded by one of each records MARKET_TRADING_PROCEEDS -- the
-- name that sorts first -- as its floor.
--
-- B-02, the open blocker the whole payout model waits on, asks whether Nodal's
-- closed-loop float is itself stored value and whether trading gains may ever be
-- withdrawn. One of its possible answers -- the float is stored value, the gains
-- are not -- is a policy that releases MARKET_TRADING_PROCEEDS and closes
-- PURCHASED. Under it, that lot is released, and half of what funded it was
-- refused. D-131's recorded residual says a policy persisted after a lot was
-- minted "would not change any floor already written ... which is the
-- conservative direction"; it is conservative only for a policy that releases a
-- superset of what this build's policies release, and the policy above is not a
-- superset of anything.
--
-- So `credit_lot_state.root_origins` carries the whole set of origins a lot's
-- provenance bottoms out in, maintained by the same recursive computation as the
-- floor -- one query, two columns -- and `valuedomain.Policy.Permits` releases a
-- lot only when the policy releases its own origin AND every root. A set can be
-- conservative for a policy nobody has written yet; a rank cannot.
--
-- `origin_floor` stays. It is the single most restricted origin, and it is what
-- a screen shows and what an ordering sorts by. It is no longer the whole of the
-- permission answer, and a CHECK constraint says what it now is: one of the
-- roots.
--
-- Custom SQLSTATE: CR005, internal/credit's provenance family (00816).

-- ---------------------------------------------------------------------------
-- 1. The roots of a lot's provenance, computed once
-- ---------------------------------------------------------------------------

-- The roots are the lots in this one's ancestry that nothing funded. A lot with
-- no parents is its own root.
--
-- UNION rather than UNION ALL, so a cycle -- which the parent trigger makes
-- unwritable, and which a database restored from before it could still hold --
-- terminates instead of recursing. DISTINCT and ORDER BY inside the aggregate so
-- the array is canonical: two lots with the same provenance hold the same array,
-- and a test comparing them compares values rather than orderings.
--
-- STABLE rather than IMMUTABLE: it reads tables. SECURITY DEFINER is not needed
-- -- every table it touches is one cp_app may already SELECT -- and is therefore
-- not taken.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_root_origins(p_lot uuid) RETURNS text[]
LANGUAGE sql STABLE SET search_path = pg_catalog, public, pg_temp AS $$
    WITH RECURSIVE reach(node) AS (
        SELECT p_lot
        UNION
        SELECT p.parent_lot_id
          FROM reach r JOIN credit_lot_parents p ON p.lot_id = r.node
    )
    SELECT array_agg(DISTINCT l.origin ORDER BY l.origin)
      FROM reach r
      JOIN credit_lots l ON l.id = r.node
     WHERE NOT EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = r.node);
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_credit_lot_root_origins(uuid) IS
    'The set of origins a lot provenance bottoms out in, most restricted first when sorted by cp_credit_origin_floor_rank. It is the rule 00816 backfill stated and its trigger did not implement; both credit_lot_state.origin_floor and credit_lot_state.root_origins are computed from it (00819, F-271, F-275, D-137, D-138).';

-- ---------------------------------------------------------------------------
-- 2. The root set, and a floor that is one of it
-- ---------------------------------------------------------------------------

ALTER TABLE credit_lot_state ADD COLUMN root_origins text[];

-- Existing rows, from the recursive rule -- which also CORRECTS any floor 00816's
-- trigger computed from a parent whose own floor had not fallen yet. A
-- deployment that minted a chain inside one transaction carries such a floor and
-- has no way to know it; recomputing every row is cheaper than deciding which.
UPDATE credit_lot_state st
   SET root_origins = coalesce(
        cp_credit_lot_root_origins(st.lot_id),
        ARRAY[(SELECT l.origin FROM credit_lots l WHERE l.id = st.lot_id)]);

UPDATE credit_lot_state st
   SET origin_floor = (
        SELECT o FROM unnest(st.root_origins) AS o
         ORDER BY cp_credit_origin_floor_rank(o), o LIMIT 1);

ALTER TABLE credit_lot_state
    ALTER COLUMN root_origins SET NOT NULL,
    ADD CONSTRAINT credit_lot_state_root_origins_declared CHECK (
        cardinality(root_origins) > 0
        AND root_origins <@ ARRAY[
            'PURCHASED','PROMOTIONAL','REFUND','CREATOR_EARNING','DATA_SALE_EARNING',
            'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING','MARKET_TRADING_PROCEEDS',
            'COMPETITION_REWARD','ADMIN_ADJUSTMENT','PROVIDER_SETTLEMENT']::text[]),
    ADD CONSTRAINT credit_lot_state_floor_is_a_root CHECK (origin_floor = ANY(root_origins));

COMMENT ON COLUMN credit_lot_state.root_origins IS
    'Every origin this value provenance bottoms out in: the lot own origin when nothing funded it, the union of its parents roots when something did. Trigger-maintained like finality and origin_floor; cp_app holds SELECT only. valuedomain.Policy.Permits releases a lot only when the policy releases the lot own origin and every one of these, because one ranked origin cannot be conservative for a policy it was not ranked by (00819, F-275, D-138).';

-- ---------------------------------------------------------------------------
-- 3. A lot with no parents is its own root
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_credit_lot_open() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    txdomain text;
BEGIN
    -- A lot must describe units that a journal transaction actually moved, in
    -- the Credit domain.
    SELECT DISTINCT la.value_domain INTO txdomain
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'CUSTOMER'
       AND la.owner_id = NEW.account_id
       AND e.asset_id = NEW.asset_id
     LIMIT 1;
    IF txdomain IS NULL THEN
        RAISE EXCEPTION 'CREDIT_LOT_UNBACKED: journal transaction % has no entry for account % on asset %',
            NEW.journal_transaction_id, NEW.account_id, NEW.asset_id USING ERRCODE = 'CR004';
    END IF;

    INSERT INTO credit_lot_state
        (lot_id, remaining_quantity, finality, origin_floor, root_origins, event_count, version, updated_at)
    VALUES (NEW.id, NEW.quantity, NEW.initial_finality, NEW.origin, ARRAY[NEW.origin], 0, 1, now());
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 4. A parent row may not be written for a lot that is already a parent
-- ---------------------------------------------------------------------------

-- The ordering constraint D-137 chose. Stated as a rule about rows rather than
-- about floors: a lot's provenance is complete before anything is derived from
-- it. BEFORE INSERT, so the refused row is never written and the mint fails.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_parent_has_no_descendant_yet() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    descendant uuid;
BEGIN
    SELECT p.lot_id INTO descendant
      FROM credit_lot_parents p WHERE p.parent_lot_id = NEW.lot_id LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'CREDIT_PARENT_AFTER_DESCENDANT: lot % is already a parent of lot %, so its provenance is closed; a lot provenance is written before anything is derived from it',
            NEW.lot_id, descendant USING ERRCODE = 'CR005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- After the mint check, so the more specific message wins on a row that fails
-- both. Trigger order on one table is alphabetical by trigger name, and
-- `credit_lot_parents_written_at_mint` sorts before this one.
CREATE TRIGGER credit_lot_parents_written_before_a_descendant
    BEFORE INSERT ON credit_lot_parents
    FOR EACH ROW EXECUTE FUNCTION cp_credit_lot_parent_has_no_descendant_yet();

COMMENT ON FUNCTION cp_credit_lot_parent_has_no_descendant_yet() IS
    'A credit_lot_parents row may not name a child that is already somebody parent. It makes the incremental floor rule identical to the recursive one: every ancestor provenance is complete before its floor is read, so a floor cannot depend on the order parent rows were inserted in (00819, F-271, D-137).';

-- ---------------------------------------------------------------------------
-- 5. The floor and the root set, from the roots
-- ---------------------------------------------------------------------------

-- One recursive computation, two columns. It replaces the one-level-deep read of
-- the parents' current floors: that read was the defect, and a rule stated in
-- two places is the shape this register is full of.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_credit_lot_apply_origin_floor() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    roots text[];
    floor text;
BEGIN
    -- Every parent must be projected, as before: a parent with no state row is
    -- a lot this database cannot describe, and inheriting from it silently is
    -- how a floor becomes a guess.
    IF EXISTS (
        SELECT 1 FROM credit_lot_parents p
         WHERE p.lot_id = NEW.lot_id
           AND NOT EXISTS (SELECT 1 FROM credit_lot_state ps WHERE ps.lot_id = p.parent_lot_id)
    ) THEN
        RAISE EXCEPTION 'CREDIT_PARENT_UNPROJECTED: lot % names parent % which has no credit_lot_state row',
            NEW.lot_id, NEW.parent_lot_id USING ERRCODE = 'CR005';
    END IF;

    roots := cp_credit_lot_root_origins(NEW.lot_id);
    IF roots IS NULL OR cardinality(roots) = 0 THEN
        RAISE EXCEPTION 'CREDIT_PARENT_ROOTLESS: lot % has parents and no provenance root',
            NEW.lot_id USING ERRCODE = 'CR005';
    END IF;
    SELECT o INTO floor FROM unnest(roots) AS o
     ORDER BY cp_credit_origin_floor_rank(o), o LIMIT 1;

    UPDATE credit_lot_state
       SET origin_floor = floor, root_origins = roots
     WHERE lot_id = NEW.lot_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CREDIT_LOT_MISSING: %', NEW.lot_id USING ERRCODE = 'CR001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_credit_lot_apply_origin_floor() IS
    'Sets credit_lot_state.root_origins to the origins a lot provenance bottoms out in and origin_floor to the most restricted of them, both from cp_credit_lot_root_origins. Recomputed from the whole ancestry on every parent row, so the answer does not depend on insertion order (00819, F-271, F-275, D-137, D-138).';

-- +goose Down
SELECT 1; -- protected: a lot's provenance roots and the floor computed from them are financial history and are never dropped by rollback
