-- +goose Up
-- Proceeds are as final as what paid for them.
--
-- `internal/nativemarket` and `internal/commerce` minted every earning at
-- `FinalityReversible`, unconditionally:
--
--     const derived = valuedomain.FinalityReversible   (nativemarket:555)
--     Finality: valuedomain.FinalityReversible         (commerce:485)
--
-- Both said the same thing in a comment -- "an earning cannot be more final than
-- the money behind it" -- and neither looked at the money behind it.
--
-- The only writer that promotes a lot out of REVERSIBLE is
-- `credit.Service.SettleFunding`, which keys on `credit_fundings.lot_id`. An
-- earning has no funding row and never gets one, so nothing could ever move it.
-- `FundingFinality.PayoutEligible()` admits only SETTLED and UNFUNDED, so five
-- of the six origins `valuedomain.SandboxPolicy` marks withdrawable could never
-- be withdrawn on any deployment, and `GET /v1/me/eligibility` reported
-- FUNDING_NOT_SETTLED -- which `internal/eligibility` documents as a reason
-- WAITING fixes -- on value whose finality nothing could move. The browser audit
-- found the same thing from the outside (F-e2e-1): a verified customer with a
-- tokenised destination and a permitted origin, held at FUNDING_NOT_SETTLED with
-- nothing able to settle it. (F-230/F-wv-3, D-124.)
--
-- ## What a derived lot is, and what it is made of
--
-- A DERIVED lot is one minted out of value that was already inside the system:
-- trading proceeds, a creator earning, marketplace proceeds and the platform's
-- fee on them. It is not funded from outside, so it has no `credit_fundings`
-- row and never will; what it has is PARENTS.
--
-- `credit_lot_parents` records them: which lots were consumed to fund this one,
-- and how much of each. It is written at mint, in the same transaction as the
-- consumption, and it is append-only. A derived lot is minted at the LEAST FINAL
-- finality among its parents, so:
--
--   * parents all UNFUNDED or SETTLED -> the derived lot is payout-eligible at
--     birth, which is what makes an earning on a sandbox tier reachable;
--   * any parent REVERSIBLE -> REVERSIBLE, and `credit.Service.SettleDerived`
--     promotes it once every parent has reached a payout-eligible finality;
--   * any parent DISPUTED or REVERSED -> the derived lot is frozen with it.
--
-- ## The pool is fungible, so the pool keeps its own record
--
-- A native market's automated maker holds a pooled credit reserve. On a BUY the
-- trader's lots are consumed into it; on a SELL credits come back out of it, and
-- there is no transaction-local answer to "whose credits were those". Without an
-- answer a sell's proceeds would either be REVERSIBLE for ever -- the defect,
-- renamed -- or payout-eligible by default, which is the laundering route the
-- whole finality model exists to close: buy with card-funded Credits, sell back,
-- withdraw, charge back.
--
-- So the pool keeps the same kind of record the credit ledger keeps.
-- `native_market_credit_sources` is one row per lot paid into a market's
-- reserve, with a `remaining` the sell side draws down in the order the credits
-- arrived. The parents of a sell's proceeds are exactly the source rows it drew
-- down, which makes them real parents rather than an average, and keeps the
-- table bounded by the pool's outstanding reserve rather than by the market's
-- whole history.
--
-- A shortfall is possible and is answered honestly: a market whose recorded
-- sources do not cover what is leaving -- a market that traded before this
-- migration, most obviously -- mints the uncovered part REVERSIBLE, because
-- "we do not know what funded this" is not "it was settled".
--
-- ## What this does NOT do
--
-- It does not claw a derived lot back when a parent's funding reverses months
-- later. `credit.Service.Reverse` unwinds the FUNDING'S OWN lot and posts a
-- deficit against the payer; taking value back from a third party who earned it
-- is a posting kind this ledger does not have, and the earner may have spent it
-- or withdrawn it. What is expressible is applied -- a derived lot whose parent
-- is DISPUTED or REVERSED is moved to DISPUTED, which is neither spendable nor
-- payout-eligible -- and the rest is recorded as the residual in D-124 rather
-- than invented here.

CREATE TABLE credit_lot_parents (
    lot_id        uuid NOT NULL REFERENCES credit_lots(id),
    parent_lot_id uuid NOT NULL REFERENCES credit_lots(id),
    -- How much of the parent funded this lot. Documentary rather than
    -- load-bearing: the finality rule reads WHICH parents, not how much of
    -- each. It is here because a provenance record that cannot say how much is
    -- a record somebody will have to reconstruct.
    quantity      numeric(38,0) NOT NULL CHECK (quantity > 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (lot_id, parent_lot_id),
    -- A lot is not its own parent. Without this a single row makes the
    -- promotion pass below wait for itself for ever.
    CONSTRAINT credit_lot_parents_not_self CHECK (lot_id <> parent_lot_id)
);
CREATE INDEX credit_lot_parents_parent_idx ON credit_lot_parents (parent_lot_id);
CREATE TRIGGER credit_lot_parents_immutable BEFORE UPDATE OR DELETE ON credit_lot_parents
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

COMMENT ON TABLE credit_lot_parents IS
    'Which lots were consumed to fund a derived lot. Written at mint, append-only. A derived lot is minted at the least final finality among its parents and promoted by credit.Service.SettleDerived once every parent is payout-eligible (00809, F-230, D-124).';

CREATE TABLE native_market_credit_sources (
    id         uuid PRIMARY KEY,
    market_id  uuid NOT NULL REFERENCES native_markets(id),
    lot_id     uuid NOT NULL REFERENCES credit_lots(id),
    quantity   numeric(38,0) NOT NULL CHECK (quantity > 0),
    -- What of this contribution is still in the pool. The sell side draws it
    -- down in arrival order, so the rows that are still open are the reserve's
    -- real provenance rather than a running average of everything it ever saw.
    remaining  numeric(38,0) NOT NULL CHECK (remaining >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT native_market_credit_sources_remaining_fits CHECK (remaining <= quantity)
);
CREATE INDEX native_market_credit_sources_open_idx
    ON native_market_credit_sources (market_id, created_at, id) WHERE remaining > 0;

COMMENT ON TABLE native_market_credit_sources IS
    'The credit lots paid into a native market''s pooled reserve, drawn down in arrival order when credits leave it. It is what lets a sell''s proceeds name real parents instead of inheriting an average or a default (00809, F-230, D-124).';

GRANT SELECT, INSERT ON credit_lot_parents TO cp_app;
GRANT SELECT ON credit_lot_parents TO cp_readonly, cp_ops;
GRANT SELECT, INSERT ON native_market_credit_sources TO cp_app;
-- The draw-down, and nothing else. What the contribution WAS is written once.
GRANT UPDATE (remaining) ON native_market_credit_sources TO cp_app;
GRANT SELECT ON native_market_credit_sources TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: a lot's provenance is financial history and is never dropped by rollback
