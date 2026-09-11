-- +goose Up
-- An intent status is not the application's to write.
--
-- F-42's stronger remedy, table seven of eleven, and the second half of the
-- decision 00748 made. `trade_intents` had exactly the gap `orders` had --
-- `rejection_code` written by the transition and carried on no transition row --
-- so it gets exactly the same answer, deliberately, rather than a second answer
-- that would have to be reconciled later.
--
-- ## Where this differs from 00748, and it is not cosmetic
--
-- The two tables' rules about the code are NOT the same, and copying the CHECK
-- across would have been wrong:
--
--   orders          REJECTED requires a code; nothing else may carry one.
--   trade_intents   REJECTED *and* NO_VALID_PLAN require one, and any terminal
--                   status may carry one.
--
-- Both come from the code rather than from the shape of the other migration:
-- `RequiresRejectionCode` (internal/intent/transitions.go) names the two, and
-- the validator refuses a code on a non-terminal transition
-- (`!to.IsTerminal() && e.RejectionCode != ""`). Six statuses are terminal:
-- COMPLETED, REJECTED, EXPIRED, CANCELLED, FAILED, NO_VALID_PLAN.
--
-- So the CHECK has two halves: the two statuses that demand a code, and the
-- terminal set that is permitted to carry one. A single-equality CHECK like
-- `orders` has would have refused a legitimate FAILED transition that explained
-- itself.
--
-- ## No compare-and-swap here
--
-- Unlike `orders`, this UPDATE had no `AND status = $N`, so there is none to
-- preserve. `lock` takes `SELECT ... FOR UPDATE` and that is the whole of the
-- concurrency control. Adding a CAS to the trigger would be a new behaviour
-- rather than a moved one, and this migration moves things.
--
-- ## The grant-back
--
-- `Link` writes five identifiers as the intent acquires them --
-- `eligibility_decision_id`, `risk_decision_id`, `reservation_id`, `plan_id`,
-- `order_id`. None is a status change; each records that some other part of the
-- system produced a decision for this intent. They also carry the row lock.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   intent_transitions_flag
--   intent_transitions_flag_edge
--   intent_transitions_writes_the_status   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001, as in 00743 through 00748.

ALTER TABLE intent_transitions ADD COLUMN rejection_code text;

-- NOT VALID for the reason 00748 gives: rows written before this column existed
-- cannot be backfilled truthfully, because an intent's current code came from
-- whichever transition set it last.
ALTER TABLE intent_transitions ADD CONSTRAINT intent_transitions_rejection_code_check
    CHECK (
        (to_status <> ALL (ARRAY['REJECTED'::text, 'NO_VALID_PLAN'::text])
         OR (rejection_code IS NOT NULL AND rejection_code <> ''))
        AND (rejection_code IS NULL
         OR to_status = ANY (ARRAY['COMPLETED'::text, 'REJECTED'::text, 'EXPIRED'::text,
                                   'CANCELLED'::text, 'FAILED'::text, 'NO_VALID_PLAN'::text]))
    ) NOT VALID;

COMMENT ON COLUMN intent_transitions.rejection_code IS
    'Why this transition ended the intent. Required when to_status is REJECTED or NO_VALID_PLAN, permitted on any terminal status, refused on the rest (00749).';

-- +goose StatementBegin
CREATE FUNCTION cp_intent_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE trade_intents
       SET status = NEW.to_status,
           -- Carried forward when this transition does not set one, matching
           -- the COALESCE(NULLIF(...)) the Go code used.
           rejection_code = coalesce(NEW.rejection_code, rejection_code),
           terminal_at = CASE
               WHEN NEW.to_status = ANY (ARRAY['COMPLETED'::text, 'REJECTED'::text, 'EXPIRED'::text,
                                               'CANCELLED'::text, 'FAILED'::text, 'NO_VALID_PLAN'::text])
               THEN coalesce(terminal_at, NEW.occurred_at) ELSE terminal_at END
     WHERE id = NEW.intent_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'INTENT_TRANSITION_ORPHANED: intent_transitions names intent % which does not exist',
            NEW.intent_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER intent_transitions_writes_the_status
    AFTER INSERT ON intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_intent_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_intent_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON trade_intents FROM cp_app;
-- Link, which records that another part of the system produced a decision for
-- this intent. Not a status change, and these also carry the row lock.
GRANT UPDATE (eligibility_decision_id, risk_decision_id, reservation_id, plan_id, order_id)
    ON trade_intents TO cp_app;

COMMENT ON FUNCTION cp_intent_apply_status_transition() IS
    'Writes trade_intents.status, rejection_code and terminal_at from the transition row. The application holds UPDATE on the five Link identifiers only (00749, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
