-- +goose Up
-- A resolution belongs on the transition that resolves, and a reconciliation
-- record's status is not the application's to write.
--
-- F-42's stronger remedy, table nine of eleven, and the widest so far: the old
-- UPDATE wrote ten columns, six of which came from a `ResolutionPatch` that no
-- transition row carried.
--
-- ## Why the six moved instead of being granted back
--
-- The same argument as 00748 and 00750, and it applies most clearly here. A
-- reconciliation record is resolved BY a transition to RESOLVED_MANUAL or
-- RESOLVED_AUTOMATIC. **Who resolved it, why, on what evidence, under whose
-- approval, and with which compensating journal transaction, are all facts
-- about that transition** -- not properties the record acquires separately. A
-- resolution written beside the transition rather than on it is a record that
-- cannot say from its own history who closed it.
--
-- `reconciliation_records_check` already demands that a RESOLVED_MANUAL record
-- carry an actor, a reason and an evidence ref. That demand now has somewhere to
-- be satisfied FROM.
--
-- ## The actor on the resolution is not the actor on the transition
--
-- This is the trap in this table and it is why the six are carried explicitly
-- rather than mapped from the columns already present.
--
-- `reconciliation_transitions` has `actor_type`/`actor_id` -- who made the
-- transition. `reconciliation_records` has `resolved_by_actor_type`/`_id` -- who
-- resolved the discrepancy. They are frequently different, and only the second
-- is refused to an AGENT
-- (`reconciliation_records_resolved_by_actor_type_check`). Mapping one onto the
-- other would have quietly let an agent be recorded as the resolver of a
-- financial discrepancy, which is precisely what that CHECK exists to prevent.
--
-- So the CHECK is mirrored onto the transition, for the reason 00750 gives: an
-- illegal resolver is refused where the row is written rather than surfacing as
-- a violation on a table the caller did not touch.
--
-- The same distinction applies to `reason` and `evidence_ref`, which explain the
-- TRANSITION, versus `resolution_reason` and `resolution_evidence_ref`, which
-- explain the RESOLUTION. 00750 made the same split for `failure_reason`.
--
-- ## What is derived, and from what
--
--   matched_at        stamped on the way to MATCHED, coalesced
--   resolved_at       stamped on the way to either RESOLVED_*, coalesced
--   blocks_new_risk   false at MATCHED or a terminal status, else carried
--
-- The terminal statuses are MATCHED, RESOLVED_AUTOMATIC and RESOLVED_MANUAL --
-- the three with no outgoing edges in `Transitions` (internal/reconciliation),
-- read from the map rather than guessed, which is the mistake 00748's first
-- draft made with FILLED.
--
-- ## The compare-and-swap moved, as in 00748
--
-- `WHERE id = $1 AND status = $12` becomes `AND status = NEW.from_status` in the
-- trigger, so it applies to every writer and doubles as a refusal of a row whose
-- `from_status` no longer describes the record.
--
-- `claimStatusUpdate`'s transaction-local claim (`cp.recon.updated.*`) is
-- untouched and keeps working: it limits a record to one status change per
-- transaction, which is a different rule from this one.
--
-- ## The grant-back
--
-- `UpdateObservation` writes `expected`, `observed`, `difference`, `material`
-- and `blocks_new_risk` as a sweep re-measures a discrepancy. That is not a
-- status change. `blocks_new_risk` appears on both sides deliberately: a
-- re-measurement may change whether a discrepancy blocks risk, and so may a
-- resolution.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   reconciliation_transitions_flag
--   reconciliation_transitions_flag_edge
--   reconciliation_transitions_writes_the_status   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001, as in 00743 through 00750.

ALTER TABLE reconciliation_transitions ADD COLUMN to_resolved_by_actor_type text;
ALTER TABLE reconciliation_transitions ADD COLUMN to_resolved_by_actor_id text;
ALTER TABLE reconciliation_transitions ADD COLUMN to_resolution_reason text;
ALTER TABLE reconciliation_transitions ADD COLUMN to_resolution_evidence_ref text;
ALTER TABLE reconciliation_transitions ADD COLUMN to_approval_id uuid;
ALTER TABLE reconciliation_transitions ADD COLUMN to_compensating_journal_transaction_id uuid;

