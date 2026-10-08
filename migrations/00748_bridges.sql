-- +goose Up
-- Bridges and cross-chain capital (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md §3-§7;
-- internal/bridge).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A bridge route joins one SIMULATED source asset on one simulated chain to one SIMULATED
--      destination asset on another, both with the devnet mint as mint_address and equal decimals, through
--      one BRIDGE facility. A source asset belongs to at most one route, so the route's books are the only
--      books of that asset's bridged value. Everything but the route's status is frozen.
--   2. A bridge leg moves only along its state machine (available, reserved, bridge_requested,
--      source_finalized, message_in_flight, destination_observed, destination_finalized, reconciled, failed,
--      manual_review), every change bound to a transition row naming both endpoints. A leg past its source
--      finality names its BRIDGE_SOURCE_FINALIZED journal; a destination-finalized or reconciled leg names its
--      BRIDGE_DESTINATION_FINALIZED journal. Finality never reverts: no edge leads from a finalized state
--      back to an earlier one.
--   3. Every BRIDGE_SOURCE_FINALIZED, BRIDGE_DESTINATION_FINALIZED and BRIDGE_REFUND journal is bound, at
--      COMMIT, to exactly one leg of one route (its customer, its quantity, the route's facility and assets),
--      and leaves the route's books conserving value:
--          BRIDGE_LOCKED(source) = BRIDGE_OUTSTANDING(destination) + Σ customers' BRIDGE_IN_FLIGHT(source)
--      so no unit is ever counted on both chains, or on neither.
--   4. Release attempts (each signed relay or refund transaction) are append-only.
--
-- Custom SQLSTATEs:
--   BR001 an illegal route or leg status transition.
--   BR002 a frozen column of a route, a leg or a release attempt changed.
--   BR003 a bridge journal that breaks the route's conservation identity.
--   BR004 a route inconsistent with the assets, chains or facility it names.
--   BR005 a bridge journal not bound to exactly one leg of one route.

-- ---------------------------------------------------------------------------
-- 1. Routes
-- ---------------------------------------------------------------------------

CREATE TABLE bridge_routes (
    id                    uuid PRIMARY KEY,
    name                  text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    facility_id           uuid NOT NULL UNIQUE REFERENCES facilities(id),
    network               text NOT NULL CHECK (network ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    source_chain          text NOT NULL CHECK (source_chain ~ '^sbx-[a-z0-9-]{1,28}$'),
    source_asset_id       uuid NOT NULL UNIQUE REFERENCES assets(id),
    source_mint           text NOT NULL,
    destination_chain     text NOT NULL CHECK (destination_chain ~ '^sbx-[a-z0-9-]{1,28}$'),
    destination_asset_id  uuid NOT NULL UNIQUE REFERENCES assets(id),
    destination_mint      text NOT NULL,
    decimals              smallint NOT NULL CHECK (decimals BETWEEN 0 AND 18),
    attester              text NOT NULL,
    unit_price_mantissa   numeric(38,0) NOT NULL CHECK (unit_price_mantissa > 0),
    unit_price_scale      integer NOT NULL CHECK (unit_price_scale BETWEEN 0 AND 38),
    max_release_attempts  integer NOT NULL CHECK (max_release_attempts BETWEEN 1 AND 20),
    status                text NOT NULL CHECK (status IN ('ACTIVE','PAUSED','CLOSED')),
    created_by_actor_type text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id   text NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT bridge_routes_two_chains CHECK (source_chain <> destination_chain),
    CONSTRAINT bridge_routes_two_assets CHECK (source_asset_id <> destination_asset_id)
);
CREATE TRIGGER bridge_routes_updated_at BEFORE UPDATE ON bridge_routes FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_bridge_route_consistent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s  record;
    d  record;
    fk text;
BEGIN
    SELECT kind, chain, mint_address, decimals INTO s FROM assets WHERE id = NEW.source_asset_id;
    SELECT kind, chain, mint_address, decimals INTO d FROM assets WHERE id = NEW.destination_asset_id;
    SELECT kind INTO fk FROM facilities WHERE id = NEW.facility_id;
    IF s.kind IS DISTINCT FROM 'SIMULATED' OR s.chain IS DISTINCT FROM NEW.source_chain OR s.mint_address IS DISTINCT FROM NEW.source_mint
       OR s.decimals IS DISTINCT FROM NEW.decimals
       OR d.kind IS DISTINCT FROM 'SIMULATED' OR d.chain IS DISTINCT FROM NEW.destination_chain
       OR d.mint_address IS DISTINCT FROM NEW.destination_mint OR d.decimals IS DISTINCT FROM NEW.decimals
       OR fk IS DISTINCT FROM 'BRIDGE' THEN
        RAISE EXCEPTION 'BRIDGE_ROUTE_INCONSISTENT: route % must join SIMULATED assets on % and % (mints % and %, % decimals) through a BRIDGE facility',
            NEW.id, NEW.source_chain, NEW.destination_chain, NEW.source_mint, NEW.destination_mint, NEW.decimals
            USING ERRCODE = 'BR004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_bridge_route_consistent() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER bridge_routes_consistent BEFORE INSERT ON bridge_routes
    FOR EACH ROW EXECUTE FUNCTION cp_bridge_route_consistent();

-- +goose StatementBegin
CREATE FUNCTION cp_bridge_route_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('ACTIVE','PAUSED'), ('PAUSED','ACTIVE'), ('ACTIVE','CLOSED'), ('PAUSED','CLOSED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_bridge_route_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (to_jsonb(NEW) - ARRAY['status','updated_at']) IS DISTINCT FROM (to_jsonb(OLD) - ARRAY['status','updated_at']) THEN
        RAISE EXCEPTION 'BRIDGE_FROZEN: route % may change status only', OLD.id USING ERRCODE = 'BR002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_bridge_route_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'BRIDGE_TRANSITION_ILLEGAL: route % cannot move % -> %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'BR001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER bridge_routes_guard BEFORE UPDATE ON bridge_routes FOR EACH ROW EXECUTE FUNCTION cp_bridge_route_guard();

CREATE TABLE bridge_route_transitions (
    id           uuid PRIMARY KEY,
    route_id     uuid NOT NULL REFERENCES bridge_routes(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL,
    actor_id     text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) > 0),
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bridge_route_transitions_idx ON bridge_route_transitions (route_id, occurred_at);
CREATE TRIGGER bridge_route_transitions_immutable BEFORE UPDATE OR DELETE ON bridge_route_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER bridge_route_transitions_flag_edge AFTER INSERT ON bridge_route_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('route_id', 'from_status', 'to_status', 'bridge_route_status');
CREATE CONSTRAINT TRIGGER bridge_routes_require_transition AFTER UPDATE OF status ON bridge_routes
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'bridge_route_status');

-- ---------------------------------------------------------------------------
-- 2. Legs
-- ---------------------------------------------------------------------------

CREATE TABLE bridge_legs (
    id                                  uuid PRIMARY KEY,
    route_id                            uuid NOT NULL REFERENCES bridge_routes(id),
    account_id                          uuid NOT NULL REFERENCES accounts(id),
    intent_id                           uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    mode                                text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    quantity                            numeric(38,0) NOT NULL CHECK (quantity > 0),
    source_owner                        text NOT NULL,
    destination_owner                   text NOT NULL,
    lock_nonce                          numeric(20,0) NOT NULL CHECK (lock_nonce >= 0),
    plan                                jsonb NOT NULL,
    plan_hash                           bytea NOT NULL CHECK (length(plan_hash) = 32),
    status                              text NOT NULL CHECK (status IN ('AVAILABLE','RESERVED','BRIDGE_REQUESTED',
                                            'SOURCE_FINALIZED','MESSAGE_IN_FLIGHT','DESTINATION_OBSERVED',
                                            'DESTINATION_FINALIZED','RECONCILED','FAILED','MANUAL_REVIEW')),
    -- Set as the leg advances; never changed once set.
    reservation_id                      uuid REFERENCES asset_reservations(id),
    source_effect_id                    uuid UNIQUE REFERENCES economic_effects(id),
    destination_effect_id               uuid UNIQUE REFERENCES economic_effects(id),
    refund_effect_id                    uuid UNIQUE REFERENCES economic_effects(id),
    lock_tx_id                          text UNIQUE,
    lock_tx                             bytea CHECK (lock_tx IS NULL OR length(lock_tx) BETWEEN 1 AND 4096),
    lock_blockhash                      text,
    lock_blockhash_height               bigint,
    source_journal_transaction_id       uuid UNIQUE REFERENCES journal_transactions(id),
    destination_journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    refund_journal_transaction_id       uuid UNIQUE REFERENCES journal_transactions(id),
    failure_code                        text,
    created_at                          timestamptz NOT NULL DEFAULT now(),
    updated_at                          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (route_id, lock_nonce),
    CONSTRAINT bridge_legs_requested_has_lock CHECK (
        status IN ('AVAILABLE','RESERVED') OR (status = 'FAILED' AND lock_tx_id IS NULL)
        OR (lock_tx_id IS NOT NULL AND lock_tx IS NOT NULL AND lock_blockhash IS NOT NULL AND lock_blockhash_height IS NOT NULL)),
    CONSTRAINT bridge_legs_source_posted CHECK (
        status NOT IN ('SOURCE_FINALIZED','MESSAGE_IN_FLIGHT','DESTINATION_OBSERVED','DESTINATION_FINALIZED','RECONCILED')
        OR source_journal_transaction_id IS NOT NULL),
    CONSTRAINT bridge_legs_destination_posted CHECK (
        status NOT IN ('DESTINATION_FINALIZED','RECONCILED') OR destination_journal_transaction_id IS NOT NULL),
    CONSTRAINT bridge_legs_refund_ends_failed CHECK (refund_journal_transaction_id IS NULL OR status = 'FAILED'),
    CONSTRAINT bridge_legs_failed_has_code CHECK ((status = 'FAILED') = (failure_code IS NOT NULL))
);
CREATE INDEX bridge_legs_open_idx ON bridge_legs (route_id, status) WHERE status NOT IN ('RECONCILED','FAILED');
CREATE INDEX bridge_legs_account_idx ON bridge_legs (account_id, created_at DESC);
CREATE TRIGGER bridge_legs_updated_at BEFORE UPDATE ON bridge_legs FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_bridge_leg_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('AVAILABLE','RESERVED'), ('AVAILABLE','FAILED'),
        ('RESERVED','BRIDGE_REQUESTED'), ('RESERVED','FAILED'),
        ('BRIDGE_REQUESTED','SOURCE_FINALIZED'), ('BRIDGE_REQUESTED','FAILED'),
        ('SOURCE_FINALIZED','MESSAGE_IN_FLIGHT'), ('SOURCE_FINALIZED','MANUAL_REVIEW'),
        ('MESSAGE_IN_FLIGHT','DESTINATION_OBSERVED'), ('MESSAGE_IN_FLIGHT','DESTINATION_FINALIZED'), ('MESSAGE_IN_FLIGHT','MANUAL_REVIEW'),
        ('DESTINATION_OBSERVED','DESTINATION_FINALIZED'), ('DESTINATION_OBSERVED','MESSAGE_IN_FLIGHT'), ('DESTINATION_OBSERVED','MANUAL_REVIEW'),
        ('DESTINATION_FINALIZED','RECONCILED'), ('DESTINATION_FINALIZED','MANUAL_REVIEW'),
        ('MANUAL_REVIEW','MESSAGE_IN_FLIGHT'), ('MANUAL_REVIEW','RECONCILED'), ('MANUAL_REVIEW','FAILED'));
$$;
-- +goose StatementEnd

-- Identity frozen; the advancing columns are set once and never changed (BR002); legal edges only (BR001).
-- +goose StatementBegin
CREATE FUNCTION cp_bridge_leg_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    once text[] := ARRAY['reservation_id','source_effect_id','destination_effect_id','refund_effect_id','lock_tx_id',
                         'lock_tx','lock_blockhash','lock_blockhash_height','source_journal_transaction_id',
                         'destination_journal_transaction_id','refund_journal_transaction_id','failure_code'];
    col  text;
    o    jsonb := to_jsonb(OLD);
    n    jsonb := to_jsonb(NEW);
BEGIN
    IF (n - once - ARRAY['status','updated_at']) IS DISTINCT FROM (o - once - ARRAY['status','updated_at']) THEN
        RAISE EXCEPTION 'BRIDGE_FROZEN: leg % may not change identity', OLD.id USING ERRCODE = 'BR002';
    END IF;
    FOREACH col IN ARRAY once LOOP
        IF o->col IS DISTINCT FROM 'null'::jsonb AND o->col IS DISTINCT FROM n->col THEN
            RAISE EXCEPTION 'BRIDGE_FROZEN: leg % already set %', OLD.id, col USING ERRCODE = 'BR002';
        END IF;
    END LOOP;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_bridge_leg_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'BRIDGE_TRANSITION_ILLEGAL: leg % cannot move % -> %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'BR001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER bridge_legs_guard BEFORE UPDATE ON bridge_legs FOR EACH ROW EXECUTE FUNCTION cp_bridge_leg_guard();

CREATE TABLE bridge_leg_transitions (
    id           uuid PRIMARY KEY,
    leg_id       uuid NOT NULL REFERENCES bridge_legs(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL,
    actor_id     text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) > 0),
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX bridge_leg_transitions_idx ON bridge_leg_transitions (leg_id, occurred_at);
CREATE TRIGGER bridge_leg_transitions_immutable BEFORE UPDATE OR DELETE ON bridge_leg_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER bridge_leg_transitions_flag_edge AFTER INSERT ON bridge_leg_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('leg_id', 'from_status', 'to_status', 'bridge_leg_status');
CREATE CONSTRAINT TRIGGER bridge_legs_require_transition AFTER UPDATE OF status ON bridge_legs
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'bridge_leg_status');

