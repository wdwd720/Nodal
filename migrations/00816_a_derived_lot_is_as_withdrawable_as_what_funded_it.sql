-- +goose Up
-- A derived lot is as withdrawable as what funded it, and its provenance is
-- written by whoever mints it.
--
-- 00809 gave a derived lot PARENTS and made it inherit their FINALITY. Two
-- things about that table turned out to be load-bearing and unguarded.
--
-- ## 1. The origin travelled and the finality did (F-261, D-131)
--
-- D-124's rule is about finality only. So:
--
--   * a trader holding nothing but PROMOTIONAL, UNFUNDED Credits buys into a
--     native market and sells back out;
--   * the proceeds are MARKET_TRADING_PROCEEDS, an origin
--     `valuedomain.SandboxPolicy` marks withdrawable, at UNFUNDED, which
--     `PayoutEligible()` admits;
--   * `payout.Engine.Evaluate` returns Sufficient() at PAYOUT_KYC.
--
-- Goal section 23 forbids that shape in as many words: "nonwithdrawable source
-- -> trade -> magically payout-eligible balance unless the eventual
-- external/legal/provider policy explicitly allows it". SandboxPolicy says the
-- opposite of allowing it -- "a promotional grant that could leave the system
-- would be the first rule somebody copied" -- and CREDIT_ECONOMY.md section 4
-- says promotional value "can never leave this system under any policy in this
-- build". Before D-124 the pattern was unreachable by accident, because every
-- earning was minted REVERSIBLE for ever; D-124 opened it.
--
-- So every lot now carries an ORIGIN FLOOR beside its finality:
-- `credit_lot_state.origin_floor`, maintained the way the finality is, by a
-- trigger and by nothing else.
--
--   * a lot with no parents has its own origin as its floor;
--   * a derived lot's floor is the MOST RESTRICTED floor among its parents.
--
-- "Most restricted" is `cp_credit_origin_floor_rank` below, which is
-- `valuedomain.CreditOrigin.Restriction()` in SQL: an origin no payout policy
-- in this build releases ranks 0, one some policy releases ranks 1, one every
-- policy releases ranks 2. Ties break on the origin's own name so the answer
-- does not depend on which parent came back first. test/integration/enums holds
-- the function and the Go ordering identical.
--
-- `valuedomain.Policy.Permits` then permits a lot only when it permits BOTH the
-- lot's origin and its floor. What that costs is stated rather than hidden: a
-- trader who buys with a grant and sells at a profit cannot withdraw the profit
-- either, and a demo tier's money stays demo money however many times it is
-- traded. That is the intended answer (D-131).
--
-- ## 2. One INSERT invented a lot's provenance (F-266)
--
-- 00809 granted `SELECT, INSERT ON credit_lot_parents TO cp_app` because the
-- rows are written at mint. Nothing bound an INSERT to the mint. So, as
-- `cp_app`, months after the fact:
--
--     INSERT INTO credit_lot_parents (lot_id, parent_lot_id, quantity)
--     VALUES (<a card payment inside its dispute window>, <anybody's settled lot>, 1);
--
-- and `credit.Service.SettleDerived` -- whose candidate query asked only
-- "REVERSIBLE or SETTLED, and has a parent row" -- promoted a REVERSIBLE
-- purchase to payout-eligible without the protected finality column ever being
-- written directly.
--
-- `cp_credit_lot_parent_is_written_at_mint` binds the row to the mint:
--
--   1. the CHILD lot must have been created in this transaction. `now()` is
--      `transaction_timestamp()` and `credit_lots.created_at` defaults to it,
--      so equality is "the same transaction created this lot". A lot minted
--      earlier can never be given a parent again.
--   2. the PARENT must be one of three things: a lot the child's own journal
--      transaction CONSUMED (a marketplace sale, a native-market buy), a lot
--      recorded as a source of some market's pooled reserve (a native-market
--      sell, where the credits leaving the pool were paid in by other people in
--      earlier transactions), or another lot of the same account.
--
-- The residual, stated precisely because a third audit round reads it. Inside
-- the transaction that creates a lot, `cp_app` may still name as a parent any
-- lot of the same account, or any lot ever paid into any market's pool -- the
-- row does not name a market, so the trigger cannot tell which draw-down funded
-- which mint. That is not a laundering route, and the reason is worth stating:
-- both derived rules take the WORST parent -- least final finality, most
-- restricted origin floor -- so naming an EXTRA parent can only make a lot less
-- withdrawable. The direction that could launder is OMITTING a parent, which no
-- INSERT trigger can see; what guards that is that the mint sites are the only
-- writers, and that `SettleDerived` now refuses any lot with a `credit_fundings`
-- row outright, so externally funded value can never be promoted by this path
-- whatever its parent rows say.
--
-- The other residual: two transactions could in principle share a
-- `transaction_timestamp()`. They would also have to overlap in time and both
-- hold INSERT on this table; the microsecond resolution and the fact that a
-- mint is a single short transaction make it a theoretical case rather than a
-- reachable one, and closing it would mean recording the transaction id on
-- every lot, which is a column on a financial-history table for a case nobody
-- can construct.
--
-- ## 3. An index the new sweep predicate needs
--
-- `SettleDerived`'s candidate query now asks `NOT EXISTS (SELECT 1 FROM
-- credit_fundings f WHERE f.lot_id = ...)`, and `credit_fundings.lot_id` had no
-- index: it is reached by account, by state and by provider reference. One
-- partial index on the rows that name a lot.
--
-- Custom SQLSTATE: CR005, in internal/credit's family (CR001 lot invariants,
-- CR003 illegal finality, CR004 an unbacked lot).

-- ---------------------------------------------------------------------------
-- 1. How restricted an origin is
-- ---------------------------------------------------------------------------

-- Mirrors valuedomain.CreditOrigin.Restriction(). test/integration/enums calls
-- this for every declared origin and compares it with the Go answer, the same
-- way cp_credit_finality_can_transition is held to CanTransitionFinality.
--
-- IMMUTABLE, so it may be used in an index or an ORDER BY without the planner
-- re-evaluating it per row, and because it is a statement about this build's
-- policies rather than about any row.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_origin_floor_rank(p_origin text) RETURNS int
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        -- Closed under every payout policy in this build: value nobody paid
        -- for and nobody earned. valuedomain.OriginClosedEverywhere.
        WHEN p_origin IN ('PROMOTIONAL','REFUND','ADMIN_ADJUSTMENT',
                          'PROVIDER_SETTLEMENT','COMPETITION_REWARD') THEN 0
        -- Released by SandboxPolicy and refused by DefaultPolicy.
        -- valuedomain.OriginClosedSomewhere.
        WHEN p_origin IN ('PURCHASED','CREATOR_EARNING','DATA_SALE_EARNING',
                          'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING',
                          'MARKET_TRADING_PROCEEDS') THEN 1
        -- Anything else is an origin this build does not declare, and an
        -- undeclared origin is the most restricted thing there is: a typo must
        -- not become the floor somebody's money is measured against.
        ELSE 0
    END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_credit_origin_floor_rank(text) IS
    'How restricted a credit origin is across this build payout policies: 0 released by none, 1 released by some, 2 released by all. Mirrors valuedomain.CreditOrigin.Restriction(); test/integration/enums holds the two identical (00816, D-131).';

-- ---------------------------------------------------------------------------
-- 2. The floor column
-- ---------------------------------------------------------------------------

ALTER TABLE credit_lot_state ADD COLUMN origin_floor text;

-- Existing rows. A lot floor is the most restricted origin among the lots its
-- provenance bottoms out in, which for a lot with no parents is its own origin.
-- UNION rather than UNION ALL, so a cycle written before this migration
-- terminates instead of recursing.
-- +goose StatementBegin
WITH RECURSIVE reach(lot_id, node) AS (
    SELECT st.lot_id, st.lot_id FROM credit_lot_state st
    UNION
    SELECT r.lot_id, p.parent_lot_id
      FROM reach r JOIN credit_lot_parents p ON p.lot_id = r.node
),
roots AS (
    SELECT r.lot_id, l.origin
      FROM reach r
      JOIN credit_lots l ON l.id = r.node
     WHERE NOT EXISTS (SELECT 1 FROM credit_lot_parents p WHERE p.lot_id = r.node)
)
UPDATE credit_lot_state st
   SET origin_floor = coalesce(
        (SELECT ro.origin FROM roots ro WHERE ro.lot_id = st.lot_id
          ORDER BY cp_credit_origin_floor_rank(ro.origin), ro.origin LIMIT 1),
        (SELECT l.origin FROM credit_lots l WHERE l.id = st.lot_id));
-- +goose StatementEnd

ALTER TABLE credit_lot_state
    ALTER COLUMN origin_floor SET NOT NULL,
    ADD CONSTRAINT credit_lot_state_origin_floor_check CHECK (origin_floor IN (
        'PURCHASED','PROMOTIONAL','REFUND','CREATOR_EARNING','DATA_SALE_EARNING',
        'AGENT_SERVICE_EARNING','MARKET_CREATOR_EARNING','MARKET_TRADING_PROCEEDS',
        'COMPETITION_REWARD','ADMIN_ADJUSTMENT','PROVIDER_SETTLEMENT'));

COMMENT ON COLUMN credit_lot_state.origin_floor IS
    'The most restricted origin anywhere in this value provenance: the lot own origin when nothing funded it, the most restricted floor among its parents when something did. Trigger-maintained like finality; cp_app holds SELECT only. valuedomain.Policy.Permits releases a lot only when it releases both the origin and the floor (00816, F-261, D-131).';

-- The projection is opened with the lot own origin as its floor. A lot with no
-- parents ends there; one with parents is lowered by the trigger below, in the
-- same transaction, because a parent row may only be written then.
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
        (lot_id, remaining_quantity, finality, origin_floor, event_count, version, updated_at)
    VALUES (NEW.id, NEW.quantity, NEW.initial_finality, NEW.origin, 0, 1, now());
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 3. A parent row is written at mint, by whoever mints it
-- ---------------------------------------------------------------------------

-- BEFORE INSERT, so a refused row is never written. search_path pinned with
-- pg_temp named explicitly: without it pg_temp is searched FIRST for relation
-- names and a caller who may CREATE TEMP could hand this its own credit_lots
-- (00717, 00804, F-48).
-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_parent_is_written_at_mint() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    child credit_lots%ROWTYPE;
BEGIN
    SELECT * INTO child FROM credit_lots WHERE id = NEW.lot_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CREDIT_PARENT_ORPHANED: credit_lot_parents names lot % which does not exist',
            NEW.lot_id USING ERRCODE = 'CR005';
    END IF;
    IF child.created_at IS DISTINCT FROM now() THEN
        RAISE EXCEPTION 'CREDIT_PARENT_NOT_AT_MINT: lot % was created at % and this transaction started at %; a lot provenance is written when it is minted and never afterwards',
            NEW.lot_id, child.created_at, now() USING ERRCODE = 'CR005';
    END IF;
    IF NOT EXISTS (
        -- Consumed by the same journal transaction: a marketplace purchase, a
        -- native-market buy, anything where the payer own lots funded the mint.
        SELECT 1 FROM credit_lot_events e
         WHERE e.lot_id = NEW.parent_lot_id
           AND e.kind = 'CONSUME'
           AND e.journal_transaction_id = child.journal_transaction_id
        UNION ALL
        -- Paid into a market pooled reserve. A sell draws credits other people
        -- paid in, in earlier transactions, so this is the one case where a
        -- parent is legitimately another account lot that this transaction did
        -- not consume.
        SELECT 1 FROM native_market_credit_sources src
         WHERE src.lot_id = NEW.parent_lot_id
        UNION ALL
        -- The same account own value.
        SELECT 1 FROM credit_lots parent
         WHERE parent.id = NEW.parent_lot_id
           AND parent.account_id = child.account_id
    ) THEN
        RAISE EXCEPTION 'CREDIT_PARENT_UNRELATED: lot % cannot be funded by lot %; the parent was neither consumed by journal transaction %, nor paid into a market pool, nor held by the same account',
            NEW.lot_id, NEW.parent_lot_id, child.journal_transaction_id USING ERRCODE = 'CR005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_lot_parents_written_at_mint
    BEFORE INSERT ON credit_lot_parents
    FOR EACH ROW EXECUTE FUNCTION cp_credit_lot_parent_is_written_at_mint();

-- ---------------------------------------------------------------------------
-- 4. The floor falls to the worst parent
-- ---------------------------------------------------------------------------

-- Recomputed from ALL the lot parents on every insert rather than folded one
-- row at a time, so the answer does not depend on the order the rows arrive in
-- and re-running it is a no-op. SECURITY DEFINER because credit_lot_state is a
-- projection the application may only SELECT.
--
-- event_count and version are deliberately untouched: they count LOT EVENTS,
-- and the provenance of a lot being minted is not one.
-- +goose StatementBegin
CREATE FUNCTION cp_credit_lot_apply_origin_floor() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
    floor text;
BEGIN
    SELECT ps.origin_floor INTO floor
      FROM credit_lot_parents p
      JOIN credit_lot_state ps ON ps.lot_id = p.parent_lot_id
     WHERE p.lot_id = NEW.lot_id
     ORDER BY cp_credit_origin_floor_rank(ps.origin_floor), ps.origin_floor
     LIMIT 1;
    IF floor IS NULL THEN
        RAISE EXCEPTION 'CREDIT_PARENT_UNPROJECTED: lot % names parent % which has no credit_lot_state row',
            NEW.lot_id, NEW.parent_lot_id USING ERRCODE = 'CR005';
    END IF;
    UPDATE credit_lot_state SET origin_floor = floor WHERE lot_id = NEW.lot_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CREDIT_LOT_MISSING: %', NEW.lot_id USING ERRCODE = 'CR001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_lot_parents_lower_the_floor
    AFTER INSERT ON credit_lot_parents
    FOR EACH ROW EXECUTE FUNCTION cp_credit_lot_apply_origin_floor();

REVOKE EXECUTE ON FUNCTION cp_credit_lot_apply_origin_floor() FROM PUBLIC;

COMMENT ON FUNCTION cp_credit_lot_parent_is_written_at_mint() IS
    'A credit_lot_parents row may only be written in the transaction that created the lot, and only for a parent that transaction consumed, a lot paid into a market pool, or another lot of the same account. Without it one INSERT invented the provenance of a card payment months after the fact and the derived sweep promoted it (00816, F-266).';
COMMENT ON FUNCTION cp_credit_lot_apply_origin_floor() IS
    'Lowers credit_lot_state.origin_floor to the most restricted floor among a lot parents, recomputed from all of them so the answer does not depend on insertion order (00816, F-261, D-131).';

-- ---------------------------------------------------------------------------
-- 5. The index the derived sweep predicate needs
-- ---------------------------------------------------------------------------

CREATE INDEX credit_fundings_lot_idx ON credit_fundings (lot_id) WHERE lot_id IS NOT NULL;

-- +goose Down
SELECT 1; -- protected: a lot's provenance and the floor computed from it are financial history and are never dropped by rollback
