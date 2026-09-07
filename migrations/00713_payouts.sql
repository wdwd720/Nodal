-- +goose Up
-- Payout requests, destinations and provider settlement (gola.md PARTS XVIII-XXI).
--
-- WHAT A PAYOUT IS HERE. A user asks to convert eligible internal Credits into external value. Nodal
-- does not send money: an approved provider does. Nodal decides eligibility, reserves the exact
-- units, hands the provider a request with a stable idempotency key, and reconciles the answer.
--
-- THE ECONOMIC SHAPE, and why it is three ledger movements rather than one:
--
--   INTERNAL_CREDIT -> PAYOUT_PENDING     reserve. Gated on PAYOUT_RESERVE. The user's Credits stop
--                                         being spendable the moment the request is real, so the
--                                         same value cannot fund a market trade while a payout for
--                                         it is in flight.
--   PAYOUT_PENDING  -> EXTERNAL_SETTLED   settle. Gated on PAYOUT_SETTLE. Only after a provider says
--                                         the money is irrevocably gone.
--   PAYOUT_PENDING  -> INTERNAL_CREDIT    return. DELIBERATELY UNGATED. If PAYOUT_SETTLE is revoked
--                                         mid-flight, the value in PAYOUT_PENDING still has to be
--                                         able to get back to the user; a capability check here
--                                         would strand it (PART XXXII).
--
-- WHAT PAYOUT_STATUS_UNKNOWN IS FOR. PART XXI is explicit: when a provider result is uncertain, do
-- not release the reservation and retry blindly. A submission that timed out may have succeeded.
-- The request goes to PAYOUT_STATUS_UNKNOWN, the reservation STAYS, and reconciliation resolves it
-- by asking the provider about the idempotency key Nodal chose before it ever called. That key is
-- written to the database BEFORE the provider is contacted, so a crash between the write and the
-- call still leaves something to reconcile against.
--
-- Custom SQLSTATEs: PO001 reservation invariant, PO002 illegal state transition, PO003 provenance
-- mismatch, PO004 amount mismatch.

-- ---------------------------------------------------------------------------
-- 1. Destinations
-- ---------------------------------------------------------------------------

-- A destination is where value would go. It is verified separately from the account, because owning
-- an account and controlling a bank account are different facts.
CREATE TABLE payout_destinations (
    id                 uuid PRIMARY KEY,
    account_id         uuid NOT NULL REFERENCES accounts(id),
    kind               text NOT NULL CHECK (kind IN ('BANK','CARD_PUSH','FIAT_WALLET','CRYPTO_WALLET')),
    provider           text NOT NULL,
    -- The provider's handle for the destination. Nodal stores a reference, never the underlying
    -- account number or key: PART LXXXIV says not to duplicate identity data the provider already
    -- holds, and a bank account number in this table is a liability with no compensating benefit.
    provider_reference text NOT NULL,
    display_label      text NOT NULL DEFAULT '',
    currency           text,
    status             text NOT NULL CHECK (status IN ('UNVERIFIED','VERIFIED','REJECTED','DISABLED')),
    verified_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_reference)
);
CREATE INDEX payout_destinations_account_idx ON payout_destinations (account_id, created_at DESC);
CREATE TRIGGER payout_destinations_updated_at BEFORE UPDATE ON payout_destinations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 2. Requests
-- ---------------------------------------------------------------------------

CREATE TABLE payout_requests (
    id                     uuid PRIMARY KEY,
    account_id             uuid NOT NULL REFERENCES accounts(id),
    destination_id         uuid REFERENCES payout_destinations(id),
    credit_asset_id        uuid NOT NULL REFERENCES assets(id),

    state                  text NOT NULL CHECK (state IN (
                               'DRAFT','ELIGIBILITY_CHECK','VERIFICATION_REQUIRED','VERIFICATION_PENDING',
                               'VERIFIED','SUBMITTED','PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN',
                               'SETTLED','FAILED','REJECTED','REVERSED','MANUAL_REVIEW')),

    requested_quantity     numeric(38,0) NOT NULL CHECK (requested_quantity > 0),
    reserved_quantity      numeric(38,0) NOT NULL DEFAULT 0 CHECK (reserved_quantity >= 0),
    settled_quantity       numeric(38,0) NOT NULL DEFAULT 0 CHECK (settled_quantity >= 0),

    -- The decision that authorised this payout, kept verbatim. A decision made in March must still
    -- be explicable in June after the policy has changed twice.
    policy_version         text NOT NULL,
    policy_hash            text NOT NULL,
    eligibility_reasons    jsonb NOT NULL DEFAULT '[]'::jsonb,
    verification_level     text NOT NULL DEFAULT 'NONE',

    -- Chosen by Nodal BEFORE the provider is called, so a crash between writing this row and making
    -- the call still leaves a key to reconcile against (PART XXXVIII).
    provider               text,
    provider_idempotency_key text UNIQUE,
    provider_reference     text,
    provider_status        text,

    idempotency_key        text NOT NULL UNIQUE,
    reserved_at            timestamptz,
    submitted_at           timestamptz,
    settled_at             timestamptz,
    failure_reason         text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),

    -- A settled payout settles what it reserved, never more.
    CHECK (settled_quantity <= reserved_quantity),
    -- A reservation never exceeds the request.
    CHECK (reserved_quantity <= requested_quantity)
);
CREATE INDEX payout_requests_account_idx ON payout_requests (account_id, created_at DESC);
-- The states a reconciler has to sweep: value is committed and the outcome is not yet final.
CREATE INDEX payout_requests_open_idx ON payout_requests (state, submitted_at)
    WHERE state IN ('SUBMITTED','PROVIDER_PENDING','PAYOUT_STATUS_UNKNOWN','MANUAL_REVIEW');
