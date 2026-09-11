-- +goose Up
-- A wallet status is not the application's to write.
--
-- F-42's stronger remedy, table three of eleven, following 00743 and 00744.
-- `wallets` next because it is the cheapest of the nine that remain: its status
-- update sets exactly one column, and that column is `to_status` verbatim.
--
-- A wallet's status is what says whether it may sign. REVOKED is the answer to
-- a compromised delegation, and a status write with no transition row behind it
-- is a revocation that never happened or a re-activation nobody approved.
--
-- ## What this does
--
-- The AFTER INSERT trigger on `wallet_status_transitions` writes
-- `wallets.status` from `NEW.to_status`. Nothing else moves, because nothing
-- else was in that statement.
--
-- ## The grant-back, which is a real write and not only a lock
--
-- `MarkDelegationVerified` writes four columns that have nothing to do with a
-- status change and no home on the transition row:
--
--   delegation_ref, signing_policy_version, delegation_verified_at, capabilities
--
-- Its own comment says so: "It is not a status change and needs no transition
-- row. An unverified status clears nothing: revocation is a status transition to
-- REVOKED." That is the right split and it is preserved exactly.
--
-- Those four also satisfy 00744's row-lock rule -- `Repository.Transition` takes
-- `SELECT ... FOR UPDATE`, which needs UPDATE privilege, and a column-level
-- grant supplies it. So unlike `accounts`, no column had to be granted purely
-- for the lock: the columns the application genuinely writes already cover it.
--
-- ## The trigger name is load-bearing, for the reason 00743 and 00744 give
--
--   wallet_status_transitions_flag
--   wallet_status_transitions_flag_edge
--   wallet_status_transitions_writes_the_status   <- 'w' sorts after 'f'
--
-- PostgreSQL fires a statement's AFTER ROW triggers in NAME order and the flag
-- setters must run first. Deferred, the order is irrelevant; under
-- `SET CONSTRAINTS ALL IMMEDIATE` it is not.

-- +goose StatementBegin
CREATE FUNCTION cp_wallet_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE wallets SET status = NEW.to_status WHERE id = NEW.wallet_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'WALLET_TRANSITION_ORPHANED: wallet_status_transitions names wallet % which does not exist',
            NEW.wallet_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER wallet_status_transitions_writes_the_status
    AFTER INSERT ON wallet_status_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_wallet_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_wallet_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON wallets FROM cp_app;
-- The delegation columns, which are a real write: MarkDelegationVerified is not
-- a status change and deliberately has no transition row. They also carry the
-- row lock Transition needs, so no column is granted here purely for locking.
GRANT UPDATE (delegation_ref, signing_policy_version, delegation_verified_at, capabilities)
    ON wallets TO cp_app;

COMMENT ON FUNCTION cp_wallet_apply_status_transition() IS
    'Writes wallets.status from the transition row. The application holds UPDATE on the four delegation columns only, so inserting the transition row is the only way a wallet status changes (00745, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