-- Each signed relay (RELEASE on the destination) or refund (a RELEASE on the source that returns units after
-- a destination that never released) transaction, once, in order.
CREATE TABLE bridge_release_attempts (
    id                 uuid PRIMARY KEY,
    leg_id             uuid NOT NULL REFERENCES bridge_legs(id),
    attempt            integer NOT NULL CHECK (attempt >= 1),
    kind               text NOT NULL CHECK (kind IN ('RELEASE','REFUND')),
    chain_id           text NOT NULL CHECK (chain_id ~ '^sbx-[a-z0-9-]{1,28}$'),
    tx_id              text NOT NULL UNIQUE,
    raw_tx             bytea NOT NULL CHECK (length(raw_tx) BETWEEN 1 AND 4096),
    recent_blockhash   text NOT NULL,
    blockhash_height   bigint NOT NULL CHECK (blockhash_height >= 0),
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (leg_id, attempt)
);
CREATE TRIGGER bridge_release_attempts_immutable BEFORE UPDATE OR DELETE ON bridge_release_attempts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 3. The bridge books, at COMMIT, for every bridge journal
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_check_bridge_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k         text;
    ref_type  text;
    ref_id    text;
    flag      text;
    leg       bridge_legs%ROWTYPE;
    rt        bridge_routes%ROWTYPE;
    locked    numeric;
    outst     numeric;
    inflight  numeric;
