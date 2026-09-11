-- +goose Up
-- The fourth state machine in this area had no edge table, so a destination
-- came back from a terminal status.
--
-- 00806 gave `compliance_profiles.identity_state` and
-- `verification_sessions.status` an edge table apiece and made their apply
-- functions refuse an edge the state machine does not have; 00807 did the same
-- for `payout_requests.state`. Both migrations name 00763
-- (`payout_destinations.status`) in their own headers as a sibling that had
-- already had the F-42 treatment. It had had half of it.
--
-- `cp_destination_apply_status_transition()` (00763) writes `status` and
-- `verified_at` from whatever the transition row says, and consults nothing.
-- 00731's deferred binding asks only whether a transition row exists naming the
-- status the destination is really in -- never whether the edge that row
-- describes is one `payout.CanTransitionDestination` has. So one INSERT, as
-- `cp_app`:
--
--     INSERT INTO payout_destination_transitions
--         (id, destination_id, from_status, to_status, actor_type, actor_id, reason)
--     VALUES (..., 'DISABLED', 'VERIFIED', 'SYSTEM', ..., ...);
--
-- and the row that says where somebody's money goes was usable again, with a
-- fresh `verified_at`, after its holder had disabled it -- which §25 marks
-- step-up-protected precisely because it is the act a person performs when a
-- destination is compromised (F-259/F-wv2-1).
--
-- 00763's own header states the rule this table now holds: "A destination never
-- returns from DISABLED or REJECTED. Re-adding one is a new row with its own
-- creation time, which is what makes the §25 cooldown on a changed destination
-- a fact the database can state."
--
-- ## What this does
--
-- `payout_destination_status_edges`, populated from
-- `payout.DestinationStateEdges()`, with 00806's grants: nothing but
-- `cp_migrate` may write it, and the REVOKE is explicit because the ALTER
-- DEFAULT PRIVILEGES in this schema hands SELECT to `cp_readonly` and `cp_ops`
-- without a GRANT being written (00741 learned that the hard way).
--
-- `cp_destination_apply_status_transition()` is replaced with one that refuses
-- an edge that is not in the table (AD001).
--
-- There is NO same-state exemption here, unlike 00806. Nothing writes a
-- same-state destination row: `payout.Service.TransitionDestination` asks
-- `CanTransitionDestination(current, to)` and no status is a legal successor of
-- itself, so a same-state row is by construction something the service did not
-- write. 00815 removes the one on `payout_requests` for the same reason and
-- narrows the one on `compliance_profiles` to the single row 00796 needs.
--
-- `test/integration/enums` gains its fourth pairing, so D-121's "held identical
-- by the enum suite" now covers every edge set in this area.
--
-- Custom SQLSTATE: AD001, as in 00761-00763, 00806 and 00807.

CREATE TABLE payout_destination_status_edges (
    from_status text NOT NULL,
    to_status   text NOT NULL,
    PRIMARY KEY (from_status, to_status)
);

-- payout.DestinationStateEdges(), in lifecycle order. Note what is absent:
-- REJECTED and DISABLED have no outgoing edge at all, and nothing reaches
-- VERIFIED except UNVERIFIED.
INSERT INTO payout_destination_status_edges (from_status, to_status) VALUES
    ('UNVERIFIED','VERIFIED'),
    ('UNVERIFIED','REJECTED'),
    ('UNVERIFIED','DISABLED'),
    ('VERIFIED','DISABLED'),
    ('VERIFIED','REJECTED');

REVOKE ALL ON payout_destination_status_edges FROM PUBLIC;
REVOKE ALL ON payout_destination_status_edges FROM cp_app, cp_readonly, cp_ops;
GRANT SELECT ON payout_destination_status_edges TO cp_app, cp_readonly, cp_ops;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_destination_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
-- pg_catalog first and pg_temp named explicitly: without it pg_temp is searched
-- FIRST for relation names, and a caller who may CREATE TEMP could otherwise
-- hand this function its own edge table (00717, 00804, F-48).
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    -- Every row must describe an edge §25's machine has. There is no same-state
    -- exemption: no destination status is a legal successor of itself, so a
    -- same-state row is a row nothing in internal/payout wrote.
    IF NOT EXISTS (SELECT 1 FROM payout_destination_status_edges e
                    WHERE e.from_status = NEW.from_status AND e.to_status = NEW.to_status) THEN
        RAISE EXCEPTION 'DESTINATION_TRANSITION_ILLEGAL_EDGE: a payout destination cannot go % -> %; no such edge exists',
            NEW.from_status, NEW.to_status USING ERRCODE = 'AD001';
    END IF;
    UPDATE payout_destinations
       SET status = NEW.to_status,
           -- Recorded when it becomes usable and cleared when it stops being,
           -- so a reader cannot mistake a historical confirmation for a current
           -- one.
           verified_at = CASE WHEN NEW.to_status = 'VERIFIED' THEN NEW.occurred_at ELSE NULL END
     WHERE id = NEW.destination_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'DESTINATION_TRANSITION_ORPHANED: payout_destination_transitions names destination % which does not exist',
            NEW.destination_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

REVOKE EXECUTE ON FUNCTION cp_destination_apply_status_transition() FROM PUBLIC;

COMMENT ON TABLE payout_destination_status_edges IS
    'The legal edges of the payout DESTINATION state machine, populated from payout.DestinationStateEdges() and held identical to it by test/integration/enums. cp_destination_apply_status_transition refuses a transition row whose edge is not here, which is what stops DISABLED -> VERIFIED in one INSERT; no role but cp_migrate may write it (00814, F-259).';
COMMENT ON FUNCTION cp_destination_apply_status_transition() IS
    'Writes payout_destinations.status and verified_at from the transition row, refusing an edge payout_destination_status_edges does not have. cp_app holds UPDATE on display_label only, so inserting a LEGAL transition row is the only way a destination becomes usable (00814, 00763, F-42, F-259).';

-- +goose Down
SELECT 1; -- protected: reverting returns the destination state machine to being an edge set no database object reads
