-- +goose Up
-- Nothing is born finished.
--
-- Every state machine in this schema binds its CHANGES: 00603 bound a state
-- change to a transition row, 00726 and 00731 made that row name both endpoints,
-- 00732 stopped the endpoints being forgeable, and 00734 closed the exit from a
-- terminal state.
--
-- All of that is about UPDATE. A row that is INSERTED in a privileged state
-- never changed, so nothing bound it, and until 00735 `capability_gates` was the
-- only entity in the schema that could not be born decided. 00735 added
-- `admin_actions`, which was the sharpest because it is where dual control
-- lives. These are the remaining three where the birth state means money or
-- authority (F-122).
--
--   agents           can be born LIVE/LIVE with an envelope and no approval,
--                    skipping the entire promotion ladder at INSERT: every
--                    evidence CHECK on agent_lifecycle_transitions guards a
--                    transition, and creating an agent is not one
--   credit_fundings  can be born SETTLED with a lot_id, which is
--                    LotFinalityFor(SETTLED) -- value that is payout-eligible,
--                    minted from nothing
--   payout_requests  can be born SETTLED with settled_quantity set, satisfying
--                    the settled <= reserved <= requested CHECKs by naming all
--                    three
--
-- Each is created by exactly one statement in the tree, each with a LITERAL
-- birth state, so the constraint is what the code already does:
--
--   internal/agent/lifecycle.go   StageDraft / StateDraft, no envelope, no mode
--   internal/credit/funding.go    'CREATED', no lot
--   internal/payout/service.go    'ELIGIBILITY_CHECK', nothing reserved
--
-- CHECK constraints rather than triggers, unlike 00735: these are statements
-- about a row's contents that hold for its whole life, not just at INSERT.
-- A funding is never SETTLED without a lot; a payout is never in
-- ELIGIBILITY_CHECK with money already settled. Expressed as implications so
-- they say nothing about the states they do not name, and so the UPDATE path
-- that legitimately fills these columns is unaffected.
--
-- The agents one has to be a trigger: DRAFT is a legitimate destination for an
-- UPDATE (nothing is, today, but the state table does not forbid it), so the
-- rule is about creation rather than about the row.
--
-- Custom SQLSTATE: AD001, as in 00723 and 00735.

-- +goose StatementBegin
CREATE FUNCTION cp_agent_born_draft() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.stage <> 'DRAFT' OR NEW.state <> 'DRAFT' THEN
        RAISE EXCEPTION 'AGENT_BORN_PROMOTED: an agent is created DRAFT/DRAFT, not %/%; every rung of the ladder is a transition with evidence behind it',
            NEW.stage, NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.envelope_id IS NOT NULL OR coalesce(NEW.mode, '') <> '' THEN
        RAISE EXCEPTION 'AGENT_BORN_PROMOTED: an agent is created with no capital envelope and no mode; both are granted by a promotion'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER agents_born_draft
    BEFORE INSERT ON agents
    FOR EACH ROW EXECUTE FUNCTION cp_agent_born_draft();

COMMENT ON FUNCTION cp_agent_born_draft() IS
    'An agent is created DRAFT with no envelope and no mode. Every evidence and approval CHECK on agent_lifecycle_transitions guards a TRANSITION, and creating an agent is not one, so without this the whole promotion ladder was skippable at INSERT (F-122).';

-- A funding that has minted is REVERSIBLE or later, and one that has not has no
-- lot. Stated in both directions because each catches a different forgery: a
-- row born SETTLED with a lot mints payout-eligible value from nothing, and a
-- row claiming a settlement with no lot is a settlement of nothing.
ALTER TABLE credit_fundings
    ADD CONSTRAINT credit_fundings_lot_matches_state
    CHECK (
        (lot_id IS NULL) = (state = ANY (ARRAY[
            'CREATED'::text, 'AUTHORIZATION_PENDING'::text, 'AUTHORIZED'::text,
            'CAPTURE_PENDING'::text, 'CAPTURED'::text, 'FAILED'::text,
            'CANCELED'::text, 'MANUAL_REVIEW'::text
        ]))
    ) NOT VALID;

-- Money is only reserved or settled in the states that reserve or settle it.
ALTER TABLE payout_requests
    ADD CONSTRAINT payout_requests_money_matches_state
    CHECK (
        (state <> ALL (ARRAY['DRAFT'::text, 'ELIGIBILITY_CHECK'::text,
                             'VERIFICATION_REQUIRED'::text, 'VERIFICATION_PENDING'::text]))
        OR (reserved_quantity = 0 AND settled_quantity = 0
            AND reserved_at IS NULL AND submitted_at IS NULL AND settled_at IS NULL
            AND provider IS NULL AND provider_reference IS NULL)
    ) NOT VALID;

-- NOT VALID, then validated: the CHECK applies to every new row and every
-- update immediately, and VALIDATE takes a lighter lock than adding a validated
-- constraint outright. If an existing row violates one of these, the VALIDATE
-- fails loudly here rather than the constraint being silently unenforced --
-- which is the point of doing it in two steps rather than skipping validation.
ALTER TABLE credit_fundings VALIDATE CONSTRAINT credit_fundings_lot_matches_state;
ALTER TABLE payout_requests VALIDATE CONSTRAINT payout_requests_money_matches_state;

-- +goose Down
SELECT 1; -- protected: reverting lets an agent be born LIVE, a funding be born minted, and a payout be born settled
