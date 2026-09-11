-- +goose Up
-- A verification session, and the evidence it produced.
--
-- Goal §20 says prefer provider-hosted KYC and store a decision rather than a
-- document; §21 says age, country, state, sanctions, risk and provider
-- availability must each be a gate in their own right, "not faked with a
-- checkbox". `compliance_profiles` had one boolean for age and one enum for
-- sanctions, and no record at all of WHO decided, WHEN, under which rule
-- version, or whether the answer was real.
--
-- Two tables, and a rule about the sandbox.
--
-- ## verification_sessions -- one attempt at establishing identity
--
-- The status union is the one PROVIDER_BOUNDARY §3 derived from Persona,
-- Veriff, Sumsub and Stripe Identity together, so no single vendor's vocabulary
-- leaks into the schema. It carries the provider's reference and NOT its hosted
-- URL: those links are single-use, expire in minutes, and are a credential to
-- resume somebody else's identity check. Nodal returns one to the browser that
-- asked for it and stores none.
--
-- Its state column follows F-42 like every other one: a transitions table, an
-- edge binding, a trigger that writes the status, no UPDATE for `cp_app`, and a
-- birth control so a session cannot be INSERTed already APPROVED.
--
-- One open session per person, as a partial unique index. Two live sessions
-- means two answers arriving in an order nobody controls, and the later one
-- winning is not a decision, it is a race.
--
-- ## verification_checks -- the sub-checks, each with its own provenance
--
-- Append-only. One row per (session, check kind, answer), naming the provider,
-- the provider's reference, the rule version that judged it, and whether the
-- answer is a SANDBOX one. Five kinds, because §21 lists five things that can
-- independently refuse: the identity document, the age, the jurisdiction, the
-- sanctions screen and the PEP flag.
--
-- ## The sandbox rule, in the schema rather than only in the service
--
-- A sandbox outcome is a rehearsal. It exists so a non-production deployment
-- can drive the whole withdrawal journey without fabricating an approval
-- (ADR-0023), and it must be impossible for one to exist where real value could
-- move. So `sandbox` is a column, `environment` is a column, and a CHECK refuses
-- the combination: no credential, no configuration and no code path produces a
-- sandbox verification decision in PROD. The service refuses it too, on
-- `cfg.SandboxTier()`; this is the same refusal in the place that cannot be
-- redeployed around.
--
-- Custom SQLSTATE: AD001, as in 00735-00737, 00743-00744 and 00761.

CREATE TABLE verification_sessions (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL REFERENCES users(id),
    -- The level this session is trying to establish. It is stated up front
    -- because a provider is told the PURPOSE of a check before it runs one,
    -- and because "we verified you" is meaningless without "for what".
    purpose              text NOT NULL CHECK (purpose IN ('PAYOUT_KYC','ENHANCED')),
    provider             text NOT NULL,
    -- The provider's own identifier for this attempt. Null until the provider
    -- answers: the row is written BEFORE the provider is called, so a crash
    -- between the write and the call still leaves something to reconcile.
    provider_ref         text,
    status               text NOT NULL CHECK (status IN (
                             'CREATED','PENDING_USER_ACTION','PROCESSING','REQUIRES_INPUT',
                             'MANUAL_REVIEW','APPROVED','DECLINED','CANCELLED','EXPIRED')),
    -- Which jurisdiction's rules this session was judged under, and which
    -- version of them. A decision made in March must be explicable in June
    -- after the rules have changed twice.
    jurisdiction_country text,
    jurisdiction_region  text,
    rules_version        text NOT NULL,
    environment          text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    sandbox              boolean NOT NULL DEFAULT false,
    failure_reason       text,
    expires_at           timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_ref),
    -- The rule. A rehearsal cannot exist where real value moves.
    CONSTRAINT verification_sessions_sandbox_never_in_prod CHECK (NOT sandbox OR environment <> 'PROD')
);
CREATE INDEX verification_sessions_user_idx ON verification_sessions (user_id, created_at DESC);
-- One live attempt per person. Two answers arriving in an uncontrolled order is
-- a race, not a decision.
CREATE UNIQUE INDEX verification_sessions_one_open_per_user ON verification_sessions (user_id)
    WHERE status IN ('CREATED','PENDING_USER_ACTION','PROCESSING','REQUIRES_INPUT','MANUAL_REVIEW');
