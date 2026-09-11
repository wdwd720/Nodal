-- +goose Up
-- A verification state is not the application's to write.
--
-- `compliance_profiles.identity_state` is the state the product goal calls the
-- FINANCIAL VERIFICATION STATE MACHINE (§20). Until now it was five states with
-- no transition table, written by `internal/compliance.Upsert` as an ordinary
-- column: an application statement could move a profile from UNVERIFIED to
-- VERIFIED with no recorded edge, no actor and no provider reference, and the
-- trail a regulator would read was whatever the last writer said.
--
-- This migration does three things and deliberately no more.
--
-- ## 1. The states §20 actually names
--
-- The CHECK grows from five to ten. `UNVERIFIED` keeps its name rather than
-- becoming §20's `NOT_STARTED`: rows already carry it, `internal/eligibility`
-- policies allowlist it by name, and renaming a value to match a document is
-- the kind of churn that breaks a policy document somebody wrote last month.
-- The mapping is recorded in D-057.
--
--   UNVERIFIED         nothing has been asked of this person (§20 NOT_STARTED)
--   REQUIRED           something the person wants needs verification
--   STARTED            a provider session exists and the person has not finished
--   PENDING            the provider is deciding
--   NEEDS_INFORMATION  the provider asked for something more
--   VERIFIED           the provider decided yes
--   REJECTED           the provider decided no
--   EXPIRED            a decision that has aged out of its validity window
--   RESTRICTED         verified, and something (jurisdiction, sanctions) limits it
--   SUSPENDED          an operator stopped it pending review
--
-- ## 2. The transition table, and the trigger that writes the state (F-42)
--
-- Exactly 00744's shape: an AFTER INSERT trigger on the transitions table
-- writes `identity_state`, `verified_at` and `expires_at` from the row, and
-- `cp_app` loses UPDATE on all three. The trigger name ends in
-- `_writes_the_state` so it sorts after both flag setters ('w' after 'f'),
-- which matters under `SET CONSTRAINTS ALL IMMEDIATE`.
--
-- A column grant is kept, and NOT for writing: `SELECT ... FOR UPDATE` requires
-- UPDATE privilege on the table, and `internal/compliance.Upsert` locks the row
-- before writing the attribute half of the profile. The columns granted are
-- exactly the attributes the application legitimately owns -- age, jurisdiction,
-- residency, sanctions, provider, policy version, restrictions -- and the three
-- state-bearing columns are not among them.
--
-- ## 3. Nothing is born verified (F-122)
--
-- Every binding above is about CHANGES. A row INSERTED as VERIFIED never
-- changed, so none of it would apply -- and a profile born VERIFIED is the whole
-- control defeated in one statement. A BEFORE INSERT trigger refuses any birth
-- state but UNVERIFIED, which is why `Upsert` now inserts that literal and
-- transitions afterwards.
--
-- ## What this deliberately does NOT do
--
-- It does not touch a Credit, a lot, a balance or a value domain. A verification
-- decision changes what a person may ASK for; it never changes what their
-- Credits ARE (PROVIDER_BOUNDARY §2 rule 1). There is no path from this table to
-- `credit_lots`, and there is no column here that any Credit writer reads.
--
-- Custom SQLSTATE: AD001, as in 00735-00737 and 00743-00744.

ALTER TABLE compliance_profiles DROP CONSTRAINT compliance_profiles_identity_state_check;
ALTER TABLE compliance_profiles ADD CONSTRAINT compliance_profiles_identity_state_check
    CHECK (identity_state IN (
        'UNVERIFIED','REQUIRED','STARTED','PENDING','NEEDS_INFORMATION',
        'VERIFIED','REJECTED','EXPIRED','RESTRICTED','SUSPENDED'));

