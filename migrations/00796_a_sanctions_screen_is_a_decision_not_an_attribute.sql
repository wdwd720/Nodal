-- +goose Up
-- A sanctions screen is a decision, not an attribute.
--
-- 00761 stated the rule it was enforcing: "an application statement could move a
-- profile from UNVERIFIED to VERIFIED with no recorded edge, no actor and no
-- provider reference, and the trail a regulator would read was whatever the last
-- writer said." It fixed that for `identity_state` with a transition table, an
-- edge binding, a SECURITY DEFINER trigger that writes the column, and -- the
-- part that enforces it -- `REVOKE UPDATE ON compliance_profiles FROM cp_app`.
--
-- The very next statement granted `UPDATE (… sanctions_state …)` back.
--
-- `sanctions_state` is not an attribute. `internal/eligibility/evaluate.go`
-- reads it as one of the allowlists that decides whether a payout may proceed,
-- and `verification.sanctionsStateFrom` derives it from the provider's sanctions
-- and PEP checks. It is a screening DECISION with exactly the properties 00761
-- gave as the reason `identity_state` had to move: one UPDATE changes it, and
-- nothing in the database records who, when, on what evidence, or from which
-- value. (F-168.)
--
-- ## What this does
--
-- 1. The screen rides on the transition row that already exists.
--
-- `compliance_profile_transitions` gains `from_sanctions_state` and
-- `to_sanctions_state`. Both NULL means "this transition says nothing about the
-- screen", which is what an ordinary verification edge says. Both set means the
-- row IS the screening decision, and it carries the actor, the reason, the
-- provider, the provider reference and the session that every other decision on
-- this table carries.
--
-- A second table was the alternative and was not taken: a screening decision and
-- a verification decision are made by the same provider answer, recorded by the
-- same writer, in the same transaction, and read by the same regulator. Two
-- tables would be two trails to join and two places to forget.
--
-- 2. The trigger writes the column and nothing else can.
--
-- `cp_compliance_apply_state_transition` is replaced to carry the screen across
-- as well, and the column grant loses `sanctions_state`. A second constraint
-- trigger refuses any change to the column that no transition row describes --
-- the same belt-and-braces 00761 put on `identity_state`, which matters because
-- a grant is one statement away from being handed back and a constraint is not.
--
-- The replacement also stops a same-state transition ERASING the standing
-- verification timestamps: `verified_at` and `expires_at` now fall back to what
-- the profile already holds when the row does not carry them. Before this, a row
-- that said only "the screen moved" and repeated the current state would have
-- rewritten `verified_at` to its own instant and blanked `expires_at`. No such
-- row could exist before this migration; this is the migration that creates them.
--
-- 3. A profile born already screened records that too.
--
-- `compliance_profiles` is INSERTed with a screen in it: `compliance.Upsert`
-- creates the profile from a provider's first answer. A row born holding HIT
-- would otherwise be a screening decision with no record, which is the whole
-- finding wearing an INSERT instead of an UPDATE. So the birth of a profile
-- whose screen is not the default writes its own transition row, from UNVERIFIED
-- to UNVERIFIED -- the state a profile is born in and has not left -- carrying
-- `UNKNOWN → <the screen it was born with>`.
--
-- A profile born UNKNOWN writes nothing: there is no decision to record, and a
-- row for every profile ever created would be noise in the one table a person
-- reads to find out what was decided about them.
--
-- ## What this deliberately does NOT do
--
-- It does not touch a Credit, a lot, a balance or a value domain, for the reason
-- 00761 gives: a screening decision changes what a person may ASK for, never
-- what their Credits ARE (PROVIDER_BOUNDARY §2 rule 1).
--
-- Custom SQLSTATE: AD001, as in 00761.

-- A UUIDv7, because the application reads every id in this schema as one.
--
-- The birth trigger below writes a row the application did not write, so it
-- needs an id the application can read back: internal/id.Parse accepts only an
-- RFC 9562 version 7 UUID and refuses everything else, which is how F-131 was
-- found -- a fixture wrote v4 ids into a column the code reads as a typed v7 id
-- and created rows this system could write and could not read. PostgreSQL 16
-- has no uuidv7(), so this is the RFC's layout by hand: 48 bits of unix
-- milliseconds, version 7, variant 10, the rest random.
-- +goose StatementBegin
CREATE FUNCTION cp_uuid_v7() RETURNS uuid
LANGUAGE plpgsql VOLATILE
SET search_path = public, pg_temp
AS $$
DECLARE
    b bytea;
BEGIN
    b := substring(int8send((floor(extract(epoch FROM clock_timestamp()) * 1000))::bigint) FROM 3)
         || gen_random_bytes(10);
    b := set_byte(b, 6, 112 | (get_byte(b, 6) & 15));  -- version 7 in the high nibble
    b := set_byte(b, 8, 128 | (get_byte(b, 8) & 63));  -- variant 10xxxxxx
    RETURN encode(b, 'hex')::uuid;
END;
$$;
-- +goose StatementEnd

COMMENT ON FUNCTION cp_uuid_v7() IS
    'An RFC 9562 version 7 UUID. internal/id refuses every other version, so a row the database writes for itself needs this rather than gen_random_uuid (00796, and the shape of F-131).';

ALTER TABLE compliance_profile_transitions
    ADD COLUMN from_sanctions_state text,
    ADD COLUMN to_sanctions_state   text;

