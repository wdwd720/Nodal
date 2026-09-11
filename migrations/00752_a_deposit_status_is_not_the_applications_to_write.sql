-- +goose Up
-- A deposit status is not the application's to write.
--
-- F-42's stronger remedy, table sixteen of seventeen. One remains:
-- `kill_switches`.
--
-- ## This is the one where the revoke buys the least, and it is still worth it
--
-- Every other table in this series moved most of its writes onto the transition
-- row. `deposits` cannot: the update is built dynamically from a patch of
-- **fifteen** optional columns -- provider session ids and references, the
-- amounts a provider reported, chain signatures and slots, fraud state,
-- eligibility flags, the journal transactions a settlement produced -- and not
-- one of them is a fact about the transition. They are facts about the deposit
-- that a sweep or a webhook learned, and they arrive on their own schedule.
--
-- So fifteen columns are granted back, and the revoke buys exactly two things:
-- **`status`, and the thirteen timestamps that record when each status was
-- reached.** That is the smallest yield in this series.
--
-- It is still the point. `status` is what decides whether money is available,
-- withdrawable or reversed, and a status write with no transition row is a
-- deposit made available with nothing on the record saying why. The other
-- fifteen columns are evidence; this one is authority.
--
-- ## The thirteen stamps, from internal/funding's own map
--
-- `timestampColumns` (internal/funding/status.go) maps each status to the column
-- that records reaching it, and the Go code wrote `to.TimestampColumn()` in the
-- same statement as the status. The CASE below is that map, transcribed. It is
-- the fourth list this series has had to copy out of Go, and like the others it
-- was read from the source rather than reconstructed.
--
-- `created_at` is in that map and is deliberately NOT in the CASE. A deposit is
-- INSERTed with status CREATED and `created_at` set in the same statement; there
-- is no transition into CREATED, so a branch for it would be dead code that
-- looked like coverage.
--
-- ## No compare-and-swap here
--
-- Unlike `orders` and `reconciliation_records`, this UPDATE carried no
-- `AND status = $N`. `get(lock=true)` is the concurrency control. Adding a CAS
-- would be a new behaviour rather than a moved one.
--
-- ## The trigger name is load-bearing, for the reason 00743 gives
--
--   deposit_transitions_flag
--   deposit_transitions_flag_edge
--   deposit_transitions_writes_the_status   <- 'w' sorts after 'f'
--
-- Custom SQLSTATE: AD001, as in 00743 through 00751.

-- +goose StatementBegin
CREATE FUNCTION cp_deposit_apply_status_transition() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    -- internal/funding's timestampColumns, transcribed. Every stamp is
    -- coalesced: re-entering a status must not restate when it was first
    -- reached, which is what `coalesce(..., $3)` did on the Go side for the
    -- stamps that could repeat.
    UPDATE deposits
       SET status = NEW.to_status,
           session_created_at      = CASE WHEN NEW.to_status = 'SESSION_CREATED'            THEN coalesce(session_created_at, NEW.occurred_at)      ELSE session_created_at END,
           customer_action_at      = CASE WHEN NEW.to_status = 'CUSTOMER_ACTION_REQUIRED'   THEN coalesce(customer_action_at, NEW.occurred_at)      ELSE customer_action_at END,
           provider_processing_at  = CASE WHEN NEW.to_status = 'PROVIDER_PROCESSING'        THEN coalesce(provider_processing_at, NEW.occurred_at)  ELSE provider_processing_at END,
           provider_confirmed_at   = CASE WHEN NEW.to_status = 'PROVIDER_CONFIRMED'         THEN coalesce(provider_confirmed_at, NEW.occurred_at)   ELSE provider_confirmed_at END,
           settlement_observed_at  = CASE WHEN NEW.to_status = 'SETTLEMENT_OBSERVED'        THEN coalesce(settlement_observed_at, NEW.occurred_at)  ELSE settlement_observed_at END,
           reconciled_at           = CASE WHEN NEW.to_status = 'RECONCILED'                 THEN coalesce(reconciled_at, NEW.occurred_at)           ELSE reconciled_at END,
           available_at            = CASE WHEN NEW.to_status = 'AVAILABLE'                  THEN coalesce(available_at, NEW.occurred_at)            ELSE available_at END,
           failed_at               = CASE WHEN NEW.to_status = 'FAILED'                     THEN coalesce(failed_at, NEW.occurred_at)               ELSE failed_at END,
           expired_at              = CASE WHEN NEW.to_status = 'EXPIRED'                    THEN coalesce(expired_at, NEW.occurred_at)              ELSE expired_at END,
           cancelled_at            = CASE WHEN NEW.to_status = 'CANCELLED'                  THEN coalesce(cancelled_at, NEW.occurred_at)            ELSE cancelled_at END,
           reversed_at             = CASE WHEN NEW.to_status = 'REVERSED'                   THEN coalesce(reversed_at, NEW.occurred_at)             ELSE reversed_at END,
           review_required_at      = CASE WHEN NEW.to_status = 'REVIEW_REQUIRED'            THEN coalesce(review_required_at, NEW.occurred_at)      ELSE review_required_at END
     WHERE id = NEW.deposit_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'DEPOSIT_TRANSITION_ORPHANED: deposit_transitions names deposit % which does not exist',
            NEW.deposit_id USING ERRCODE = 'AD001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd

CREATE TRIGGER deposit_transitions_writes_the_status
    AFTER INSERT ON deposit_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_deposit_apply_status_transition();

REVOKE EXECUTE ON FUNCTION cp_deposit_apply_status_transition() FROM PUBLIC;

REVOKE UPDATE ON deposits FROM cp_app;
-- The patch: fifteen columns a sweep or a webhook learns about the deposit,
-- none of which is a fact about a transition. They also carry the row lock.
GRANT UPDATE (
    provider_session_id, provider_ref, fiat_amount_minor, fiat_currency,
    expected_quantity, observed_quantity, tx_signature, chain_slot,
    fraud_state, reversible_until, buying_power_eligible, withdrawal_eligible,
    availability_policy_version, journal_transaction_id, reversal_journal_transaction_id
) ON deposits TO cp_app;

COMMENT ON FUNCTION cp_deposit_apply_status_transition() IS
    'Writes deposits.status and the timestamp its destination records, from the transition row. The application holds UPDATE on the fifteen evidence columns only: those are what a provider reported, this is what the deposit IS (00752, F-42).';

-- +goose Down
SELECT 1; -- protected: reverting returns the status column to the application's reach