CREATE TRIGGER verification_sessions_updated_at BEFORE UPDATE ON verification_sessions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE verification_session_transitions (
    id             uuid PRIMARY KEY,
    session_id     uuid NOT NULL REFERENCES verification_sessions(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL CHECK (actor_type IN ('SYSTEM','OPERATOR','USER')),
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    provider_event text,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now(),
    CHECK (btrim(actor_id) <> ''),
    CHECK (btrim(reason) <> '')
);
CREATE INDEX verification_session_transitions_idx ON verification_session_transitions (session_id, occurred_at);
CREATE TRIGGER verification_session_transitions_immutable
    BEFORE UPDATE OR DELETE ON verification_session_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER verification_session_transitions_flag_edge
    AFTER INSERT ON verification_session_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('session_id', 'from_status', 'to_status', 'verification_sessions');

-- +goose StatementBegin
CREATE FUNCTION cp_verification_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE verification_sessions
       SET status = NEW.to_status
     WHERE id = NEW.session_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'VERIFICATION_TRANSITION_ORPHANED: verification_session_transitions names session % which does not exist',
            NEW.session_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER verification_session_transitions_writes_the_status
    AFTER INSERT ON verification_session_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_verification_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_verification_apply_status_transition() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER verification_sessions_require_transition
    AFTER UPDATE OF status ON verification_sessions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'verification_sessions');

-- +goose StatementBegin
CREATE FUNCTION cp_verification_session_born_created() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'CREATED' THEN
        RAISE EXCEPTION 'VERIFICATION_SESSION_BORN_DECIDED: a verification session is born CREATED and reaches % through a transition row',
            NEW.status USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER verification_sessions_born_created
    BEFORE INSERT ON verification_sessions
    FOR EACH ROW EXECUTE FUNCTION cp_verification_session_born_created();

-- ---------------------------------------------------------------------------
-- The evidence: one row per sub-check, append-only
-- ---------------------------------------------------------------------------

CREATE TABLE verification_checks (
    id            uuid PRIMARY KEY,
    session_id    uuid NOT NULL REFERENCES verification_sessions(id),
    user_id       uuid NOT NULL REFERENCES users(id),
    kind          text NOT NULL CHECK (kind IN ('IDENTITY_DOCUMENT','AGE','JURISDICTION','SANCTIONS','PEP')),
    outcome       text NOT NULL CHECK (outcome IN ('PASS','FAIL','NEEDS_INFORMATION','UNKNOWN','NOT_APPLICABLE')),
    provider      text NOT NULL,
    provider_ref  text,
    -- Which rule table judged this, so an age threshold or a jurisdiction list
    -- that changes later cannot silently rewrite an old decision.
    rules_version text NOT NULL,
    environment   text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    sandbox       boolean NOT NULL DEFAULT false,
    -- A safe reason code, never a document and never a date of birth. The
    -- provider holds the evidence; Nodal holds the conclusion (goal §20).
    detail        text NOT NULL DEFAULT '',
    recorded_at   timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT verification_checks_sandbox_never_in_prod CHECK (NOT sandbox OR environment <> 'PROD'),
    -- A check answer is about a person and a kind at a moment. Two identical
    -- answers to the same question in the same session are one answer.
    UNIQUE (session_id, kind, outcome)
);
CREATE INDEX verification_checks_user_idx ON verification_checks (user_id, kind, recorded_at DESC);
CREATE INDEX verification_checks_session_idx ON verification_checks (session_id, recorded_at);
CREATE TRIGGER verification_checks_immutable BEFORE UPDATE OR DELETE ON verification_checks
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON verification_sessions TO cp_app;
-- Not for writing the status. The provider reference arrives after the row is
-- written, the expiry is refreshed when a hosted link is reissued, and a row
-- lock needs an UPDATE privilege (00744).
GRANT UPDATE (provider_ref, expires_at, failure_reason) ON verification_sessions TO cp_app;
GRANT SELECT, INSERT ON verification_session_transitions, verification_checks TO cp_app;
GRANT SELECT ON verification_sessions, verification_session_transitions, verification_checks
    TO cp_readonly, cp_ops;

-- 00761 left session_id unconstrained so the two migrations were independently
-- applicable. The table exists now, so the reference is real.
ALTER TABLE compliance_profile_transitions
    ADD CONSTRAINT compliance_profile_transitions_session_fk
    FOREIGN KEY (session_id) REFERENCES verification_sessions(id);

COMMENT ON TABLE verification_checks IS
    'The sub-checks behind a verification decision: identity document, age, jurisdiction, sanctions, PEP. Append-only, each naming its provider, its provider reference, the rule version that judged it and whether it is a SANDBOX answer, which a CHECK refuses to be in PROD (00762, goal §21).';

-- +goose Down
SELECT 1; -- protected: verification evidence is compliance history and is never dropped by rollback
