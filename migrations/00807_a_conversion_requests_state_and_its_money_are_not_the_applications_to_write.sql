-- +goose Up
-- A conversion request's state, and the money beside it, are not the
-- application's to write.
--
-- `payout_requests` is the row that says whether somebody's money left. Its
-- neighbours in this area were all given the F-42 treatment in this goal's wave
-- -- 00761 (`compliance_profiles.identity_state`), 00762
-- (`verification_sessions.status`), 00763 (`payout_destinations.status`) -- each
-- revoking UPDATE from `cp_app` and having a SECURITY DEFINER trigger write the
-- column from the transition row. This table kept a column grant that included
-- `state`, `reserved_quantity`, `settled_quantity`, `reserved_at` and
-- `settled_at` (00713, narrowed but not removed by 00733), with only 00731's
-- edge binding in front of it.
--
-- That binding asks whether a transition row exists naming the state the
-- request is really in. It never asks whether the edge that row describes is one
-- `internal/payout.CanTransition` has. So one transaction, as `cp_app`,
-- committed (F-226/F-wv-6):
--
--     INSERT INTO payout_request_transitions
--         (..., from_state, to_state, ...) VALUES (..., 'REJECTED', 'SETTLED', ...);
--     UPDATE payout_requests
--        SET state = 'SETTLED', reserved_quantity = requested_quantity,
--            settled_quantity = requested_quantity, settled_at = now(),
--            provider_reference = 'forged-by-cp_app'
--      WHERE id = ...;
--
-- REJECTED is terminal in Go. The request was rendered to its holder afterwards
-- as a SETTLED payout of the whole amount, with no ledger posting, no
-- allocation, and its eligibility reasons still reading
-- ["INSUFFICIENT_ELIGIBLE_VALUE","ORIGIN_NOT_PAYOUT_ELIGIBLE"].
--
-- ## What this does
--
-- 1. `payout_request_state_edges`, populated from `payout.StateEdges()` and held
--    identical to it by `test/integration/enums` -- 00806's shape, for the same
--    reason.
--
-- 2. `cp_payout_apply_state_transition()` writes `state` from the transition
--    row, refusing an edge that is not in the table (AD001).
--
-- 3. The money moves onto the transition row rather than sitting beside it.
--    `reserved_quantity`, `settled_quantity`, `reserved_at` and `settled_at`
--    become columns of `payout_request_transitions`, NULL meaning "this change
--    says nothing about that number", and the same trigger writes them. Every
--    place `internal/payout` wrote one of them it was also moving the state, so
--    nothing is lost: reserving is the VERIFIED step, settling is the SETTLED
--    step, and returning a reservation is the FAILED or REJECTED step. What is
--    gained is that a quantity can no longer be rewritten beside a lawful state
--    move, which is exactly what 00733's header describes and could not stop
--    for the columns it left granted.
--
--    `provider_reference` and `provider_status` come with them. They are the
--    provider's words about this payout and were written in the same breath as
--    the state change in both places that set them; leaving them granted would
--    have left `provider_reference = 'forged-by-cp_app'` reachable, which is
--    half of what the reproduction rendered to the holder.
--
-- 4. `REVOKE UPDATE ON payout_requests FROM cp_app`, with a column grant back
--    for exactly what the application owns: the provider slot it claims before
--    it calls (`provider`, `provider_idempotency_key`, `submitted_at`), the
--    decision it records (`verification_level`, `policy_version`, `policy_hash`,
--    `eligibility_reasons`) and `failure_reason`. The grant is also what permits
--    the row lock every one of those paths takes first: `SELECT ... FOR UPDATE`
--    requires UPDATE privilege, and a table-level REVOKE with no column grant
--    refuses all four lock strengths (00744 probed this).
--
-- The DEFERRED edge binding from 00731 stays exactly where it is. It is now
-- belt and braces rather than the only control, and it still catches the case
-- the trigger cannot see: a write to `payout_requests.state` by a role that has
-- the privilege -- the owner, in a migration or by hand -- with no transition
-- row at all.
--
-- Custom SQLSTATE: AD001, as in 00761-00763 and 00806.

CREATE TABLE payout_request_state_edges (
    from_state text NOT NULL,
    to_state   text NOT NULL,
    PRIMARY KEY (from_state, to_state)
);

