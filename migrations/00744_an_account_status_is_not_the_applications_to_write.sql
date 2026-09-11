-- +goose Up
-- An account status is not the application's to write.
--
-- F-42's stronger remedy, table two of the eleven, following 00743's pattern
-- exactly. `accounts` next because freezing an account is the control that stops
-- a compromised or abusive one, and because the scoping rule 00743 established
-- picked it: **count the state-change sites first, not the table's importance.**
-- `accounts` has one.
--
-- ## What this does
--
-- `internal/accounts.Transition` writes the transition row and then updates
-- three columns. The AFTER INSERT trigger on `account_status_transitions` now
-- writes all three, from the row:
--
--   status         <- NEW.to_status
--   status_reason  <- NEW.reason
--   frozen_at      <- NEW.occurred_at, only when the destination is FROZEN
--
-- Every one of them is already carried by the transition row, which is why this
-- table is cheaper than 00743 was: nothing had to be derived, only moved.
--
-- ## One column is granted back, and not for writing
--
-- The plain REVOKE was written first and broke four integration packages with
-- `accounts: lock: permission denied for table accounts`. The cause is worth
-- recording because it is not obvious and it will recur on every remaining
-- table:
--
--   **`SELECT ... FOR UPDATE` requires UPDATE privilege.** So do FOR NO KEY
--   UPDATE, FOR SHARE and FOR KEY SHARE -- all four were probed here and all
--   four are refused with no UPDATE grant at all.
--
-- `accounts.Transition` locks the row before checking the transition is legal,
-- and that lock is what makes two concurrent transitions produce a clean
-- CONFLICT rather than an audit row recording a `from_status` that was already
-- superseded. It is worth keeping.
--
-- A COLUMN-level grant is enough for the lock and still refuses the write --
-- probed, not assumed:
--
--   GRANT UPDATE (cost_basis_method) -> SELECT ... FOR UPDATE succeeds
--                                    -> UPDATE ... SET status  still refused
--
-- `cost_basis_method` is the column because **nothing in this repository ever
-- updates it**, checked rather than assumed, so granting it writes nothing that
-- matters. It is here to permit a row lock, not a write, and a reader who finds
-- it later should not conclude the application is meant to set it.
--
-- `updated_at` is set by a BEFORE UPDATE trigger, and a trigger writing NEW does
-- not need a column privilege, so it keeps working with no grant.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
-- PostgreSQL fires a statement's AFTER ROW triggers in NAME order, and this one
-- must run after both flag setters:
--
--   account_status_transitions_flag
--   account_status_transitions_flag_edge
--   account_status_transitions_writes_the_status    <- 'w' sorts after 'f'
--
-- Under a DEFERRED audit check the order does not matter. Under
-- `SET CONSTRAINTS ALL IMMEDIATE` it does, and the restore drill does exactly
-- that on this table -- it drives an accounts transition and forces the check
-- immediately, which is the one place in this repository that would catch a
-- wrong name. Renaming this trigger to something alphabetically earlier would
-- pass every deferred test and fail the drill.
--
-- ## What remains
--
-- Nine tables. `agents` is the next by site count (one) but writes seven columns
-- in that one statement, including the promotion evidence, so it is a bigger
-- piece than its count suggests -- which is the limit of the counting rule and
-- worth knowing before trusting it.
--
-- And every one of them will hit the row-lock rule above, because locking the
-- row before checking a transition is the pattern this repository uses
-- everywhere. Budget a column grant per table, and say in the migration that it
-- is for the lock.

-- +goose StatementBegin
CREATE FUNCTION cp_account_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE accounts
       SET status = NEW.to_status,
           status_reason = NEW.reason,
           -- Set on the way in and never cleared on the way out, which is what
           -- the Go code did: frozen_at records that a freeze happened, not
           -- that one is current. `status` answers the second question.
           frozen_at = CASE WHEN NEW.to_status = 'FROZEN' THEN NEW.occurred_at ELSE frozen_at END
     WHERE id = NEW.account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ACCOUNT_TRANSITION_ORPHANED: account_status_transitions names account % which does not exist',
            NEW.account_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER account_status_transitions_writes_the_status
    AFTER INSERT ON account_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_account_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_account_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON accounts FROM cp_app;
-- Not for writing. See the header: a row lock needs UPDATE privilege, and this
-- is the narrowest column that grants one on a table nothing writes it on.
GRANT UPDATE (cost_basis_method) ON accounts TO cp_app;

COMMENT ON FUNCTION cp_account_apply_status_transition() IS
    'Writes accounts.status, status_reason and frozen_at from the transition row. The application holds UPDATE on cost_basis_method only, and that grant exists to permit a row lock rather than a write, so inserting the transition row is the only way an account status changes (00744, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
