-- +goose Up
-- A cooling-off period is measured by the database's clock, and a state machine
-- keeps its edges in the schema.
--
-- 00758 said of the closure wait: "enforced by the trigger below, not only by
-- the service, because the whole point of a cooling-off period is that it
-- survives a bug in code that is in a hurry." The trigger compared
-- `NEW.occurred_at` -- a column the INSERTing role chooses -- against
-- `cooling_off_until`, so the invariant was the caller's own timestamp compared
-- with a column. One INSERT stamped `now() + interval '400 days'` effected a
-- closure inside the wait, and the wall clock was never consulted (F-174).
--
-- Two things follow, and they are separate:
--
--   1. **The wait is measured against the transaction's own clock.**
--      `statement_timestamp()` is a value no caller supplies. It is the
--      statement's clock rather than `now()`'s transaction clock because a
--      transaction that began before the wait ended must not be able to effect
--      a closure it could not have effected at the moment it wrote the row.
--   2. **The audit stamp is bounded.** `occurred_at` still exists, and still
--      says when the actor acted; it is now required to be within two minutes
--      of the statement clock in either direction. That bound is clock skew
--      between the application host and the database and nothing else: it is
--      not a window inside which a stamp may be chosen. A row stamped in the
--      future is a claim about something that has not happened, and one stamped
--      in the distant past is a claim about when somebody decided (F-174).
--
-- ## The edge sets, which existed only in Go
--
-- `internal/profile` declares both state machines -- `closureTransitions`, and
-- the four legal `users.status` edges 00757's own header spells out -- and
-- enforces them in `CanCloseTransition` and in the service. The schema enforced
-- only that a transition row DESCRIBES the change it makes (00726's edge
-- binding); it never asked whether the change was legal. So the application role
-- could walk a terminal EFFECTED request back to CANCELLED, keeping the reason
-- given for the decision it no longer recorded, and could reopen a CLOSED user
-- that 00757 says has no edge out of it "here or in Go" (F-176).
--
-- The edge sets are written as CHECK constraints on the transition tables, and
-- deliberately not as an `IF` inside the apply functions. Three reasons:
--
--   * A CHECK refuses at the INSERT, where the mistake is -- 00760's argument
--     for `operator_roles_role_check`, and the same argument.
--   * A CHECK is readable by `pg_get_constraintdef`, which is what lets
--     `test/integration/enums` hold the list against the Go declaration that
--     repeats it. A branch inside a function body can be compared with nothing.
--   * A second copy of the rule inside the apply function would be unreachable,
--     because the CHECK fires first, and two rules refusing one write make every
--     failure ambiguous about which one fired, which is F-53's lesson.
--
-- The pair is complete because 00726's binding already forces `from_state` to be
-- the state the row actually moved from: the CHECK says which moves are legal,
-- and the binding says the row is describing the move it makes.

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_closure_request_apply_transition() RETURNS trigger
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
    -- The cooling-off period is a database invariant, measured against a clock
    -- the caller does not supply.
    IF NEW.to_state = 'EFFECTED' AND statement_timestamp() < waits_until THEN
        RAISE EXCEPTION 'CLOSURE_STILL_COOLING: closure request % cannot be effected until %, and it is now %',
            NEW.request_id, waits_until, statement_timestamp() USING ERRCODE = 'AD001';
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

REVOKE EXECUTE ON FUNCTION cp_closure_request_apply_transition() FROM PUBLIC;

-- +goose StatementBegin
CREATE FUNCTION cp_transition_stamp_is_honest() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    skew CONSTANT interval := interval '2 minutes';
BEGIN
    IF NEW.occurred_at > statement_timestamp() + skew OR NEW.occurred_at < statement_timestamp() - skew THEN
        RAISE EXCEPTION 'TRANSITION_STAMP_SKEWED: a % row stamped % is further than % from the database clock (%); occurred_at records when the actor acted, and the only slack it may carry is clock skew',
            TG_TABLE_NAME, NEW.occurred_at, skew, statement_timestamp() USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

REVOKE EXECUTE ON FUNCTION cp_transition_stamp_is_honest() FROM PUBLIC;

CREATE TRIGGER account_closure_request_transitions_stamp_is_honest
    BEFORE INSERT ON account_closure_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_transition_stamp_is_honest();

ALTER TABLE account_closure_request_transitions
    ADD CONSTRAINT account_closure_request_transitions_edge_check CHECK (
        (from_state, to_state) IN (
            ('PENDING', 'CANCELLED'),
            ('PENDING', 'REFUSED'),
            ('PENDING', 'EFFECTED')));

ALTER TABLE user_status_transitions
    ADD CONSTRAINT user_status_transitions_edge_check CHECK (
        (from_status, to_status) IN (
            ('ACTIVE', 'SUSPENDED'),
            ('ACTIVE', 'CLOSED'),
            ('SUSPENDED', 'ACTIVE'),
            ('SUSPENDED', 'CLOSED')));

COMMENT ON FUNCTION cp_closure_request_apply_transition() IS
    'Writes account_closure_requests.state, decided_at and decided_reason from the transition row, and refuses an EFFECTED transition before the cooling-off period has passed -- measured against statement_timestamp(), never against the caller-supplied occurred_at (00798, F-174).';
COMMENT ON FUNCTION cp_transition_stamp_is_honest() IS
    'Bounds a transition row occurred_at to two minutes either side of the database clock. The bound is clock skew between the application host and the database, not a window in which an audit stamp may be chosen (00798, F-174).';
COMMENT ON CONSTRAINT account_closure_request_transitions_edge_check ON account_closure_request_transitions IS
    'The closure edge set: PENDING is the only origin and every other state is terminal. profile.ClosureEdges() is the same list in Go, and test/integration/enums holds the two together (00798, F-176).';
COMMENT ON CONSTRAINT user_status_transitions_edge_check ON user_status_transitions IS
    'The user-status edge set 00757 describes in words. CLOSED is terminal: there is no edge out of it, here or in Go. profile.UserStatusEdges() is the same list, held against this by test/integration/enums (00798, F-176).';

-- +goose Down
SELECT 1; -- protected: reverting would measure a cooling-off period against a timestamp its caller chooses, and would return both edge sets to Go alone