-- payout.StateEdges(), in declaration order. Note what is absent:
-- PAYOUT_STATUS_UNKNOWN cannot go back to VERIFIED or SUBMITTED, and FAILED,
-- REJECTED and REVERSED have no outgoing edge at all.
INSERT INTO payout_request_state_edges (from_state, to_state) VALUES
    ('DRAFT','ELIGIBILITY_CHECK'),
    ('DRAFT','REJECTED'),
    ('ELIGIBILITY_CHECK','VERIFICATION_REQUIRED'),
    ('ELIGIBILITY_CHECK','VERIFIED'),
    ('ELIGIBILITY_CHECK','REJECTED'),
    ('ELIGIBILITY_CHECK','MANUAL_REVIEW'),
    ('VERIFICATION_REQUIRED','VERIFICATION_PENDING'),
    ('VERIFICATION_REQUIRED','VERIFIED'),
    ('VERIFICATION_REQUIRED','REJECTED'),
    ('VERIFICATION_REQUIRED','FAILED'),
    ('VERIFICATION_PENDING','VERIFIED'),
    ('VERIFICATION_PENDING','REJECTED'),
    ('VERIFICATION_PENDING','MANUAL_REVIEW'),
    ('VERIFICATION_PENDING','FAILED'),
    ('VERIFIED','SUBMITTED'),
    ('VERIFIED','REJECTED'),
    ('VERIFIED','MANUAL_REVIEW'),
    ('VERIFIED','FAILED'),
    ('SUBMITTED','PROVIDER_PENDING'),
    ('SUBMITTED','PAYOUT_STATUS_UNKNOWN'),
    ('SUBMITTED','FAILED'),
    ('SUBMITTED','SETTLED'),
    ('SUBMITTED','MANUAL_REVIEW'),
    ('PROVIDER_PENDING','SETTLED'),
    ('PROVIDER_PENDING','FAILED'),
    ('PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN'),
    ('PROVIDER_PENDING','MANUAL_REVIEW'),
    ('PAYOUT_STATUS_UNKNOWN','SETTLED'),
    ('PAYOUT_STATUS_UNKNOWN','FAILED'),
    ('PAYOUT_STATUS_UNKNOWN','MANUAL_REVIEW'),
    ('SETTLED','REVERSED'),
    ('MANUAL_REVIEW','VERIFIED'),
    ('MANUAL_REVIEW','SUBMITTED'),
    ('MANUAL_REVIEW','SETTLED'),
    ('MANUAL_REVIEW','FAILED'),
    ('MANUAL_REVIEW','REJECTED');

REVOKE ALL ON payout_request_state_edges FROM PUBLIC;
REVOKE ALL ON payout_request_state_edges FROM cp_app, cp_readonly, cp_ops;
GRANT SELECT ON payout_request_state_edges TO cp_app, cp_readonly, cp_ops;

-- The money the change carries. NULL means the row says nothing about that
-- number, which is what an ordinary state move says.
ALTER TABLE payout_request_transitions
    ADD COLUMN reserved_quantity  numeric(38,0) CHECK (reserved_quantity >= 0),
    ADD COLUMN settled_quantity   numeric(38,0) CHECK (settled_quantity >= 0),
    ADD COLUMN reserved_at        timestamptz,
    ADD COLUMN settled_at         timestamptz,
    ADD COLUMN provider_reference text,
    ADD COLUMN provider_status    text;

-- +goose StatementBegin
CREATE FUNCTION cp_payout_apply_state_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.from_state IS DISTINCT FROM NEW.to_state
       AND NOT EXISTS (SELECT 1 FROM payout_request_state_edges e
                        WHERE e.from_state = NEW.from_state AND e.to_state = NEW.to_state) THEN
        RAISE EXCEPTION 'PAYOUT_TRANSITION_ILLEGAL_EDGE: a payout cannot go % -> %; no such edge exists',
            NEW.from_state, NEW.to_state USING ERRCODE = 'AD001';
    END IF;
    UPDATE payout_requests
       SET state              = NEW.to_state,
           reserved_quantity  = coalesce(NEW.reserved_quantity, reserved_quantity),
           settled_quantity   = coalesce(NEW.settled_quantity, settled_quantity),
           reserved_at        = coalesce(NEW.reserved_at, reserved_at),
           settled_at         = coalesce(NEW.settled_at, settled_at),
           provider_reference = coalesce(NEW.provider_reference, provider_reference),
           provider_status    = coalesce(NEW.provider_status, provider_status)
     WHERE id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'PAYOUT_TRANSITION_ORPHANED: payout_request_transitions names request % which does not exist',
            NEW.request_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

-- The name ends in _writes_the_state so it sorts after both flag setters
-- ('w' after 'f'), which is what 00761 established and what matters under
-- SET CONSTRAINTS ALL IMMEDIATE.
CREATE TRIGGER payout_request_transitions_writes_the_state
    AFTER INSERT ON payout_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_payout_apply_state_transition();

REVOKE EXECUTE ON FUNCTION cp_payout_apply_state_transition() FROM PUBLIC;

REVOKE UPDATE ON payout_requests FROM cp_app;
GRANT UPDATE (
    provider, provider_idempotency_key, submitted_at,
    verification_level, policy_version, policy_hash, eligibility_reasons,
    failure_reason
) ON payout_requests TO cp_app;

COMMENT ON TABLE payout_request_state_edges IS
    'The legal edges of the payout state machine, populated from payout.StateEdges() and held identical to it by test/integration/enums. cp_payout_apply_state_transition refuses a transition row whose edge is not here; no role but cp_migrate may write it (00807, F-226).';
COMMENT ON FUNCTION cp_payout_apply_state_transition() IS
    'Writes payout_requests.state and the money beside it -- reserved, settled, their instants, and the provider''s reference and status -- from the transition row. cp_app holds UPDATE on the decision and provider-slot columns only, so inserting the transition row is the only way a conversion request moves or a quantity changes (00807, F-42, F-226).';
COMMENT ON COLUMN payout_request_transitions.settled_quantity IS
    'What this change settled, or NULL when it says nothing about the number. Written onto payout_requests by cp_payout_apply_state_transition; cp_app holds no UPDATE on that column (00807, F-226).';

-- +goose Down
SELECT 1; -- protected: reverting returns a conversion request's state and its money columns to the application's reach
