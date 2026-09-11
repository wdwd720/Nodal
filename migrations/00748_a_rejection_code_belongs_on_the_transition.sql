-- +goose Up
-- A rejection code belongs on the transition, and an order status is not the
-- application's to write.
--
-- F-42's stronger remedy, table six of eleven. `orders` is the first of the nine
-- that needed a decision rather than a translation, and the decision is reusable:
-- `trade_intents` has exactly the same gap and gets the same answer next.
--
-- ## The gap
--
-- `transitionLocked` wrote three columns: `status`, `rejection_code` and
-- `terminal_at`. Two come straight off the transition row. `rejection_code` had
-- nowhere to come from -- `order_transitions` records `reason` (free text) and
-- `evidence_ref`, and neither is the code.
--
-- Two ways to close that. Grant `rejection_code` back to the application, or put
-- it where it belongs. It belongs on the transition: **it is the reason this
-- particular transition happened**, the code that says why an order was
-- REJECTED, and the event payload already emits it next to the transition id.
-- Granting it back would have left the audit trail unable to answer "why was
-- this order rejected" from its own rows.
--
-- So the column moves to `order_transitions`, and a CHECK moves with it:
--
--   * a transition to REJECTED must carry a code;
--   * a transition to anything else must not.
--
-- That was a Go-level validation (`REJECTED requires a rejection code`) enforced
-- in one function. It is now a property of the table.
--
-- ## The compare-and-swap moved too, and got stronger
--
-- The UPDATE carried `WHERE id = $1 AND status = $5`, turning a concurrent
-- change into CodeConflict. Every caller of `transitionLocked` already holds
-- `SELECT ... FOR UPDATE` on the row -- checked, all four -- so it was
-- belt-and-braces, and nothing in the repository asserts its message.
--
-- It is preserved here rather than dropped, and moving it makes it apply to
-- every writer instead of to one function. The trigger's UPDATE carries
-- `AND status = NEW.from_status` and raises when it matches nothing, which is
-- also a second, independent refusal of a transition row whose `from_status`
-- does not describe where the order actually is -- the same property 00731's
-- edge binding asserts, now enforced at the moment the row is written rather
-- than at COMMIT.
--
-- ## The grant-back
--
-- `filled_input_quantity` and `filled_output_quantity`, written by the fill path
-- and not by a transition. They also carry the row lock every caller takes.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   order_transitions_flag
--   order_transitions_flag_edge
--   order_transitions_writes_the_status   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001 for both the orphan and the concurrent change. There is
-- no existing custom state for a conflict, and inventing one would need
-- internal/db to learn it; `mapError` already turns AD001 into a refusal, and
-- the message names the condition. The Go call site keeps its own CodeConflict
-- for the case it can still detect.

ALTER TABLE order_transitions ADD COLUMN rejection_code text;

-- NOT VALID, and that is the honest form rather than a shortcut.
--
-- Rows written before this migration have no rejection_code, because the column
-- did not exist. A validating constraint would refuse to be added at all on any
-- database with a historical REJECTED transition, and backfilling from
-- `orders.rejection_code` would be a guess: an order's current code came from
-- whichever transition set it last, which is not necessarily the row being
-- backfilled.
--
-- NOT VALID means "every row written from here on satisfies this". PostgreSQL
-- enforces it on INSERT and UPDATE exactly as a validated constraint does; what
-- it skips is the scan of what is already there. Making the audit trail complete
-- backwards is not something a migration can do truthfully.
ALTER TABLE order_transitions ADD CONSTRAINT order_transitions_rejection_code_check
    CHECK ((to_status = 'REJECTED') = (rejection_code IS NOT NULL AND rejection_code <> '')) NOT VALID;

COMMENT ON COLUMN order_transitions.rejection_code IS
    'Why this transition rejected the order. Required exactly when to_status is REJECTED, so the audit trail can answer the question from its own rows rather than from the order''s current state (00748).';

-- +goose StatementBegin
CREATE FUNCTION cp_order_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    terminal boolean;
BEGIN
    -- OrderStatus.Terminal() in internal/execution/order.go, read from the
    -- code rather than guessed: SETTLED, REJECTED, EXPIRED, CANCELLED,
    -- FAILED_FINAL. FILLED is deliberately NOT terminal -- settlement follows
    -- it -- and a copy of this list that assumed otherwise would stamp
    -- terminal_at on an order that is still moving.
    terminal := NEW.to_status = ANY (ARRAY['SETTLED'::text, 'REJECTED'::text, 'EXPIRED'::text,
                                           'CANCELLED'::text, 'FAILED_FINAL'::text]);
    UPDATE orders
       SET status = NEW.to_status,
           -- Carried forward when this transition does not set one, which is
           -- what the Go code did: a rejection code outlives the REJECTED
           -- transition that produced it.
           rejection_code = coalesce(NEW.rejection_code, rejection_code),
           terminal_at = CASE WHEN terminal THEN coalesce(terminal_at, NEW.occurred_at) ELSE terminal_at END
     WHERE id = NEW.order_id
       AND status = NEW.from_status;
    IF NOT FOUND THEN
        IF NOT EXISTS (SELECT 1 FROM orders WHERE id = NEW.order_id) THEN
            RAISE EXCEPTION 'ORDER_TRANSITION_ORPHANED: order_transitions names order % which does not exist',
                NEW.order_id USING ERRCODE = 'AD001';
        END IF;
        RAISE EXCEPTION 'ORDER_CHANGED_CONCURRENTLY: order % is not in % any more, so a transition from that state does not describe it',
            NEW.order_id, NEW.from_status USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER order_transitions_writes_the_status
    AFTER INSERT ON order_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_order_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_order_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON orders FROM cp_app;
-- The fill path, which is not a transition. These also carry the row lock.
GRANT UPDATE (filled_input_quantity, filled_output_quantity) ON orders TO cp_app;

COMMENT ON FUNCTION cp_order_apply_status_transition() IS
    'Writes orders.status, rejection_code and terminal_at from the transition row, refusing when the row''s from_status no longer describes the order. The application holds UPDATE on the two fill columns only (00748, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
