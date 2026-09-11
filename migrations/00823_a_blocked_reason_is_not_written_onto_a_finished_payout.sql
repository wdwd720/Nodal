-- +goose Up
-- A blocked reason may be WRITTEN only onto a payout that can still be sent
-- (F-279, D-139 amended).
--
-- 00822 added `blocked_reason` and `blocked_at` and granted cp_app UPDATE on
-- them. The reason is a fact about a RESERVED payout: "this cannot be
-- submitted, and cancelling is what releases the Credits it holds". It stays on
-- the request after a cancellation on purpose -- why somebody's value could not
-- be sent is part of that payout's history -- and `payout.Request.Blocked()`
-- and the API now read it only while the request is still in VERIFIED, because
-- that is the only state in which the sentence is true.
--
-- What the DATABASE held it to was the pairing CHECK: a reason and an instant
-- exist together. Nothing said a reason could not be STAMPED onto a payout that
-- had already finished. `payout.Service.recordBlocked` carries
-- `AND state = 'VERIFIED'` in its WHERE and is the only writer that does; any
-- other statement the application role runs -- a future sweep, a repair script,
-- a mistake -- could write "this withdrawal cannot be sent: its Credits are
-- still reserved" onto a REJECTED request whose reserved quantity is zero, and
-- the row would accept it.
--
-- So the rule is enforced where it cannot be forgotten. It is about the WRITE,
-- not about the row: an UPDATE that carries a reason forward unchanged -- which
-- is what every state transition on a blocked request does, including the
-- cancellation that resolves it -- is untouched, and only a statement that
-- actually changes the reason to something non-null is judged.
--
-- Terminal is read from `payout_request_state_edges` rather than listed here.
-- A state with no outgoing edge is a payout that has finished moving; listing
-- REJECTED, FAILED and REVERSED by hand would be a fourth statement of the
-- state machine, and this register is full of rules that were written twice and
-- came apart. SETTLED has an edge (to REVERSED) and is therefore not refused
-- here: the application writes a reason in VERIFIED only, which is narrower
-- than this floor on purpose -- the database refuses what can never be true,
-- and the service decides what it is willing to say.
--
-- The refusal is loud. A BEFORE UPDATE trigger returning NULL would make the
-- statement affect no rows and report no error, which is the shape this
-- schema's own protections have always refused to take: a write that silently
-- does nothing is how a caller comes to believe something was recorded.
--
-- Custom SQLSTATE: AD001, the administrative-authority family (00807, 00806).

-- +goose StatementBegin
CREATE FUNCTION cp_payout_blocked_reason_is_not_history() RETURNS trigger
LANGUAGE plpgsql
SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM payout_request_state_edges e WHERE e.from_state = NEW.state) THEN
        RAISE EXCEPTION 'PAYOUT_BLOCKED_REASON_ON_FINISHED: payout % is %, which has no outgoing edge; a blocked reason says a reserved payout cannot be sent YET and cannot become true of a payout that has finished moving',
            NEW.id, NEW.state USING ERRCODE = 'AD001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- The WHEN clause is the whole of "this is a write OF the reason": a transition
-- that carries an existing reason forward, or clears it, never reaches the
-- function.
CREATE TRIGGER payout_requests_blocked_reason_is_not_history
    BEFORE UPDATE ON payout_requests
    FOR EACH ROW
    WHEN (NEW.blocked_reason IS NOT NULL AND NEW.blocked_reason IS DISTINCT FROM OLD.blocked_reason)
    EXECUTE FUNCTION cp_payout_blocked_reason_is_not_history();

REVOKE EXECUTE ON FUNCTION cp_payout_blocked_reason_is_not_history() FROM PUBLIC;

COMMENT ON FUNCTION cp_payout_blocked_reason_is_not_history() IS
    'Refuses a write of payout_requests.blocked_reason onto a request whose state has no outgoing edge in payout_request_state_edges. The reason is a fact about a payout that can still be sent; it stays on the row afterwards as history, and it is not something a finished payout can newly acquire (00823, F-279, D-139).';
COMMENT ON COLUMN payout_requests.blocked_reason IS
    'Why this reserved payout cannot be submitted, in words its holder can read, or NULL when nothing is blocking it. It is not a state and not a failure: the request is still VERIFIED and its Credits are still reserved, and cancelling it is what releases them. It stays on the row after a cancellation as history, and cp_payout_blocked_reason_is_not_history refuses a NEW reason written onto a payout that has finished moving (00822, 00823, F-277, F-279).';

-- +goose Down
SELECT 1; -- protected: reverting returns a finished payout's history to the application's reach
