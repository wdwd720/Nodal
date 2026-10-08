-- +goose Up
-- Robust batch upkeep for the advanced subsystems (internal/capital, orders, equities, predmarket, delegation,
-- bridge, stableasset, reconciliation; the scheduled jobs of internal/advancedjobs). Amends 00731, 00752 and
-- 00753 without editing them.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A batch upkeep operation (expire due reservations, complete finished orders, settle due equity fills,
--      close due prediction markets, reconcile pending delegated subjects, observe bridge legs, resolve stable
--      asset submissions, pay queued redemptions) remembers the items it could not process: upkeep_failures
--      holds one row per (operation, item) -- how many times in a row it failed, when it first and last failed,
--      its last error code and the earliest time it may be tried again. The backoff is the database's policy,
--      in one place (cp_upkeep_backoff): 1, 2, 4, 8, 16, 32 and then 60 minutes after each consecutive failure.
--      A success clears the row (cleared_at); nothing is ever deleted, so the table is the history of what
--      failed and when it recovered. An item that keeps failing is therefore tried less and less often and is
--      never in front of the items behind it; the operations select an item only once its backoff has passed
--      (or the clock has gone back past its last failure) and take never-failed items first.
--   2. cp_upkeep_check_deferred() runs every pending deferred check of the transaction now and keeps the
--      deferrable constraints deferred afterwards. An operation that processes several items in one caller's
--      transaction calls it at the end of each item, inside the item's savepoint: a refusal the database would
--      only make at COMMIT (a transition binding, a journal check) is the item's own failure, rolled back with
--      the item's savepoint, instead of the COMMIT failure of every item processed with it. Every deferrable
--      constraint of this schema is INITIALLY DEFERRED, so "deferred afterwards" is exactly how each one
--      started; a check is only ever run earlier than COMMIT, never skipped.
--   3. A delegated swap or withdrawal records the devnet network (devnethost.Config.Network) it was admitted on
--      and will be signed, submitted and observed on. A new row without one is refused (UK002); the column is
--      frozen like every other identity column by the guards of 00752 and 00753. Rows that existed before this
--      migration are attributed where the chain history proves it: a signed transaction to the one network
--      whose history holds its SUBMIT, and every row to the database's only network when it has only ever had
--      one. A row that cannot be attributed keeps NULL: it is never observed or signed, and a pending one is
--      ended (its funds released) by expiry.
--   4. The order book's journal proof and the customer transfer reconciliation have reconciliation kinds of
--      their own, BOOK_JOURNAL and TRANSFERS, instead of LEDGER_INTERNAL. Their unresolved records written
--      under LEDGER_INTERNAL before this migration take the new kind, so the next pass refreshes them instead of
--      opening a second record; resolved records are history and keep the kind they were resolved under.
--
-- Custom SQLSTATEs (prefix UK): UK001 an upkeep failure's identity (id, operation, item_id, created_at)
-- changed; UK002 a delegated swap or withdrawal born without the devnet network it belongs to.

-- ---------------------------------------------------------------------------
-- 1. The upkeep failure ledger
-- ---------------------------------------------------------------------------

CREATE TABLE upkeep_failures (
    id               uuid PRIMARY KEY,
    -- The batch operation, as <package>.<operation> (capital.expire_due, delegation.observe_swap ...).
    operation        text NOT NULL CHECK (length(operation) <= 64 AND operation ~ '^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$'),
    -- The item the operation could not process: a reservation, an order's intent, a settlement, a market, a
    -- delegated swap or withdrawal, a bridge leg, a mint or burn request.
    item_id          uuid NOT NULL,
    -- Consecutive failures since the item last succeeded (or since it first failed).
    failures         integer NOT NULL CHECK (failures >= 1),
    first_failed_at  timestamptz NOT NULL,
    last_failed_at   timestamptz NOT NULL,
    -- The item is not tried again before this.
    retry_after      timestamptz NOT NULL,
    -- The errs code of the last failure (INTERNAL for anything that is not a refusal): never its text.
    last_code        text NOT NULL CHECK (last_code ~ '^[A-Z][A-Z0-9_]{0,63}$'),
    -- Set when the item was processed after failing; a later failure starts a new run of failures.
    cleared_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT upkeep_failures_one_per_item UNIQUE (operation, item_id),
    CONSTRAINT upkeep_failures_backoff_ahead CHECK (retry_after > last_failed_at)
);
CREATE INDEX upkeep_failures_open_idx ON upkeep_failures (operation, retry_after) WHERE cleared_at IS NULL;
CREATE TRIGGER upkeep_failures_updated_at BEFORE UPDATE ON upkeep_failures FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_upkeep_failure_identity_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.operation IS DISTINCT FROM OLD.operation
       OR NEW.item_id IS DISTINCT FROM OLD.item_id OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'UPKEEP_FAILURE_FROZEN: upkeep failure % may not change its operation or item', OLD.id
            USING ERRCODE = 'UK001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_upkeep_failure_identity_frozen() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER upkeep_failures_identity_frozen BEFORE UPDATE ON upkeep_failures
    FOR EACH ROW EXECUTE FUNCTION cp_upkeep_failure_identity_frozen();

