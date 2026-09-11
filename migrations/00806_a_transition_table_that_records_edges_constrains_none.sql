-- +goose Up
-- A transition table that records edges constrains none.
--
-- 00761 and 00762 gave `compliance_profiles.identity_state` and
-- `verification_sessions.status` the F-42 treatment: a transitions table, an
-- edge binding (00731/00741), a SECURITY DEFINER trigger that writes the
-- column, and no UPDATE for `cp_app`. VERIFICATION_AND_WITHDRAWAL.md §4 then
-- says of the result:
--
--   "The absence that matters: nothing reaches VERIFIED except from PENDING
--    (a provider decided), from RESTRICTED (a limitation was lifted) or from
--    SUSPENDED ... that is a property of the transition table rather than of
--    the code that reads it."
--
-- It was a property of `internal/verification.CanTransition` and of nothing
-- else. The edge binding asks whether a transition row exists naming the state
-- the row really is in; it never asks whether the edge that row describes is
-- one the state machine has. So one INSERT, as `cp_app`, committed:
--
--     INSERT INTO compliance_profile_transitions
--         (id, user_id, from_state, to_state, from_sanctions_state, to_sanctions_state, ...)
--     VALUES (..., 'UNVERIFIED', 'VERIFIED', 'UNKNOWN', 'CLEAR', ...);
--
-- and the trigger wrote `identity_state = VERIFIED`, a `verified_at`, an
-- `expires_at` and a CLEAR sanctions screen from it, with no provider, no
-- session and no decision. `verification_session_transitions` was the same
-- shape: `CREATED -> APPROVED` in one row, for a session the provider had never
-- been called for (F-224/F-wv-7).
--
-- ## The remedy: the edge set becomes a table the database can read
--
-- Two tables, `compliance_profile_state_edges` and
-- `verification_session_status_edges`, populated here from
-- `verification.stateTransitions` and `verification.sessionTransitions`, and
-- both apply functions now refuse a row whose edge is not in them (AD001).
--
-- The Go lists stay authoritative and `test/integration/enums` holds the two
-- identical -- exactly how `AllStates()` is held to the CHECK, and for the same
-- reason: a list kept in two languages diverges, and the divergence here would
-- be an edge the code walks and the database refuses, or worse, one the
-- database licenses and the code has no meaning for.
--
-- Nothing may write the edge tables but `cp_migrate`. An edge set the
-- application can add a row to is not a constraint; the default privileges in
-- this schema hand SELECT to `cp_readonly` and `cp_ops` without a GRANT being
-- written, so the REVOKE is explicit (00741 learned that the hard way).
--
-- A row whose `from_state` equals its `to_state` is left legal. 00796's birth
-- screen writes exactly one -- `UNVERIFIED -> UNVERIFIED` carrying the sanctions
-- result a profile was created holding -- and it is not a state change at all.
-- The residual is stated in D-121: a same-state row can still carry a screening
-- decision, because the sanctions screen has no edge table of its own in either
-- language yet.
--
-- ## verification_checks: evidence has to attach to something
--
-- The third half of the reproduction did not need a state change. `cp_app`
-- holds INSERT on `verification_checks`, so four PASS rows against a session
-- nobody had answered made `verification.Resolver.Level` report PAYOUT_KYC. The
-- edge tables above close the route that gets such a session to APPROVED; this
-- closes the route that hangs evidence off one that never got there.
--
-- A check row must now name the session's own provider, and the session must be
-- in a status only a PROVIDER ANSWER produces: PROCESSING, REQUIRES_INPUT,
-- MANUAL_REVIEW, APPROVED, DECLINED. The four excluded statuses are the ones no
-- provider verdict can explain -- CREATED (the row written before the call),
-- PENDING_USER_ACTION (a link is out and nothing has come back), CANCELLED and
-- EXPIRED (the attempt lapsed without a decision).
--
-- Residual, stated rather than hidden: a session that HAS been answered can
-- still be given further check rows by `cp_app`, and on a sandbox tier a
-- rehearsal can reach APPROVED. Closing that needs the `capability_gates`
-- treatment -- `REVOKE INSERT ON verification_checks FROM cp_app` and a SECURITY
-- DEFINER `cp_verification_record_check()` that the repository calls -- which is
-- a change to the one call site and is recorded in D-121 as the next step
-- rather than done here, because it is a privilege change to evidence rows that
-- wants its own migration and its own exploit test.
--
-- Custom SQLSTATE: AD001, as in 00761, 00762 and 00796.