BEGIN
    SELECT kind, reference_type, reference_id INTO k, ref_type, ref_id FROM journal_transactions WHERE id = NEW.transaction_id;
    IF k IS NULL OR k NOT IN ('BRIDGE_SOURCE_FINALIZED','BRIDGE_DESTINATION_FINALIZED','BRIDGE_REFUND') THEN
        RETURN NULL;
    END IF;
    flag := 'cp.bridge.checked.x' || translate(NEW.transaction_id::text, '-', '_');
    IF current_setting(flag, true) = '1' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config(flag, '1', true);

    IF ref_type IS DISTINCT FROM 'bridge_leg' THEN
        RAISE EXCEPTION 'BRIDGE_JOURNAL_UNBOUND: % transaction % does not reference a bridge leg', k, NEW.transaction_id
            USING ERRCODE = 'BR005';
    END IF;
    SELECT * INTO leg FROM bridge_legs WHERE id::text = ref_id;
    SELECT * INTO rt FROM bridge_routes WHERE id = leg.route_id;
    IF leg.id IS NULL OR rt.id IS NULL THEN
        RAISE EXCEPTION 'BRIDGE_JOURNAL_UNBOUND: % transaction % references no leg', k, NEW.transaction_id USING ERRCODE = 'BR005';
    END IF;
    IF EXISTS (
        SELECT 1 FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.transaction_id
           AND (e.quantity <> leg.quantity
                OR e.asset_id NOT IN (rt.source_asset_id, rt.destination_asset_id)
                OR (la.owner_type = 'FACILITY' AND la.owner_id <> rt.facility_id)
                OR (la.owner_type = 'CUSTOMER' AND la.owner_id <> leg.account_id)
                OR la.owner_type NOT IN ('FACILITY','CUSTOMER'))) THEN
        RAISE EXCEPTION 'BRIDGE_JOURNAL_UNBOUND: % transaction % moves something other than leg % of route %',
            k, NEW.transaction_id, leg.id, rt.id USING ERRCODE = 'BR005';
    END IF;

    SELECT coalesce(sum(b.balance), 0) INTO locked FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
     WHERE la.owner_type = 'FACILITY' AND la.owner_id = rt.facility_id AND la.code = 'BRIDGE_LOCKED' AND la.asset_id = rt.source_asset_id;
    SELECT coalesce(sum(b.balance), 0) INTO outst FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
     WHERE la.owner_type = 'FACILITY' AND la.owner_id = rt.facility_id AND la.code = 'BRIDGE_OUTSTANDING'
       AND la.asset_id = rt.destination_asset_id;
    SELECT coalesce(sum(b.balance), 0) INTO inflight FROM ledger_accounts la JOIN ledger_balances b ON b.ledger_account_id = la.id
     WHERE la.owner_type = 'CUSTOMER' AND la.code = 'BRIDGE_IN_FLIGHT' AND la.asset_id = rt.source_asset_id;
    IF locked <> outst + inflight THEN
        RAISE EXCEPTION 'BRIDGE_CONSERVATION: route % has % locked, % outstanding and % in flight', rt.id, locked, outst, inflight
            USING ERRCODE = 'BR003';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_bridge_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER journal_entries_bridge_rules AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_bridge_journal();

-- ---------------------------------------------------------------------------
-- 4. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON bridge_routes, bridge_route_transitions, bridge_legs, bridge_leg_transitions,
    bridge_release_attempts TO cp_app;
GRANT UPDATE (status) ON bridge_routes TO cp_app;
GRANT UPDATE (status, reservation_id, source_effect_id, destination_effect_id, refund_effect_id, lock_tx_id, lock_tx,
              lock_blockhash, lock_blockhash_height, source_journal_transaction_id, destination_journal_transaction_id,
              refund_journal_transaction_id, failure_code) ON bridge_legs TO cp_app;
GRANT SELECT ON bridge_routes, bridge_route_transitions, bridge_legs, bridge_leg_transitions, bridge_release_attempts
    TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: bridge books and leg history are never dropped
