-- +goose Up
-- A funding and a payout are born at the start.
--
-- 00736 added two CHECK constraints saying that a funding's lot and a payout's
-- money agree with its state. Both are true and neither closes the forgery they
-- were reached for, which is worth writing down rather than quietly fixing.
--
-- `credit_fundings_lot_matches_state` says a SETTLED funding HAS a lot. So a
-- row born SETTLED **with** a lot satisfies it. The constraint refuses a
-- settlement of nothing; it does not refuse a settlement minted from nothing.
--
-- `payout_requests_money_matches_state` names the four pre-reservation states.
-- A row born SETTLED is not one of them, so the constraint says nothing about
-- it, and `settled <= reserved <= requested` is satisfied by naming all three.
--
-- The rule that closes both is the one 00735 used for `admin_actions` and 00736
-- used for `agents`: the birth state itself. Each of these tables is created by
-- exactly one statement in the tree, and each names a literal:
--
--   internal/credit/funding.go   'CREATED'
--   internal/payout/service.go   'ELIGIBILITY_CHECK'
--
-- So the constraint is what the code already does, and everything after it is a
-- transition with a row behind it.
--
-- A trigger rather than a CHECK because these are statements about CREATION.
-- CREATED and ELIGIBILITY_CHECK are both legitimate destinations for an UPDATE
-- as far as the state tables are concerned, and a CHECK cannot tell the two
-- apart (F-122).
--
-- Custom SQLSTATE: AD001, as in 00723, 00735 and 00736.

-- +goose StatementBegin
CREATE FUNCTION cp_credit_funding_born_created() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'CREATED' THEN
        RAISE EXCEPTION 'FUNDING_BORN_FINISHED: a credit funding is created CREATED, not %; a capture, a settlement and a reversal are each a transition with a row behind it',
            NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.lot_id IS NOT NULL OR NEW.settled_at IS NOT NULL OR NEW.reversed_at IS NOT NULL THEN
        RAISE EXCEPTION 'FUNDING_BORN_FINISHED: a credit funding is created with no lot, no settlement and no reversal; Credits are minted on CAPTURED -> REVERSIBLE, never at INSERT'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_fundings_born_created
    BEFORE INSERT ON credit_fundings
    FOR EACH ROW EXECUTE FUNCTION cp_credit_funding_born_created();

-- +goose StatementBegin
CREATE FUNCTION cp_payout_born_at_the_start() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'ELIGIBILITY_CHECK' THEN
        RAISE EXCEPTION 'PAYOUT_BORN_FINISHED: a payout request is created ELIGIBILITY_CHECK, not %; verification, reservation, submission and settlement are each a transition with a row behind it',
            NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.reserved_quantity <> 0 OR NEW.settled_quantity <> 0
       OR NEW.reserved_at IS NOT NULL OR NEW.submitted_at IS NOT NULL OR NEW.settled_at IS NOT NULL
       OR NEW.provider IS NOT NULL OR NEW.provider_reference IS NOT NULL
       OR NEW.provider_idempotency_key IS NOT NULL THEN
        RAISE EXCEPTION 'PAYOUT_BORN_FINISHED: a payout request is created with nothing reserved, nothing settled and no provider; a provider is chosen when it is submitted'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER payout_requests_born_at_the_start
    BEFORE INSERT ON payout_requests
    FOR EACH ROW EXECUTE FUNCTION cp_payout_born_at_the_start();

COMMENT ON FUNCTION cp_credit_funding_born_created() IS
    'A credit funding is created CREATED with no lot. 00736 asserted a SETTLED funding HAS a lot, which a row born SETTLED with one satisfies; the birth state is what refuses minting payout-eligible value from nothing (F-122).';
COMMENT ON FUNCTION cp_payout_born_at_the_start() IS
    'A payout request is created ELIGIBILITY_CHECK with nothing reserved. 00736 named only the pre-reservation states, so a row born SETTLED escaped it by not being one of them (F-122).';

-- +goose Down
SELECT 1; -- protected: reverting lets a funding be born minted and a payout be born settled