CREATE TABLE compliance_profile_state_edges (
    from_state text NOT NULL,
    to_state   text NOT NULL,
    PRIMARY KEY (from_state, to_state)
);

-- verification.StateEdges(), in declaration order. §20's machine: nothing
-- reaches VERIFIED except from PENDING, RESTRICTED or SUSPENDED.
INSERT INTO compliance_profile_state_edges (from_state, to_state) VALUES
    ('UNVERIFIED','REQUIRED'),
    ('UNVERIFIED','STARTED'),
    ('UNVERIFIED','SUSPENDED'),
    ('REQUIRED','STARTED'),
    ('REQUIRED','SUSPENDED'),
    ('STARTED','PENDING'),
    ('STARTED','NEEDS_INFORMATION'),
    ('STARTED','REJECTED'),
    ('STARTED','REQUIRED'),
    ('STARTED','SUSPENDED'),
    ('PENDING','VERIFIED'),
    ('PENDING','REJECTED'),
    ('PENDING','NEEDS_INFORMATION'),
    ('PENDING','RESTRICTED'),
    ('PENDING','SUSPENDED'),
    ('NEEDS_INFORMATION','STARTED'),
    ('NEEDS_INFORMATION','PENDING'),
    ('NEEDS_INFORMATION','REJECTED'),
    ('NEEDS_INFORMATION','SUSPENDED'),
    ('VERIFIED','EXPIRED'),
    ('VERIFIED','RESTRICTED'),
    ('VERIFIED','REJECTED'),
    ('VERIFIED','SUSPENDED'),
    ('REJECTED','REQUIRED'),
    ('REJECTED','SUSPENDED'),
    ('EXPIRED','REQUIRED'),
    ('EXPIRED','STARTED'),
    ('EXPIRED','SUSPENDED'),
    ('RESTRICTED','VERIFIED'),
    ('RESTRICTED','REJECTED'),
    ('RESTRICTED','EXPIRED'),
    ('RESTRICTED','SUSPENDED'),
    ('SUSPENDED','VERIFIED'),
    ('SUSPENDED','RESTRICTED'),
    ('SUSPENDED','REJECTED'),
    ('SUSPENDED','REQUIRED');

CREATE TABLE verification_session_status_edges (
    from_status text NOT NULL,
    to_status   text NOT NULL,
    PRIMARY KEY (from_status, to_status)
);

-- verification.SessionEdges(), in declaration order. A session nobody was sent
-- to cannot have been decided, so CREATED reaches no verdict.
INSERT INTO verification_session_status_edges (from_status, to_status) VALUES
    ('CREATED','PENDING_USER_ACTION'),
    ('CREATED','CANCELLED'),
    ('CREATED','EXPIRED'),
    ('PENDING_USER_ACTION','PROCESSING'),
    ('PENDING_USER_ACTION','REQUIRES_INPUT'),
    ('PENDING_USER_ACTION','CANCELLED'),
    ('PENDING_USER_ACTION','EXPIRED'),
    ('PROCESSING','APPROVED'),
    ('PROCESSING','DECLINED'),
    ('PROCESSING','REQUIRES_INPUT'),
    ('PROCESSING','MANUAL_REVIEW'),
    ('PROCESSING','EXPIRED'),
    ('REQUIRES_INPUT','PENDING_USER_ACTION'),
    ('REQUIRES_INPUT','PROCESSING'),
    ('REQUIRES_INPUT','DECLINED'),
    ('REQUIRES_INPUT','CANCELLED'),
    ('REQUIRES_INPUT','EXPIRED'),
    ('MANUAL_REVIEW','APPROVED'),
    ('MANUAL_REVIEW','DECLINED'),
    ('MANUAL_REVIEW','EXPIRED');

-- An edge set the application can add a row to is not a constraint. The
-- ALTER DEFAULT PRIVILEGES in this schema grants SELECT on every table
-- cp_migrate creates to cp_readonly and cp_ops, so writing no GRANT is not the
-- same as granting nothing (00741).
REVOKE ALL ON compliance_profile_state_edges, verification_session_status_edges FROM PUBLIC;
REVOKE ALL ON compliance_profile_state_edges, verification_session_status_edges
    FROM cp_app, cp_readonly, cp_ops;