ALTER TABLE compliance_profile_transitions
    ADD CONSTRAINT compliance_profile_transitions_from_sanctions_state_check
        CHECK (from_sanctions_state IS NULL
               OR from_sanctions_state IN ('UNKNOWN','CLEAR','HIT','REVIEW')),
    ADD CONSTRAINT compliance_profile_transitions_to_sanctions_state_check
        CHECK (to_sanctions_state IS NULL
               OR to_sanctions_state IN ('UNKNOWN','CLEAR','HIT','REVIEW')),
    -- Half an edge describes nothing. A row that names where the screen ended
    -- without naming where it started is the "no recorded edge" this migration
    -- exists to close, written more carefully.
    ADD CONSTRAINT compliance_profile_transitions_sanctions_edge_is_whole
        CHECK ((from_sanctions_state IS NULL) = (to_sanctions_state IS NULL));

COMMENT ON COLUMN compliance_profile_transitions.to_sanctions_state IS
    'The sanctions screen this decision reached, or NULL when the row says nothing about the screen. Written onto compliance_profiles by cp_compliance_apply_state_transition; cp_app holds no UPDATE on that column (00796, F-168).';

-- The edge flag the constraint trigger below reads. Guarded by a WHEN clause
-- rather than inside the function, because cp_flag_transition_edge builds
-- `<from>>` `<to>` by concatenation and a NULL endpoint would flag the literal
-- NULL that a row saying nothing about the screen means.
CREATE TRIGGER compliance_profile_transitions_flag_sanctions_edge
    AFTER INSERT ON compliance_profile_transitions
    FOR EACH ROW WHEN (NEW.to_sanctions_state IS NOT NULL)
    EXECUTE FUNCTION cp_flag_transition_edge('user_id', 'from_sanctions_state', 'to_sanctions_state', 'compliance_profiles_sanctions');

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_compliance_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE compliance_profiles
       SET identity_state = NEW.to_state,
           -- Set on the way in; cleared when a decision stops being one. A
           -- profile that is no longer VERIFIED must not keep a verified_at
           -- that a reader would take for a current fact. A row that does not
           -- carry one leaves the standing value alone, so a transition about
           -- the SCREEN does not rewrite when the person was verified.
           verified_at = CASE WHEN NEW.to_state = 'VERIFIED'
                              THEN coalesce(NEW.verified_at, verified_at, NEW.occurred_at)
                              ELSE NULL END,
           expires_at  = CASE WHEN NEW.to_state = 'VERIFIED'
                              THEN coalesce(NEW.expires_at, expires_at)
                              ELSE expires_at END,
           -- The screening decision, when the row carries one.
           sanctions_state = coalesce(NEW.to_sanctions_state, sanctions_state)
     WHERE user_id = NEW.user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'COMPLIANCE_TRANSITION_ORPHANED: compliance_profile_transitions names user % which has no profile',
            NEW.user_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER compliance_profiles_require_sanctions_transition
    AFTER UPDATE OF sanctions_state ON compliance_profiles
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('sanctions_state', 'compliance_profiles_sanctions', 'user_id');

-- A birth that carries a screen records it. SECURITY DEFINER because the row it
-- writes belongs to the audit trail rather than to the caller: a role that may
-- create a profile records the decision that profile was born holding, whether
-- or not it holds INSERT on the transitions table.
-- +goose StatementBegin
CREATE FUNCTION cp_compliance_profile_birth_screen() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.sanctions_state IS NOT DISTINCT FROM 'UNKNOWN' THEN
        -- Nothing was decided. A profile that has not been screened is the
        -- ordinary case and does not need a row saying so.
        RETURN NULL;
    END IF;
    INSERT INTO compliance_profile_transitions
        (id, user_id, from_state, to_state, from_sanctions_state, to_sanctions_state,
         actor_type, actor_id, reason, occurred_at)
    VALUES (cp_uuid_v7(), NEW.user_id, NEW.identity_state, NEW.identity_state,
            'UNKNOWN', NEW.sanctions_state,
            'SYSTEM', 'compliance:profile-created',
            'the profile was created already carrying a sanctions screening result',
            now());
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER compliance_profiles_birth_screen
    AFTER INSERT ON compliance_profiles
    FOR EACH ROW EXECUTE FUNCTION cp_compliance_profile_birth_screen();

REVOKE EXECUTE ON FUNCTION cp_compliance_profile_birth_screen() FROM PUBLIC;

-- The attribute half of the profile, as 00761 left it minus the screen. The
-- grant is also what permits the row lock `compliance.Upsert` takes before
-- writing it: SELECT ... FOR UPDATE requires UPDATE privilege, and a table-level
-- REVOKE with no column grant refuses all four lock strengths (00744 probed
-- this). Revoking the table privilege drops the column grants with it, so the
-- list below is the whole of what cp_app may write.
REVOKE UPDATE ON compliance_profiles FROM cp_app;
GRANT UPDATE (age_verified, jurisdiction_country, jurisdiction_region, residency_country,
              provider, provider_ref, policy_version, restrictions)
    ON compliance_profiles TO cp_app;

COMMENT ON FUNCTION cp_compliance_apply_state_transition() IS
    'Writes compliance_profiles.identity_state, verified_at, expires_at and sanctions_state from the transition row. cp_app holds UPDATE on the attribute columns only, so inserting the transition row is the only way a verification state or a sanctions screen changes (00761, 00796, F-42, F-168).';
COMMENT ON FUNCTION cp_compliance_profile_birth_screen() IS
    'A profile born carrying a sanctions screen records that decision as a transition row. Without it the screen could reach HIT or CLEAR by INSERT, which no binding about CHANGES applies to (00796, F-168, and the shape of F-122).';

-- +goose Down
SELECT 1; -- protected: reverting returns the sanctions screening decision to the application's reach