CREATE TABLE compliance_profile_transitions (
    id             uuid PRIMARY KEY,
    user_id        uuid NOT NULL REFERENCES users(id),
    from_state     text NOT NULL,
    to_state       text NOT NULL,
    actor_type     text NOT NULL CHECK (actor_type IN ('SYSTEM','OPERATOR')),
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    -- The provider that produced the decision, and its own reference for it.
    -- Nodal stores a decision and a reference; it never stores a document, an
    -- SSN or a date of birth (goal §20, PROVIDER_BOUNDARY §1 role B).
    provider       text,
    provider_ref   text,
    -- The verification session this decision came from, when it came from one.
    -- Populated by 00762, which creates the table it points at; kept
    -- unconstrained here so the two migrations are independently applicable.
    session_id     uuid,
    -- Written onto the profile by the trigger below, so that the moment a
    -- verification became true, and the moment it stops being true, are both
    -- properties of the change rather than of a later UPDATE.
    verified_at    timestamptz,
    expires_at     timestamptz,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    -- A customer never attests their own compliance state. The application
    -- refuses USER, AGENT and SERVICE actors as well; this is the same rule
    -- said where it cannot be forgotten.
    CHECK (btrim(actor_id) <> ''),
    CHECK (btrim(reason) <> '')
);
CREATE INDEX compliance_profile_transitions_idx ON compliance_profile_transitions (user_id, occurred_at);
CREATE TRIGGER compliance_profile_transitions_immutable
    BEFORE UPDATE OR DELETE ON compliance_profile_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER compliance_profile_transitions_flag_edge
    AFTER INSERT ON compliance_profile_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('user_id', 'from_state', 'to_state', 'compliance_profiles');

-- +goose StatementBegin
CREATE FUNCTION cp_compliance_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE compliance_profiles
       SET identity_state = NEW.to_state,
           -- Set on the way in; cleared when a decision stops being one. A
           -- profile that is no longer VERIFIED must not keep a verified_at
           -- that a reader would take for a current fact.
           verified_at = CASE WHEN NEW.to_state = 'VERIFIED' THEN coalesce(NEW.verified_at, NEW.occurred_at)
                              ELSE NULL END,
           expires_at  = CASE WHEN NEW.to_state = 'VERIFIED' THEN NEW.expires_at
                              ELSE expires_at END
     WHERE user_id = NEW.user_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'COMPLIANCE_TRANSITION_ORPHANED: compliance_profile_transitions names user % which has no profile',
            NEW.user_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER compliance_profile_transitions_writes_the_state
    AFTER INSERT ON compliance_profile_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_compliance_apply_state_transition();

REVOKE EXECUTE ON FUNCTION cp_compliance_apply_state_transition() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER compliance_profiles_require_transition
    AFTER UPDATE OF identity_state ON compliance_profiles
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('identity_state', 'compliance_profiles', 'user_id');

-- +goose StatementBegin
CREATE FUNCTION cp_compliance_profile_born_unverified() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.identity_state IS DISTINCT FROM 'UNVERIFIED' THEN
        RAISE EXCEPTION 'COMPLIANCE_PROFILE_BORN_VERIFIED: a compliance profile is born UNVERIFIED and reaches % through a transition row, never by being inserted in it',
            NEW.identity_state USING ERRCODE = 'AD001';
    END IF;
    IF NEW.verified_at IS NOT NULL THEN
        RAISE EXCEPTION 'COMPLIANCE_PROFILE_BORN_VERIFIED: a compliance profile cannot be born with a verified_at'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER compliance_profiles_born_unverified
    BEFORE INSERT ON compliance_profiles
    FOR EACH ROW EXECUTE FUNCTION cp_compliance_profile_born_unverified();

REVOKE UPDATE ON compliance_profiles FROM cp_app;
-- The attribute half of the profile, which the application does own. It is also
-- what permits the row lock `Upsert` takes before writing it: SELECT ... FOR
-- UPDATE requires UPDATE privilege, and a table-level REVOKE with no column
-- grant refuses all four lock strengths (00744 probed this).
GRANT UPDATE (age_verified, jurisdiction_country, jurisdiction_region, residency_country,
              sanctions_state, provider, provider_ref, policy_version, restrictions)
    ON compliance_profiles TO cp_app;

GRANT SELECT, INSERT ON compliance_profile_transitions TO cp_app;
GRANT SELECT ON compliance_profile_transitions TO cp_readonly, cp_ops;

COMMENT ON FUNCTION cp_compliance_apply_state_transition() IS
    'Writes compliance_profiles.identity_state, verified_at and expires_at from the transition row. cp_app holds UPDATE on the attribute columns only, so inserting the transition row is the only way a verification state changes (00761, F-42).';
COMMENT ON FUNCTION cp_compliance_profile_born_unverified() IS
    'A compliance profile is born UNVERIFIED. Without this a profile INSERTED as VERIFIED never changes state, so no binding applies to it (00761, F-122).';

-- +goose Down
SELECT 1; -- protected: reverting returns the verification state column to the application's reach