-- The backoff after the n-th consecutive failure: 1, 2, 4, 8, 16, 32, then 60 minutes.
-- +goose StatementBegin
CREATE FUNCTION cp_upkeep_backoff(p_failures integer) RETURNS interval
LANGUAGE sql IMMUTABLE AS $$
    SELECT least(60, 1 << least(greatest(p_failures, 1) - 1, 6)) * interval '1 minute';
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_upkeep_backoff(integer) SET search_path = pg_catalog, public, pg_temp;

-- Records that p_operation could not process p_item at p_at with error code p_code, and returns how many
-- times in a row it has now failed and when it may be tried again. p_id names the row when it is the item's
-- first; an existing row keeps its id.
-- +goose StatementBegin
CREATE FUNCTION cp_upkeep_failed(p_id uuid, p_operation text, p_item uuid, p_at timestamptz, p_code text)
RETURNS TABLE (failures integer, retry_after timestamptz)
LANGUAGE sql AS $$
    INSERT INTO upkeep_failures AS f (id, operation, item_id, failures, first_failed_at, last_failed_at, retry_after, last_code)
    VALUES (p_id, p_operation, p_item, 1, p_at, p_at, p_at + cp_upkeep_backoff(1), p_code)
    ON CONFLICT (operation, item_id) DO UPDATE SET
        failures        = CASE WHEN f.cleared_at IS NULL THEN f.failures + 1 ELSE 1 END,
        first_failed_at = CASE WHEN f.cleared_at IS NULL THEN f.first_failed_at ELSE EXCLUDED.first_failed_at END,
        last_failed_at  = EXCLUDED.last_failed_at,
        retry_after     = EXCLUDED.last_failed_at
                          + cp_upkeep_backoff(CASE WHEN f.cleared_at IS NULL THEN f.failures + 1 ELSE 1 END),
        last_code       = EXCLUDED.last_code,
        cleared_at      = NULL
    RETURNING f.failures, f.retry_after;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_upkeep_failed(uuid, text, uuid, timestamptz, text) SET search_path = pg_catalog, public, pg_temp;

-- Records that p_operation processed p_item at p_at: a run of failures, if there was one, is over.
-- +goose StatementBegin
CREATE FUNCTION cp_upkeep_succeeded(p_operation text, p_item uuid, p_at timestamptz) RETURNS void
LANGUAGE sql AS $$
    UPDATE upkeep_failures SET cleared_at = p_at
     WHERE operation = p_operation AND item_id = p_item AND cleared_at IS NULL;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_upkeep_succeeded(text, uuid, timestamptz) SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 2. Running an item's deferred checks before its savepoint is released
-- ---------------------------------------------------------------------------

-- SET CONSTRAINTS ALL IMMEDIATE runs every pending deferred check now and raises the first refusal; inside a
-- savepoint, a refusal rolls back with it and leaves the checks of everything outside it pending. SET
-- CONSTRAINTS ALL DEFERRED then returns every deferrable constraint to deferred, which is how each one of this
-- schema starts (all are INITIALLY DEFERRED), so the statements that follow are checked exactly as before.
-- +goose StatementBegin
CREATE FUNCTION cp_upkeep_check_deferred() RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
    SET CONSTRAINTS ALL IMMEDIATE;
    SET CONSTRAINTS ALL DEFERRED;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_upkeep_check_deferred() SET search_path = pg_catalog, public, pg_temp;

GRANT SELECT, INSERT ON upkeep_failures TO cp_app;
GRANT UPDATE (failures, first_failed_at, last_failed_at, retry_after, last_code, cleared_at, updated_at)
    ON upkeep_failures TO cp_app;
