-- +goose Up
-- A payout allocation records what left as what it WAS (F-270, D-136).
--
-- `payout_allocations` records the lot, the origin and the quantity of every
-- unit a payout reserved. Since D-131 an origin is half of what a lot is: a lot
-- of MARKET_TRADING_PROCEEDS whose provenance bottoms out in a settled purchase
-- and one whose provenance bottoms out in a promotional grant carry the same
-- origin and are not the same value, and `valuedomain.Policy.Permits` releases
-- one and refuses the other.
--
-- So the allocation carries the floor beside the origin. Three things need it:
--
--   1. `GET /v1/payouts/{id}`'s provenance breakdown, which folded per origin
--      and therefore reported a payout drawn from two different provenances as
--      one line;
--   2. an audit of a settled payout, which is the only reader that can ever
--      answer "what actually left" after the lot has been consumed;
--   3. the fourth round of this audit, which reads exactly this table.
--
-- It is a RECORD, not a control. What stops the wrong lot being reserved is
-- `payout.Service.reserve` passing the decision's lot ids, which is D-136's
-- other half; this column is how a reader can tell afterwards.
--
-- The backfill reads the floor off the lot's own state row. That is the same
-- answer for every allocation this schema can hold, because 00816 fixes a lot's
-- floor at mint and 00819 makes it independent of insertion order: a floor never
-- moves, so the floor now is the floor then. It is stated rather than assumed
-- because a column backfilled from a value that CAN move is a column that lies
-- about history.

ALTER TABLE payout_allocations ADD COLUMN origin_floor text;

UPDATE payout_allocations a
   SET origin_floor = st.origin_floor
  FROM credit_lot_state st
 WHERE st.lot_id = a.lot_id;

-- An allocation whose lot has no state row cannot exist -- lot_id references
-- credit_lots and cp_credit_lot_open writes the projection in the same
-- statement -- so anything still null here is a row this schema says is
-- impossible. Failing the migration is the right answer; NOT NULL does it.
ALTER TABLE payout_allocations
    ALTER COLUMN origin_floor SET NOT NULL,
    ADD CONSTRAINT payout_allocations_origin_floor_check CHECK (origin_floor IN (
        'PURCHASED','PROMOTIONAL','REFUND','CREATOR_EARNING','DATA_SALE_EARNING',
        'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING','MARKET_TRADING_PROCEEDS',
        'COMPETITION_REWARD','ADMIN_ADJUSTMENT','PROVIDER_SETTLEMENT'));

COMMENT ON COLUMN payout_allocations.origin_floor IS
    'The most restricted origin in the provenance of the lot these units came from, recorded beside the origin so a settled payout can be told apart from one drawn on value a grant funded. payout.Provenance folds by (origin, origin_floor) (00820, F-270, D-136).';

-- No new grant: 00713 already gives cp_app table-level INSERT on this table,
-- and a column grant beside a table grant is a second statement of one rule.

-- +goose Down
SELECT 1; -- protected: what a payout took, and what that value was, is financial history and is never dropped by rollback