GRANT SELECT ON compliance_profile_state_edges, verification_session_status_edges
    TO cp_app, cp_readonly, cp_ops;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_compliance_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    -- The edge the row describes must be one §20 has. A row whose endpoints are
    -- the same state is not a state change: 00796's birth screen writes one to
    -- record a sanctions result a profile was created holding.
    IF NEW.from_state IS DISTINCT FROM NEW.to_state
       AND NOT EXISTS (SELECT 1 FROM compliance_profile_state_edges e
                        WHERE e.from_state = NEW.from_state AND e.to_state = NEW.to_state) THEN
        RAISE EXCEPTION 'COMPLIANCE_TRANSITION_ILLEGAL_EDGE: a verification state cannot go % -> %; no such edge exists',
            NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
    END IF;
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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_verification_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.from_status IS DISTINCT FROM NEW.to_status
       AND NOT EXISTS (SELECT 1 FROM verification_session_status_edges e
                        WHERE e.from_status = NEW.from_status AND e.to_status = NEW.to_status) THEN
        RAISE EXCEPTION 'VERIFICATION_TRANSITION_ILLEGAL_EDGE: a verification session cannot go % -> %; no such edge exists',
            NEW.from_status, NEW.to_status USING ERRCODE = 'AD001';
    END IF;
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

-- ---------------------------------------------------------------------------
-- Evidence attaches to a session a provider actually answered
-- ---------------------------------------------------------------------------

-- BEFORE INSERT rather than AFTER, so a refused row is never written, and with
-- `search_path` pinned even though this is not a definer: without pg_temp named
-- explicitly it is searched FIRST for relation names, and a caller who may
-- CREATE TEMP could otherwise hand the check its own verification_sessions
-- (00717, 00804, F-48).
-- +goose StatementBegin
CREATE FUNCTION cp_verification_check_has_a_session() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    s_provider text;
    s_status   text;
BEGIN
    SELECT provider, status INTO s_provider, s_status
      FROM verification_sessions WHERE id = NEW.session_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'VERIFICATION_CHECK_ORPHANED: verification_checks names session % which does not exist',
            NEW.session_id USING ERRCODE = 'AD001';
    END IF;
    IF NEW.provider IS DISTINCT FROM s_provider THEN
        RAISE EXCEPTION 'VERIFICATION_CHECK_FOREIGN_PROVIDER: a check on session % claims provider % and the session was opened with %',
            NEW.session_id, NEW.provider, s_provider USING ERRCODE = 'AD001';
    END IF;
    IF s_status NOT IN ('PROCESSING','REQUIRES_INPUT','MANUAL_REVIEW','APPROVED','DECLINED') THEN
        RAISE EXCEPTION 'VERIFICATION_CHECK_UNANSWERED_SESSION: session % is %, which no provider answer produces; evidence cannot attach to it',
            NEW.session_id, s_status USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER verification_checks_have_a_session
    BEFORE INSERT ON verification_checks
    FOR EACH ROW EXECUTE FUNCTION cp_verification_check_has_a_session();

COMMENT ON TABLE compliance_profile_state_edges IS
    'The legal edges of the §20 verification state machine, populated from verification.StateEdges() and held identical to it by test/integration/enums. cp_compliance_apply_state_transition refuses a transition row whose edge is not here; no role but cp_migrate may write it (00806, F-224).';
COMMENT ON TABLE verification_session_status_edges IS
    'The legal edges of the verification SESSION state machine, populated from verification.SessionEdges(). cp_verification_apply_status_transition refuses a row whose edge is not here, which is what stops CREATED -> APPROVED in one INSERT (00806, F-224).';
COMMENT ON FUNCTION cp_verification_check_has_a_session() IS
    'A verification_checks row names the session''s own provider and attaches only to a session in a status a provider answer produces. Without it, four PASS rows against a session nobody was ever sent to made the resolver report PAYOUT_KYC (00806, F-224).';

-- +goose Down
SELECT 1; -- protected: reverting returns the verification state machines to being edge sets no database object reads
