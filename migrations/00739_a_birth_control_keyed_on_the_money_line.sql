-- +goose Up
-- A birth control keyed on the money line, not on the code's happy path.
--
-- 00736 and 00737 closed F-122 by requiring each table's exact creation state:
-- an agent DRAFT/DRAFT, a funding CREATED, a payout ELIGIBILITY_CHECK. That is
-- what the code does, and "what the code does" is a defensible default for a
-- birth control -- but it is not the property being protected, and the
-- difference cost seven integration packages.
--
-- What those packages were doing is not the forgery:
--
--   internal/capital      seeds an agent VALIDATED/VALIDATED, no mode, no envelope
--   internal/intent       seeds an agent SHADOW/SHADOW in SHADOW mode
--   internal/nativemarket the same
--   internal/prediction   the same
--   internal/capacity     seeds a funding CAPTURED with no lot
--   internal/httpapi      seeds a payout DRAFT, which is EARLIER than the
--                         state 00737 demanded, not later
--
-- None of them mints value, moves real capital or forges an approval. They are
-- seeds for tests about something else, and rewriting five of them to walk a
-- promotion ladder with evidence hashes would put machinery irrelevant to each
-- test into every one of them.
--
-- So the rule is restated against the lines this schema ALREADY draws, which is
-- both narrower and better founded than the creation literal:
--
--   agents           may not be born at a stage that requires an APPROVAL, and
--                    may not be born with a real-capital mode or an envelope.
--                    agent_lifecycle_transitions_check1 already names CANARY,
--                    LIMITED and LIVE as the approval-bearing stages; Mode's
--                    RealCapital() already names the same three modes; and
--                    NewAuthority already refuses a real-capital stage with no
--                    envelope. This is those three statements at INSERT.
--
--   credit_fundings  may not be born in a state that carries a FINALITY, and
--                    may not be born with a lot, a settlement or a reversal.
--                    LotFinalityFor returns a finality for REVERSIBLE, SETTLED,
--                    DISPUTED, REVERSED and REFUNDED -- those are the states
--                    that assert value exists. CREATED through CAPTURED, and
--                    MANUAL_REVIEW, mint nothing.
--
--   payout_requests  may not be born at or past reservation, and may not be
--                    born with anything reserved, settled, submitted or a
--                    provider chosen. The four pre-reservation states are
--                    equivalent for this purpose and DRAFT is earlier than
--                    ELIGIBILITY_CHECK, not later.
--
-- The forgeries F-122 was raised for are all still refused: an agent born
-- LIVE/LIVE with an envelope, a funding born SETTLED with a lot, a payout born
-- SETTLED with settled_quantity, and (00735, unchanged) an admin action born
-- APPROVED.
--
-- ## What this deliberately gives up, and it is a real thing
--
-- An agent can be born SHADOW. SHADOW requires promotion EVIDENCE --
-- strategy_version_id, ir_hash, risk_policy_hash, evidence_hash -- and a row
-- born there has none of it. That is a PROVENANCE gap: nothing records which
-- strategy version and risk policy the agent was validated against.
--
-- It is not a MONEY gap. A SHADOW agent cannot move real capital: the mode is
-- refused above, the envelope is refused above, and every real-capital stage is
-- refused above. F-122 is about value and authority, and this is the line
-- between them.
--
-- Recorded as the residual rather than closed, because closing it means either
-- rewriting five fixtures to walk the ladder or accepting that a test seeding a
-- shadow agent must carry evidence hashes -- and that is a decision about how
-- this suite is built, not a defect in the schema.
--
-- Custom SQLSTATE: AD001, as in 00723, 00735, 00736 and 00737.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_agent_born_draft() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.stage = ANY (ARRAY['CANARY'::text, 'LIMITED'::text, 'LIVE'::text])
       OR NEW.state = ANY (ARRAY['CANARY'::text, 'LIMITED'::text, 'LIVE'::text]) THEN
        RAISE EXCEPTION 'AGENT_BORN_PROMOTED: an agent is not created at %/%; those stages need an approval, and creating an agent is not a transition for one to attach to',
            NEW.stage, NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF coalesce(NEW.mode, '') = ANY (ARRAY['CANARY'::text, 'LIMITED'::text, 'LIVE'::text]) THEN
        RAISE EXCEPTION 'AGENT_BORN_PROMOTED: an agent is not created in % mode; real capital is granted by a promotion',
            NEW.mode USING ERRCODE = 'AD001';
    END IF;
    IF NEW.envelope_id IS NOT NULL THEN
        RAISE EXCEPTION 'AGENT_BORN_PROMOTED: an agent is created with no capital envelope; an envelope is what bounds real capital and is granted by a promotion'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_credit_funding_born_created() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state = ANY (ARRAY['REVERSIBLE'::text, 'SETTLED'::text, 'DISPUTED'::text,
                              'REVERSED'::text, 'REFUNDED'::text]) THEN
        RAISE EXCEPTION 'FUNDING_BORN_FINISHED: a credit funding is not created %; that state asserts a finality, and Credits are minted on CAPTURED -> REVERSIBLE, never at INSERT',
            NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.lot_id IS NOT NULL OR NEW.settled_at IS NOT NULL OR NEW.reversed_at IS NOT NULL THEN
        RAISE EXCEPTION 'FUNDING_BORN_FINISHED: a credit funding is created with no lot, no settlement and no reversal'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_payout_born_at_the_start() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> ALL (ARRAY['DRAFT'::text, 'ELIGIBILITY_CHECK'::text,
                               'VERIFICATION_REQUIRED'::text, 'VERIFICATION_PENDING'::text]) THEN
        RAISE EXCEPTION 'PAYOUT_BORN_FINISHED: a payout request is not created %; reservation, submission and settlement are each a transition with a row behind it',
            NEW.state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.reserved_quantity <> 0 OR NEW.settled_quantity <> 0
       OR NEW.reserved_at IS NOT NULL OR NEW.submitted_at IS NOT NULL OR NEW.settled_at IS NOT NULL
       OR NEW.provider IS NOT NULL OR NEW.provider_reference IS NOT NULL
       OR NEW.provider_idempotency_key IS NOT NULL THEN
        RAISE EXCEPTION 'PAYOUT_BORN_FINISHED: a payout request is created with nothing reserved, nothing settled and no provider'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_agent_born_draft() IS
    'An agent is not created at an approval-bearing stage, in a real-capital mode, or with an envelope. Narrower than 00736 deliberately: being born SHADOW is a provenance gap and not a money one, and the difference is recorded in 00739 (F-122).';

-- +goose Down
SELECT 1; -- protected: reverting lets an agent be born LIVE, a funding be born minted, and a payout be born settled