GRANT SELECT ON upkeep_failures TO cp_readonly, cp_ops;
REVOKE ALL ON FUNCTION cp_upkeep_failed(uuid, text, uuid, timestamptz, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION cp_upkeep_succeeded(text, uuid, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION cp_upkeep_check_deferred() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION cp_upkeep_failed(uuid, text, uuid, timestamptz, text) TO cp_app;
GRANT EXECUTE ON FUNCTION cp_upkeep_succeeded(text, uuid, timestamptz) TO cp_app;
GRANT EXECUTE ON FUNCTION cp_upkeep_check_deferred() TO cp_app;

-- ---------------------------------------------------------------------------
-- 3. The network of a delegated swap or withdrawal
-- ---------------------------------------------------------------------------

ALTER TABLE delegated_swaps ADD COLUMN network text
    CONSTRAINT delegated_swaps_network_format CHECK (network IS NULL OR network ~ '^[a-z0-9][a-z0-9-]{0,62}$');
ALTER TABLE delegated_withdrawals ADD COLUMN network text
    CONSTRAINT delegated_withdrawals_network_format CHECK (network IS NULL OR network ~ '^[a-z0-9][a-z0-9-]{0,62}$');

-- Attribute the rows written before this migration. The guards freeze every column they do not name, so they
-- stand aside for exactly these two statements per table, in this transaction.
ALTER TABLE delegated_swaps DISABLE TRIGGER delegated_swaps_guard;
ALTER TABLE delegated_withdrawals DISABLE TRIGGER delegated_withdrawals_guard;

UPDATE delegated_swaps s SET network = e.network
  FROM (SELECT chain_id, tx_id, min(network) AS network FROM devnet_events
         WHERE kind = 'SUBMIT' GROUP BY chain_id, tx_id HAVING count(DISTINCT network) = 1) e
 WHERE s.network IS NULL AND s.tx_id IS NOT NULL AND e.chain_id = s.chain_id AND e.tx_id = s.tx_id;
UPDATE delegated_swaps SET network = (SELECT min(network) FROM devnet_events)
 WHERE network IS NULL AND (SELECT count(DISTINCT network) FROM devnet_events) = 1;

UPDATE delegated_withdrawals w SET network = e.network
  FROM (SELECT chain_id, tx_id, min(network) AS network FROM devnet_events
         WHERE kind = 'SUBMIT' GROUP BY chain_id, tx_id HAVING count(DISTINCT network) = 1) e
 WHERE w.network IS NULL AND w.tx_id IS NOT NULL AND e.chain_id = w.chain_id AND e.tx_id = w.tx_id;
UPDATE delegated_withdrawals SET network = (SELECT min(network) FROM devnet_events)
 WHERE network IS NULL AND (SELECT count(DISTINCT network) FROM devnet_events) = 1;

ALTER TABLE delegated_swaps ENABLE TRIGGER delegated_swaps_guard;
ALTER TABLE delegated_withdrawals ENABLE TRIGGER delegated_withdrawals_guard;

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_network_recorded() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.network IS NULL THEN
        RAISE EXCEPTION 'DELEGATION_NETWORK_REQUIRED: % % must name the devnet network it is signed and observed on',
            TG_TABLE_NAME, NEW.id USING ERRCODE = 'UK002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_network_recorded() SET search_path = pg_catalog, public, pg_temp;
-- Named to sort before the birth triggers of 00752 and 00753 (triggers on one event fire in name order), so a
-- row without a network is refused for that before anything else is checked.
CREATE TRIGGER delegated_swaps_admit_network BEFORE INSERT ON delegated_swaps
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_network_recorded();
CREATE TRIGGER delegated_withdrawals_admit_network BEFORE INSERT ON delegated_withdrawals
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_network_recorded();

CREATE INDEX delegated_swaps_network_open_idx ON delegated_swaps (network, status, created_at)
    WHERE status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED','UNKNOWN');
CREATE INDEX delegated_withdrawals_network_open_idx ON delegated_withdrawals (network, status, created_at)
    WHERE status IN ('AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED','UNKNOWN');

-- ---------------------------------------------------------------------------
-- 4. Reconciliation kinds of their own for the order-book proof and the transfer register
-- ---------------------------------------------------------------------------

ALTER TABLE reconciliation_records DROP CONSTRAINT reconciliation_records_kind_check;
ALTER TABLE reconciliation_records ADD CONSTRAINT reconciliation_records_kind_check CHECK (kind IN (
    'EXECUTION','WALLET_BALANCE','FUNDING','LEDGER_INTERNAL','POSITION_LEDGER','SUBMISSION_UNKNOWN',
    'ECONOMIC_EFFECT','STABLE_RESERVE','BRIDGE','PREDICTION_ESCROW','CLEARING','DEVNET_CHAIN',
    'BOOK_JOURNAL','TRANSFERS'));

UPDATE reconciliation_records SET kind = 'BOOK_JOURNAL'
 WHERE kind = 'LEDGER_INTERNAL' AND scope_type = 'order_book'
   AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED');
UPDATE reconciliation_records SET kind = 'TRANSFERS'
 WHERE kind = 'LEDGER_INTERNAL' AND scope_type = 'transfers' AND scope_id = 'register'
   AND status IN ('OPEN','MISMATCH','INVESTIGATING','ESCALATED');

-- +goose Down
SELECT 1; -- protected: the network a delegated transaction was signed for and the reconciliation kinds of recorded findings are financial history
