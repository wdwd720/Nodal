-- +goose Up
-- Closing an account is a request that waits, and never a deletion.
--
-- Goal PART 4 asks for account deactivation and PART 52 for the evidence of it.
-- Neither can be a DELETE: every financial table references `users.id` and
-- `accounts.id`, the ledger is append-only, and ADR-0020 already settled that
-- retention against an append-only table is partition detachment and never row
-- removal. So "close my account" is a REQUEST with three properties:
--
--   1. **It waits.** `cooling_off_until` is stamped when the request is made and
--      the request cannot be effected before it passes -- enforced by the
--      trigger below, not only by the service, because the whole point of a
--      cooling-off period is that it survives a bug in code that is in a hurry.
--      It is the control against a hijacked session ending an account before its
--      owner notices: cancelling needs no step-up, requesting does.
--   2. **The user can stop it.** PENDING -> CANCELLED is available to the person
--      whose account it is, for as long as the request is pending.
--   3. **A person effects it.** PENDING -> EFFECTED is an operator action under
--      `account:freeze` and a step-up, and it is what writes the status
--      transitions (`users` and every account owned) that actually close things.
--      No background job closes an account on a timer: the cooling-off period
--      makes closure possible, it does not make it automatic.
--
-- PENDING -> REFUSED exists for the case the product will actually meet: an
-- account with an unsettled payout, an open dispute, or a balance that has to be
-- dealt with first. The refusal carries its reason and the user can ask again;
-- nothing is silently dropped.
--
-- ## Why this is not a status on `accounts`
--
-- `accounts.status` already has CLOSED and 00744 binds it. A "closing" status
-- there would be a fourth answer to "may this account take risk" and would put a
-- product intention into a column the risk kernel reads. The request is a
-- separate record about a future transition; the transition itself remains
-- exactly what 00744 and 00757 describe.

CREATE TABLE account_closure_requests (
    id                   uuid PRIMARY KEY,
    user_id              uuid NOT NULL REFERENCES users(id),
    state                text NOT NULL CHECK (state IN ('PENDING','CANCELLED','REFUSED','EFFECTED')),
    -- What the user typed, if anything. Bounded, and never required: a person
    -- leaving does not owe an explanation.
    requested_reason     text CHECK (requested_reason IS NULL OR length(requested_reason) <= 500),
    requested_at         timestamptz NOT NULL DEFAULT now(),
    cooling_off_until    timestamptz NOT NULL,
    -- The session that asked, so the security page and the audit trail can say
    -- which device started this.
    requested_session_id uuid,
    decided_at           timestamptz,
    decided_reason       text,
    correlation_id       text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT account_closure_requests_waits CHECK (cooling_off_until > requested_at),
    CONSTRAINT account_closure_requests_decided CHECK ((state = 'PENDING') = (decided_at IS NULL))
);

-- One open request per person. A second POST while one is pending is a CONFLICT,
-- not a second row: two pending requests would have two cooling-off clocks and
-- no answer to which one governs.
CREATE UNIQUE INDEX account_closure_requests_one_pending
    ON account_closure_requests (user_id) WHERE state = 'PENDING';
CREATE INDEX account_closure_requests_user_idx ON account_closure_requests (user_id, requested_at DESC);

CREATE TRIGGER account_closure_requests_updated_at BEFORE UPDATE ON account_closure_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_closure_request_is_not_born_decided() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.state <> 'PENDING' OR NEW.decided_at IS NOT NULL OR NEW.decided_reason IS NOT NULL THEN
        RAISE EXCEPTION 'CLOSURE_BORN_DECIDED: a closure request is born PENDING; a decision is a transition row, not an INSERT'
            USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER account_closure_requests_not_born_decided BEFORE INSERT ON account_closure_requests
    FOR EACH ROW EXECUTE FUNCTION cp_closure_request_is_not_born_decided();

CREATE TABLE account_closure_request_transitions (
    id              uuid PRIMARY KEY,
    request_id      uuid NOT NULL REFERENCES account_closure_requests(id),
    from_state      text NOT NULL,
    to_state        text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX account_closure_request_transitions_request_idx
    ON account_closure_request_transitions (request_id, occurred_at);
CREATE TRIGGER account_closure_request_transitions_immutable
    BEFORE UPDATE OR DELETE ON account_closure_request_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TRIGGER account_closure_request_transitions_flag_edge
    AFTER INSERT ON account_closure_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('request_id', 'from_state', 'to_state', 'account_closure_requests');

-- +goose StatementBegin
CREATE FUNCTION cp_closure_request_apply_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    waits_until timestamptz;
BEGIN
    SELECT cooling_off_until INTO waits_until FROM account_closure_requests WHERE id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'CLOSURE_TRANSITION_ORPHANED: account_closure_request_transitions names request % which does not exist',
            NEW.request_id USING ERRCODE = 'AD001';
    END IF;
    -- The cooling-off period is a database invariant, not a service convention.
    IF NEW.to_state = 'EFFECTED' AND NEW.occurred_at < waits_until THEN
        RAISE EXCEPTION 'CLOSURE_STILL_COOLING: closure request % cannot be effected until %, and the transition is stamped %',
            NEW.request_id, waits_until, NEW.occurred_at USING ERRCODE = 'AD001';
    END IF;
    UPDATE account_closure_requests
       SET state = NEW.to_state,
           decided_at = coalesce(decided_at, NEW.occurred_at),
           decided_reason = coalesce(decided_reason, NEW.reason)
     WHERE id = NEW.request_id;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER account_closure_request_transitions_writes_the_state
    AFTER INSERT ON account_closure_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_closure_request_apply_transition();

REVOKE EXECUTE ON FUNCTION cp_closure_request_apply_transition() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION cp_closure_request_is_not_born_decided() FROM PUBLIC;

CREATE CONSTRAINT TRIGGER account_closure_requests_require_transition
    AFTER UPDATE OF state ON account_closure_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('state', 'account_closure_requests');

GRANT SELECT, INSERT ON account_closure_requests TO cp_app;
-- Not for writing: a row lock needs UPDATE privilege (00744), and correlation_id
-- is the narrowest column on this table that nothing ever rewrites. Granting
-- cooling_off_until instead would hand the application the ability to shorten
-- the wait it is not allowed to skip.
GRANT UPDATE (correlation_id) ON account_closure_requests TO cp_app;
GRANT SELECT, INSERT ON account_closure_request_transitions TO cp_app;
GRANT SELECT ON account_closure_requests, account_closure_request_transitions TO cp_readonly, cp_ops;

COMMENT ON TABLE account_closure_requests IS
    'A user asking for their account to be closed. It waits out cooling_off_until (enforced in cp_closure_request_apply_transition), the user can cancel it at any time, and an operator effects it; nothing here deletes anything (ADR-0020, D-055).';
COMMENT ON FUNCTION cp_closure_request_apply_transition() IS
    'Writes account_closure_requests.state, decided_at and decided_reason from the transition row, and refuses an EFFECTED transition before the cooling-off period has passed.';

-- +goose Down
SELECT 1; -- protected: reverting returns the closure state and its cooling-off period to the application reach
