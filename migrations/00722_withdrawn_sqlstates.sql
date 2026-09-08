-- +goose Up
-- Four SQLSTATEs that were documented and never raised are withdrawn, and the
-- mechanism that actually holds each invariant is recorded on the schema
-- object that holds it (F-58).
--
-- Withdrawn SQLSTATEs: CR002, CR005, PO002, PO004.
--
-- Each was named in a `-- Custom SQLSTATEs:` header and raised by nothing. The
-- invariants themselves all hold; a different mechanism enforces each, under a
-- different SQLSTATE:
--
--   CR002 "immutable row"          -> forbid_mutation() on credit_lots,
--                                     credit_lot_events and
--                                     credit_funding_transitions; raises P0001.
--   CR005 "more than one Credit"   -> the partial unique index
--                                     assets_single_credit_asset; raises 23505.
--   PO002 "illegal state change"   -> the state CHECK on payout_requests plus
--                                     the AU001 transition binding installed by
--                                     cp_require_transition('state'); raises
--                                     23514 and AU001 respectively.
--   PO004 "amount mismatch"        -> the settled <= reserved <= requested
--                                     CHECKs on payout_requests; raises 23514.
--
-- Withdrawing rather than implementing is the deliberate choice. Making CR002
-- real means replacing forbid_mutation on three live tables with a
-- credit-specific function, which changes what db.IsImmutableRow sees for those
-- tables to buy a more specific name for a refusal that is already correct and
-- already tested. Making PO002 real means a second copy of a state machine the
-- CHECK and the transition binding already enforce, and a second copy is a
-- second thing to drift. The cost of the fiction was never a missing control;
-- it was that an application could not tell these refusals apart from any other
-- unique or check violation, and that is a smaller problem than the one the
-- implementations would create.
--
-- What makes this a correction rather than an excuse is the pair of controls
-- landing with it: `TestIntegration_EveryDocumentedSQLStateIsRaised` fails on a
-- documented code that is neither raised nor withdrawn here, and
-- `TestIntegration_TheWithdrawnInvariantsAreStillEnforced` drives all four
-- invariants and watches each refusal happen. A withdrawal that was really an
-- abandonment would fail the second one.
--
-- The mechanisms are recorded on the objects themselves, so `\d+` and a schema
-- dump carry the correction rather than a comment in a file nobody greps.

COMMENT ON INDEX assets_single_credit_asset IS
    'Enforces what 00711 documented as CR005 (more than one Credit asset). Raises 23505, not CR005; that code was withdrawn by 00722.';

COMMENT ON TRIGGER credit_lots_immutable ON credit_lots IS
    'Enforces what 00711 documented as CR002 (immutable row). Raises P0001 through forbid_mutation(); CR002 was withdrawn by 00722.';
COMMENT ON TRIGGER credit_lot_events_immutable ON credit_lot_events IS
    'Enforces what 00711 documented as CR002 (immutable row). Raises P0001 through forbid_mutation(); CR002 was withdrawn by 00722.';
COMMENT ON TRIGGER credit_funding_transitions_immutable ON credit_funding_transitions IS
    'Enforces what 00711 documented as CR002 (immutable row). Raises P0001 through forbid_mutation(); CR002 was withdrawn by 00722.';

COMMENT ON TABLE payout_requests IS
    'Payout requests. Two codes 00713 documented are enforced here by other means and were withdrawn by 00722: PO002 (illegal state transition) by the state CHECK plus the AU001 transition binding, and PO004 (amount mismatch) by the settled <= reserved <= requested CHECKs.';

-- +goose Down
SELECT 1; -- protected: the comments are the record of a correction, not decoration
