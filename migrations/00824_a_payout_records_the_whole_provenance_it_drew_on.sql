-- +goose Up
-- A payout allocation records the whole root SET, not only the floor
-- (F-282, D-141).
--
-- 00820 added `origin_floor` beside `origin` so an audit of a settled payout
-- could say what left as what it WAS. D-138 then moved the permission itself
-- onto the root set: `valuedomain.Policy.Permits` releases a lot only when the
-- policy releases the lot's own origin AND every origin its provenance bottoms
-- out in, because one ranked origin cannot be conservative for a policy it was
-- not ranked by. `Policy.RefusedRoot` exists for the same reason -- the root a
-- policy refuses is not always the one this build's rank calls the most
-- restricted.
--
-- The floor is the most restricted root, so two DIFFERENT sets share a floor
-- whenever they share a minimum: {CREATOR_EARNING} and {CREATOR_EARNING,
-- PURCHASED} both floor at CREATOR_EARNING. Under the policy B-02 can come back
-- with -- the closed-loop float is stored value, the gains are not -- the first
-- is released and the second is refused, and until this migration the two were
-- recorded identically and reported by GET /v1/payouts/{id} as one provenance.
--
-- It was never a lost fact: `payout_allocations.lot_id` references
-- `credit_lots`, a lot's root set never moves once written, and the set was one
-- join away for anyone who knew to make it. What was wrong is what the record
-- SAYS on its own, and 00820's own words are that this table is "the only
-- reader that can ever answer 'what actually left'" after the lot has been
-- consumed. A reader that has to know to join is a reader who will one day not.
--
-- The backfill is 00820's argument exactly: the set is read off the lot's own
-- state row, because 00816 fixes a lot's provenance at mint and 00819 makes it
-- independent of insertion order. A root set never moves, so the set now is the
-- set then. It is stated rather than assumed because a column backfilled from a
-- value that CAN move is a column that lies about history.

ALTER TABLE payout_allocations ADD COLUMN root_origins text[];

UPDATE payout_allocations a
   SET root_origins = st.root_origins
  FROM credit_lot_state st
 WHERE st.lot_id = a.lot_id;

-- An allocation whose lot has no state row cannot exist -- lot_id references
-- credit_lots and cp_credit_lot_open writes the projection in the same
-- statement -- and 00819 made root_origins NOT NULL on that projection. So
-- anything still null here is a row this schema says is impossible, and failing
-- the migration is the right answer.
ALTER TABLE payout_allocations
    ALTER COLUMN root_origins SET NOT NULL,
    ADD CONSTRAINT payout_allocations_root_origins_declared CHECK (
        cardinality(root_origins) > 0
        AND root_origins <@ ARRAY[
            'PURCHASED','PROMOTIONAL','REFUND','CREATOR_EARNING','DATA_SALE_EARNING',
            'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING','MARKET_TRADING_PROCEEDS',
            'COMPETITION_REWARD','ADMIN_ADJUSTMENT','PROVIDER_SETTLEMENT']::text[]),
    ADD CONSTRAINT payout_allocations_floor_is_a_root CHECK (origin_floor = ANY(root_origins));

COMMENT ON COLUMN payout_allocations.root_origins IS
    'Every origin the units in this allocation ultimately came from, copied off credit_lot_state at the moment they were reserved. It is what the payout policy actually reads (D-138): a policy releases these units only when it releases the lot own origin and all of these, and two sets that share a floor are two different provenances. payout.Provenance folds by (origin, origin_floor, root_origins) (00824, F-282, D-141).';

-- No new grant: 00713 already gives cp_app table-level INSERT on this table,
-- and a column grant beside a table grant is a second statement of one rule.

-- +goose Down
SELECT 1; -- protected: what a payout took, and what that value was, is financial history and is never dropped by rollback
