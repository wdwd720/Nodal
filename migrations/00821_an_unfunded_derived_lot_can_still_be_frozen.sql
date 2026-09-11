-- +goose Up
-- An UNFUNDED derived lot can still be frozen by a disputed parent (F-273,
-- D-124 amended).
--
-- 00711's `cp_credit_finality_can_transition` mirrors
-- `valuedomain.CanTransitionFinality`, and both say UNFUNDED is TERMINAL:
-- "nothing external backs it, so nothing external can change it."
--
-- That sentence is true of the lots UNFUNDED was written for -- a promotional
-- grant, an admin adjustment -- and false of a DERIVED lot minted there. D-124
-- gives a derived lot the LEAST FINAL finality among its parents, and
-- valuedomain's ordering puts UNFUNDED between REVERSIBLE and SETTLED. So a
-- seller's earning funded by a buyer's grant AND a buyer's settled card
-- purchase is minted UNFUNDED -- and something external backs part of it.
--
-- When the card is charged back, D-124's freeze direction says that earning
-- moves to DISPUTED. It could not: the candidate query opened
-- `finality IN ('REVERSIBLE','SETTLED')`, and even had it selected the lot,
-- `SetFinality` and this function would both have refused the edge. Nothing
-- ever froze it. F-260 named the direction: "an earning whose funding was
-- charged back stays spendable."
--
-- UNFUNDED -> DISPUTED is therefore legal. Nothing else about UNFUNDED changes:
-- it may not become REVERSIBLE or SETTLED, because there is still no funding row
-- to settle and a promotion out of UNFUNDED would be value inventing a backer.
-- The edge only ever runs one way, towards frozen, which is the direction a
-- mistake in it falls safely.
--
-- What comes after is the edge table's existing answer: DISPUTED -> SETTLED,
-- REVERSED or REVERSIBLE. A derived lot frozen this way and later resolved in
-- the platform's favour does not return to UNFUNDED -- no edge does -- and
-- `SettleDerived` only promotes from REVERSIBLE, so it stays DISPUTED until a
-- human moves it. That residual is D-124's, recorded again here rather than
-- widened: an automatic un-freeze is a second sweep deciding that somebody
-- else's dispute ended well, and this build has no evidence it could read to
-- decide that.
--
-- test/integration/enums and internal/valuedomain's finality parity test hold
-- this function and CanTransitionFinality identical over all twenty-five pairs.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_credit_finality_can_transition(p_from text, p_to text)
RETURNS boolean LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_from
        -- UNFUNDED is terminal except towards frozen. A derived lot is minted
        -- here whenever its least final parent is a grant, and one of its OTHER
        -- parents can still be charged back (00821, F-273).
        WHEN 'UNFUNDED'   THEN p_to IN ('DISPUTED')
        WHEN 'REVERSIBLE' THEN p_to IN ('SETTLED','DISPUTED','REVERSED')
        WHEN 'SETTLED'    THEN p_to IN ('DISPUTED')
        WHEN 'DISPUTED'   THEN p_to IN ('SETTLED','REVERSED','REVERSIBLE')
        WHEN 'REVERSED'   THEN false
        ELSE false
    END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_credit_finality_can_transition(text, text) IS
    'The legal funding-finality edges. Mirrors internal/valuedomain.CanTransitionFinality; internal/valuedomain finality parity test compares the two on all twenty-five pairs. UNFUNDED may only move to DISPUTED, because a derived lot minted UNFUNDED can have a parent something external can still reclaim (00821, F-273, D-124).';

-- +goose Down
SELECT 1; -- protected: reverting leaves a derived lot funded in part by a charged-back payment spendable for ever