-- The record's own rule, mirrored so an illegal resolver is refused where it is
-- written. NOT VALID for the reason 00748 gives.
ALTER TABLE reconciliation_transitions
    ADD CONSTRAINT reconciliation_transitions_resolver_is_not_an_agent
    CHECK (to_resolved_by_actor_type IS NULL OR to_resolved_by_actor_type <> 'AGENT') NOT VALID;

COMMENT ON COLUMN reconciliation_transitions.to_resolved_by_actor_type IS
    'Who resolved the discrepancy, which is not necessarily who made the transition. Only this one is refused to an AGENT; actor_type on this row is not (00751).';

-- +goose StatementBegin
CREATE FUNCTION cp_reconciliation_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    settled boolean;
BEGIN
    -- The birth transition records arrival, it does not move anything.
    --
    -- `Create` writes the record and then a NONE -> OPEN row, so the record
    -- already holds its status when this fires. NONE is not a stored status
    -- (internal/reconciliation names it exactly so), and the compare-and-swap
    -- below would refuse it: nothing is in NONE, ever. Returning early here is
    -- not a hole -- 00724's `cp_require_transition_on_insert` is what makes the
    -- birth row compulsory, and it is untouched.
    IF NEW.from_status = 'NONE' THEN
        RETURN NULL;
    END IF;

    -- MATCHED, RESOLVED_AUTOMATIC and RESOLVED_MANUAL: the three statuses with
    -- no outgoing edge in internal/reconciliation's Transitions map.
    settled := NEW.to_status = ANY (ARRAY['MATCHED'::text, 'RESOLVED_AUTOMATIC'::text, 'RESOLVED_MANUAL'::text]);
    UPDATE reconciliation_records
       SET status = NEW.to_status,
           matched_at = CASE WHEN NEW.to_status = 'MATCHED'
                             THEN coalesce(matched_at, NEW.occurred_at) ELSE matched_at END,
           resolved_at = CASE WHEN NEW.to_status = ANY (ARRAY['RESOLVED_AUTOMATIC'::text, 'RESOLVED_MANUAL'::text])
                             THEN coalesce(resolved_at, NEW.occurred_at) ELSE resolved_at END,
           blocks_new_risk = CASE WHEN settled THEN false ELSE blocks_new_risk END,
           -- coalesce: a transition that does not resolve leaves the resolution
           -- alone, which is what the Go code did.
           resolved_by_actor_type = coalesce(NEW.to_resolved_by_actor_type, resolved_by_actor_type),
           resolved_by_actor_id = coalesce(NEW.to_resolved_by_actor_id, resolved_by_actor_id),
           resolution_reason = coalesce(NEW.to_resolution_reason, resolution_reason),
           resolution_evidence_ref = coalesce(NEW.to_resolution_evidence_ref, resolution_evidence_ref),
           approval_id = coalesce(NEW.to_approval_id, approval_id),
           compensating_journal_transaction_id =
               coalesce(NEW.to_compensating_journal_transaction_id, compensating_journal_transaction_id)
     WHERE id = NEW.record_id
       AND status = NEW.from_status;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM reconciliation_records WHERE id = NEW.record_id) THEN
            RAISE EXCEPTION 'RECONCILIATION_TRANSITION_ORPHANED: reconciliation_transitions names record % which does not exist',
                NEW.record_id USING ERRCODE = 'AD001';
        END IF;
        RAISE EXCEPTION 'RECONCILIATION_CHANGED_CONCURRENTLY: record % is not in % any more, so a transition from that status does not describe it',
            NEW.record_id, NEW.from_status USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER reconciliation_transitions_writes_the_status
    AFTER INSERT ON reconciliation_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_reconciliation_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_reconciliation_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON reconciliation_records FROM cp_app;
-- UpdateObservation, which re-measures a discrepancy and is not a status
-- change. These also carry the row lock GetForUpdate takes.
GRANT UPDATE (expected, observed, difference, material, blocks_new_risk)
    ON reconciliation_records TO cp_app;

COMMENT ON FUNCTION cp_reconciliation_apply_status_transition() IS
    'Writes reconciliation_records.status, its stamps and the whole resolution from the transition row, refusing when the row''s from_status no longer describes the record. The application holds UPDATE on the five observation columns only (00751, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
