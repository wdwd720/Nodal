-- +goose Up
-- A funding state is not the application's to write.
--
-- F-42's stronger remedy, applied to the first of the eleven tables that still
-- grant `cp_app` blanket UPDATE. 00741 made the audit binding unforgeable, which
-- is detection: the application may still write the column, and is refused at
-- COMMIT if it cannot show a transition row. This is privilege: the application
-- cannot write the column at all, and the transition row IS the state change.
--
-- `credit_fundings` first because it is the one whose forgery costs money -- the
-- state machine decides when Credits are minted against captured payment -- and
-- because it is the cheapest of the eleven to do correctly. Exactly one place in
-- the repository changes a funding's state, and it already writes the transition
-- row immediately before.
--
-- ## The shape
--
-- The AFTER INSERT trigger on `credit_funding_transitions` now performs the
-- state change and the stamp that belongs to the destination. It runs SECURITY
-- DEFINER, so it has the privilege the application has just lost. `cp_app` keeps
-- UPDATE on exactly two columns, which are the two it legitimately writes
-- outside a transition:
--
--   lot_id             -- MintFrom, after the state has already moved
--   provider_reference -- recorded once, when the provider first names it
--
-- Everything else on this table is now written by the transition or not at all.
--
-- ## Why the stamps moved too, and this is the better half of the change
--
-- `reversible_at`, `settled_at`, `reversed_at` and `failure_reason` were set by
-- the same UPDATE the application issued, in a switch on the destination state.
-- That is a rule the caller had to remember. `reversible_at` in particular is
-- what the settlement window is measured from, so a transition that moved the
-- state and forgot the stamp would put a funding into REVERSIBLE with no clock
-- running on it -- money that never becomes settled and never reverses.
--
-- Moving them here makes the stamp part of the transition rather than something
-- next to it. There is no longer a way to reach REVERSIBLE without
-- `reversible_at`.
--
-- ## The trigger name is load-bearing
--
-- PostgreSQL fires a statement's AFTER ROW triggers in NAME order, and this one
-- must run after both flag setters:
--
--   credit_funding_transitions_flag
--   credit_funding_transitions_flag_edge
--   credit_funding_transitions_writes_the_state     <- 'w' sorts after 'f'
--
-- With the audit check DEFERRED the order does not matter, because the check
-- runs at COMMIT by which time the flags are set. It matters under
-- `SET CONSTRAINTS ALL IMMEDIATE`, where the check fires during this trigger's
-- UPDATE -- and something in this repository does exactly that, so the ordering
-- is a requirement rather than a tidiness. The name is the mechanism; renaming
-- it to something alphabetically earlier would break the binding under an
-- immediate check and pass every deferred test.
--
-- ## What this does not do
--
-- Ten tables still grant `cp_app` blanket UPDATE. This is one, done as the
-- worked example the register asked for, and `kill_switches` is deliberately not
-- next: its single write sets `active` together with six other columns under
-- optimistic concurrency, so moving one column into a trigger breaks the version
-- check and the returned row at once.

-- +goose StatementBegin
CREATE FUNCTION cp_credit_funding_apply_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE credit_fundings
       SET state = NEW.to_state,
           -- coalesce, not now(): re-entering REVERSIBLE must not restart the
           -- settlement window, which is the same rule the Go code had.
           reversible_at = CASE WHEN NEW.to_state = 'REVERSIBLE'
                                THEN coalesce(reversible_at, NEW.occurred_at) ELSE reversible_at END,
           settled_at    = CASE WHEN NEW.to_state = 'SETTLED'
                                THEN NEW.occurred_at ELSE settled_at END,
           reversed_at   = CASE WHEN NEW.to_state IN ('REVERSED', 'REFUNDED')
                                THEN NEW.occurred_at ELSE reversed_at END,
           failure_reason = CASE WHEN NEW.to_state = 'FAILED'
                                THEN NEW.reason ELSE failure_reason END
     WHERE id = NEW.funding_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'FUNDING_TRANSITION_ORPHANED: credit_funding_transitions names funding % which does not exist',
            NEW.funding_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER credit_funding_transitions_writes_the_state
    AFTER INSERT ON credit_funding_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_credit_funding_apply_transition();

REVOKE EXECUTE ON FUNCTION cp_credit_funding_apply_transition() FROM PUBLIC;

REVOKE UPDATE ON credit_fundings FROM cp_app;
GRANT UPDATE (lot_id, provider_reference) ON credit_fundings TO cp_app;

COMMENT ON FUNCTION cp_credit_funding_apply_transition() IS
    'Writes credit_fundings.state and the stamp its destination carries, from the transition row. The application holds no UPDATE on those columns, so inserting the transition row is the only way a funding state changes (00743, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the state column to the application's reach
