-- +goose Up
-- A native asset status is not the application's to write.
--
-- F-42's stronger remedy, table five of eleven, after 00743 credit_fundings,
-- 00744 accounts, 00745 wallets and 00746 native_markets.
--
-- `native_assets.status` is what decides whether a creator's asset is live, and
-- ACTIVE is the moment its economics stop being editable. A status write with no
-- transition row is an asset made live with nothing on the record saying who
-- made it live -- and, because activation is also what locks the economics, it is
-- the moment a supply cap or a creator allocation stops being changeable.
--
-- ## Two call sites collapse into one trigger
--
-- `SetStatus` wrote `status`. `Activate` wrote `status`, `activated_at` and
-- `economics_locked_at` in a single statement, with a comment saying why:
-- "economics_locked_at is set in the same statement as the status, so there is
-- no instant in which the asset is live and still editable."
--
-- That property is now stronger than the comment claimed. It was true of one
-- statement issued by one function; it is now true of the table, because the
-- only way to reach ACTIVE is a transition row and the trigger writes all three
-- together. A second call site cannot forget.
--
-- **Both stamps stay coalesced.** `cp_native_asset_economics_frozen` (00712)
-- raises NM003 if `economics_locked_at` moves once set, so re-activating after
-- a halt must not restamp it -- exactly the reason the Go code coalesced.
--
-- ## This table's primary key is not `id`
--
-- It is `asset_id`, and the transition binding already knows:
-- `cp_require_transition_edge('status','native_assets','asset_id')` passes the
-- third argument for exactly this reason (00731). The trigger below matches on
-- `asset_id` and a copy of this migration for another table must not carry that
-- over blindly.
--
-- ## The grant-back
--
-- `SetModeration` writes `content_moderation_state` and `moderation_notes`.
-- Neither is on the transition row and neither is a status change: moderating
-- content is a different decision from listing an asset, and the schema keeps
-- them apart. Those two also carry the row lock `Repository.Get` takes, so
-- nothing is granted here purely for locking.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   native_asset_transitions_flag
--   native_asset_transitions_flag_edge
--   native_asset_transitions_writes_the_status   <- 'w' sorts after 'f'

-- +goose StatementBegin
CREATE FUNCTION cp_native_asset_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    UPDATE native_assets
       SET status = NEW.to_status,
           activated_at = CASE WHEN NEW.to_status = 'ACTIVE'
                               THEN coalesce(activated_at, NEW.occurred_at) ELSE activated_at END,
           -- Set with the status and never moved again: re-activating after a
           -- halt must not reopen the economics, and NM003 says so if it tries.
           economics_locked_at = CASE WHEN NEW.to_status = 'ACTIVE'
                               THEN coalesce(economics_locked_at, NEW.occurred_at) ELSE economics_locked_at END
     WHERE asset_id = NEW.asset_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'NATIVE_ASSET_TRANSITION_ORPHANED: native_asset_transitions names asset % which does not exist',
            NEW.asset_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER native_asset_transitions_writes_the_status
    AFTER INSERT ON native_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_native_asset_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_native_asset_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON native_assets FROM cp_app;
-- Moderation is a real write and a different decision from listing. These two
-- also carry the row lock, so nothing here is granted purely for locking.
GRANT UPDATE (content_moderation_state, moderation_notes) ON native_assets TO cp_app;

COMMENT ON FUNCTION cp_native_asset_apply_status_transition() IS
    'Writes native_assets.status and stamps activated_at and economics_locked_at from the transition row. The application holds UPDATE on the two moderation columns only, so inserting the transition row is the only way an asset status changes (00747, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