CREATE TRIGGER payout_requests_updated_at BEFORE UPDATE ON payout_requests
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE payout_request_transitions (
    id             uuid PRIMARY KEY,
    request_id     uuid NOT NULL REFERENCES payout_requests(id),
    from_state     text NOT NULL,
    to_state       text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL,
    provider_event text,
    correlation_id text,
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payout_request_transitions_idx ON payout_request_transitions (request_id, occurred_at);
CREATE TRIGGER payout_request_transitions_immutable BEFORE UPDATE OR DELETE ON payout_request_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER payout_request_transitions_flag AFTER INSERT ON payout_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition('request_id', 'to_state', 'payout_requests');
CREATE CONSTRAINT TRIGGER payout_requests_require_transition AFTER UPDATE OF state ON payout_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_require_transition('state');

-- ---------------------------------------------------------------------------
-- 3. Which units a payout reserved
-- ---------------------------------------------------------------------------

-- A payout does not reserve "500 Credits". It reserves specific units from specific provenance lots,
-- so that cancelling it returns exactly what it took. Returning a bare quantity would let a user
-- launder a promotional grant into an earning by reserving a payout and cancelling it.
CREATE TABLE payout_allocations (
    id           uuid PRIMARY KEY,
    request_id   uuid NOT NULL REFERENCES payout_requests(id),
    lot_id       uuid NOT NULL REFERENCES credit_lots(id),
    origin       text NOT NULL,
    quantity     numeric(38,0) NOT NULL CHECK (quantity > 0),
    returned     boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (request_id, lot_id)
);
CREATE INDEX payout_allocations_request_idx ON payout_allocations (request_id);
-- Append-only except for the return flag, which is the one fact that legitimately changes.
-- +goose StatementBegin
CREATE FUNCTION cp_payout_allocation_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'PAYOUT_ALLOCATION_IMMUTABLE: allocations are never deleted' USING ERRCODE = 'PO003';
    END IF;
    IF NEW.request_id IS DISTINCT FROM OLD.request_id
    OR NEW.lot_id     IS DISTINCT FROM OLD.lot_id
    OR NEW.origin     IS DISTINCT FROM OLD.origin
    OR NEW.quantity   IS DISTINCT FROM OLD.quantity THEN
        RAISE EXCEPTION 'PAYOUT_ALLOCATION_IMMUTABLE: only the returned flag may change' USING ERRCODE = 'PO003';
    END IF;
    IF OLD.returned AND NOT NEW.returned THEN
        RAISE EXCEPTION 'PAYOUT_ALLOCATION_IMMUTABLE: returned units cannot be un-returned' USING ERRCODE = 'PO003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER payout_allocations_immutable BEFORE UPDATE OR DELETE ON payout_allocations
    FOR EACH ROW EXECUTE FUNCTION cp_payout_allocation_immutable();

-- The allocations must add up to what the request says it reserved. Checked at commit, because the
-- allocations are written after the request row in the same transaction.
-- +goose StatementBegin
CREATE FUNCTION cp_payout_reservation_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    req      payout_requests%ROWTYPE;
    alloc    numeric;
BEGIN
    SELECT * INTO req FROM payout_requests WHERE id = NEW.request_id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    SELECT coalesce(sum(quantity), 0) INTO alloc
      FROM payout_allocations WHERE request_id = NEW.request_id AND NOT returned;
    IF req.reserved_quantity <> alloc THEN
        RAISE EXCEPTION 'PAYOUT_RESERVATION_UNBALANCED: request % reserves % but its outstanding allocations total %',
            req.id, req.reserved_quantity, alloc USING ERRCODE = 'PO001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
CREATE CONSTRAINT TRIGGER payout_allocations_balanced AFTER INSERT OR UPDATE ON payout_allocations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_payout_reservation_balanced();

-- ---------------------------------------------------------------------------
-- 4. Provider evidence
-- ---------------------------------------------------------------------------

-- Every provider interaction is recorded raw, before it is interpreted. A provider that sends
-- contradictory statuses (PART LXXII item 23) has to be arguable against something, and the
-- something is this table.
CREATE TABLE payout_provider_events (
    id              uuid PRIMARY KEY,
    request_id      uuid REFERENCES payout_requests(id),
    provider        text NOT NULL,
    direction       text NOT NULL CHECK (direction IN ('REQUEST','RESPONSE','WEBHOOK')),
    provider_status text,
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    received_at     timestamptz NOT NULL DEFAULT now(),
    -- A webhook the provider re-delivers must be recorded once. Null for outbound requests.
    provider_event_id text,
    UNIQUE (provider, provider_event_id)
);
CREATE INDEX payout_provider_events_request_idx ON payout_provider_events (request_id, received_at);
CREATE TRIGGER payout_provider_events_immutable BEFORE UPDATE OR DELETE ON payout_provider_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 5. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE ON payout_destinations, payout_requests TO cp_app;
GRANT SELECT, INSERT ON payout_request_transitions, payout_provider_events, payout_allocations TO cp_app;
GRANT UPDATE (returned) ON payout_allocations TO cp_app;
GRANT SELECT ON payout_destinations, payout_requests, payout_request_transitions,
                payout_allocations, payout_provider_events TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: payout history is financial history and is never dropped by rollback
