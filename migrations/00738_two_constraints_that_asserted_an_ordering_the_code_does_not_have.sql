-- +goose Up
-- Two constraints that asserted an ordering the code does not have.
--
-- 00736 added `credit_fundings_lot_matches_state` and
-- `payout_requests_money_matches_state` alongside the birth control on
-- `agents`. Both are wrong, both were caught by the integration suite within
-- minutes, and both are dropped here rather than weakened, because what they
-- were reaching for is already done by the birth triggers in 00737.
--
-- This migration exists to record WHY, because the reason is a fact about the
-- money path that the next person to reach for the same constraint needs.
--
-- ## credit_fundings_lot_matches_state
--
-- It said: a funding has a lot exactly when its state is REVERSIBLE or later.
-- `MintFrom` does this, in this order:
--
--     AdvanceFunding(id, REVERSIBLE, ...)          -- state moves first
--     UPDATE credit_fundings SET lot_id = $2 ...   -- then the lot is recorded
--
-- so between those two statements the row is REVERSIBLE with a NULL lot, and a
-- CHECK is immediate. The ordering is deliberate: the advance is what carries
-- the transition row AU001 requires, and the lot id is the idempotency marker
-- that makes a second mint a no-op. Reversing them to satisfy a constraint
-- would put the marker before the state change it marks.
--
-- ## payout_requests_money_matches_state
--
-- It said: nothing is reserved while the state is one of the four
-- pre-reservation ones. `reserve` posts the ledger entries and writes
-- `reserved_quantity` while the request is still in ELIGIBILITY_CHECK or
-- VERIFICATION_*, and only then transitions. Same shape: the money moves inside
-- the transaction that will end with the state change, not after it.
--
-- ## What actually closes F-122
--
-- The birth triggers. `cp_credit_funding_born_created` and
-- `cp_payout_born_at_the_start` (00737) constrain CREATION, which is where the
-- forgery is, and say nothing about the orderings above. A row-lifetime CHECK
-- was the wrong instrument for a birth-time rule, and the two failures it
-- caused were the suite telling me so.
--
-- Left in place: `cp_agent_born_draft` (00736), which is a trigger and is
-- correct.

ALTER TABLE credit_fundings DROP CONSTRAINT credit_fundings_lot_matches_state;
ALTER TABLE payout_requests DROP CONSTRAINT payout_requests_money_matches_state;

-- +goose Down
SELECT 1; -- protected: these constraints refuse the money path's real ordering
