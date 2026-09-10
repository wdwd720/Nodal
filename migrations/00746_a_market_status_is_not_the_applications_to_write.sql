-- +goose Up
-- A market status is not the application's to write.
--
-- F-42's stronger remedy, table four of eleven, after 00743 credit_fundings,
-- 00744 accounts and 00745 wallets.
--
-- `native_markets.status` is what decides whether a market may be traded. A
-- status write with no transition row behind it is a market opened or halted
-- with nothing on the record saying who did it or why -- and this table's status
-- is mirrored into the instrument registry by the same call, so a forged status
-- would propagate to what the platform believes is tradeable.
--
-- ## What this does
--
-- The AFTER INSERT trigger on `native_market_transitions` writes `status`, and
-- stamps `activated_at` on the way to ACTIVE. Both come off the transition row.
--
-- **`activated_at` is coalesced, and that is not a style choice.**
-- `cp_native_market_curve_frozen` (00712) raises NM003 if `activated_at`
-- changes once it is set, along with the reserves, the fee basis points and
-- both asset ids. A trigger that wrote `activated_at = NEW.occurred_at`
-- unconditionally would make re-entering ACTIVE raise NM003 and look like a
-- frozen-curve violation, which is the one error on this table that means
-- something else entirely. The Go code had the same `m.ActivatedAt == nil`
-- guard for the same reason.
--
-- ## The grant-back is for the lock and for nothing else
--
-- Unlike `wallets`, nothing in this repository updates `native_markets` outside
-- a status transition -- checked, not assumed. But `Repository.Get` takes
-- `SELECT ... FOR UPDATE`, and 00744 established that a row lock requires
-- UPDATE privilege, so a plain REVOKE would break the lock.
--
-- `virtual_credit_reserve` is the column, and it is the safest possible choice
-- **because `cp_native_market_curve_frozen` already refuses to let it change on
-- any activated market.** The grant permits a lock; the trigger permits no
-- write. On a market that has not yet activated the grant is a real capability,
-- which is why it is named here rather than left to be discovered: nothing
-- writes it, and a reader who finds this grant should not conclude the
-- application is meant to move a bonding curve.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   native_market_transitions_flag
--   native_market_transitions_flag_edge
--   native_market_transitions_writes_the_status   <- 'w' sorts after 'f'

-- +goose StatementBegin
CREATE FUNCTION cp_native_market_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE native_markets
       SET status = NEW.to_status,
           -- coalesce, or re-entering ACTIVE raises NM003 from the frozen-curve
           -- guard and reports a bonding-curve violation that did not happen.
           activated_at = CASE WHEN NEW.to_status = 'ACTIVE'
                               THEN coalesce(activated_at, NEW.occurred_at) ELSE activated_at END
     WHERE id = NEW.market_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'MARKET_TRANSITION_ORPHANED: native_market_transitions names market % which does not exist',
            NEW.market_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER native_market_transitions_writes_the_status
    AFTER INSERT ON native_market_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_native_market_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_native_market_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON native_markets FROM cp_app;
-- Not for writing. See the header: a row lock needs UPDATE privilege, and
-- cp_native_market_curve_frozen already refuses this column on any activated
-- market, so the grant buys a lock and no capability that matters.
GRANT UPDATE (virtual_credit_reserve) ON native_markets TO cp_app;

COMMENT ON FUNCTION cp_native_market_apply_status_transition() IS
    'Writes native_markets.status and stamps activated_at from the transition row. The application holds UPDATE on virtual_credit_reserve only, to permit a row lock rather than a write, so inserting the transition row is the only way a market status changes (00746, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
