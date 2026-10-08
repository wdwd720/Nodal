-- +goose Up
-- Delegated agent signing (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md §2 and §5-§7, goal §16;
-- Go: internal/delegation). Capability AGENT_SIGNING, modes SANDBOX and DEVNET.
--
-- An agent never holds a key. A customer grants one of the account's own agents a capability; the agent
-- asks for a signature only through Agent -> Intent -> Risk Kernel V2 -> Capital Reservation -> Settlement
-- Plan -> Transaction Inspector -> Signing Policy -> Signing Provider, and the provider validates the
-- capability again, from these rows, before it signs.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A delegated capability is a grant by the user who owns a customer account to one of that account's
--      own agents, for that agent's own strategy. It is born ACTIVE, carries a content hash and a unique
--      nonce, and is frozen except for its one possible change, ACTIVE -> REVOKED, which is bound to a
--      transition row and records who revoked it and why. No role can widen, extend or re-target a
--      capability: every term column is frozen by trigger and the application role holds no UPDATE
--      privilege on any of them.
--   2. Every use of a capability (a delegated swap, a withdrawal proposal) writes one usage row, and the
--      database admits it only while the capability is ACTIVE and valid at the time of use, for an action
--      class in its scope, by its own agent on its own account, and within its single, daily (UTC day) and
--      lifetime notional limits. Admissions for one capability are serialized on the capability's row and
--      each one writes that row, so two concurrent uses can never together exceed a limit at any isolation
--      level: the second waits and then counts the first (READ COMMITTED) or fails to serialize.
--   3. A delegated swap is admitted for exactly one usage, capital reservation and economic intent of its
--      own account, agent and capability, on a chain and against a venue inside the capability's chain and
--      destination allowlists. Its plan, amounts and parties are frozen from birth; what it gains as it
--      advances (compiled message, signed transaction, effect, journal) is set once; it moves only along
--      an acyclic state machine whose every step is bound to a transition row naming both endpoints.
--   4. A swap reaches APPROVED or CANCELLED only with the matching human decision bound to its plan hash,
--      and SIGNED only with an ACTIVE capability valid at the signing time, the human approval its
--      capability requires, and an APPROVED signing decision over the exact message it signed. There is
--      one signing decision per swap: the boundary decides once.
--   5. A SETTLED swap names the SIM_TRADE journal that moved exactly its amounts between exactly the
--      customer and the venue's house book, and the APPLIED effect that applied that journal.
--   6. Signer keys are public keys only. The provider derives every private key from a seed it holds and
--      stores nothing secret in the database.
--
-- Custom SQLSTATEs:
--   DG001 an illegal capability or swap status transition, or a transition row describing one.
--   DG002 a frozen column of a capability, signer key, venue or swap changed, or a set-once column changed.
--   DG003 a capability, usage, swap, approval or signing decision that crosses an account boundary or
--         disagrees with the rows it names.
--   DG004 a capability used outside its grant: not ACTIVE or not valid at the time of use, outside its
--         scope, or beyond its single, daily or lifetime notional limit.
--   DG005 a swap that reaches APPROVED, CANCELLED or SIGNED without what licenses it.
--   DG006 a SETTLED swap whose journal or effect is not exactly its SIM_TRADE.

-- ---------------------------------------------------------------------------
-- 0. Shared helpers
-- ---------------------------------------------------------------------------

-- A devnet address is the canonical base58 form of 32 bytes.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_addresses_valid(p text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT coalesce(bool_and(a IS NOT NULL AND a ~ '^[1-9A-HJ-NP-Za-km-z]{32,44}$'), false) FROM unnest(p) AS a;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 1. Capabilities
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_capabilities (
    id                        uuid PRIMARY KEY,
    account_id                uuid NOT NULL REFERENCES accounts(id),
    agent_id                  uuid NOT NULL REFERENCES agents(id),
    strategy_id               uuid NOT NULL REFERENCES strategies(id),
    environment               text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    mode                      text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    instrument_classes        text[] NOT NULL,
    action_classes            text[] NOT NULL,
    max_notional_minor        bigint NOT NULL,
    max_daily_notional_minor  bigint NOT NULL,
    max_single_trade_minor    bigint NOT NULL,
    destination_allowlist     text[] NOT NULL,
    chain_allowlist           text[] NOT NULL,
    granted_at                timestamptz NOT NULL,
    expires_at                timestamptz NOT NULL,
    nonce                     text NOT NULL UNIQUE CHECK (nonce ~ '^[0-9a-f]{64}$'),
    requires_human_approval   boolean NOT NULL,
    content_hash              bytea NOT NULL UNIQUE CHECK (length(content_hash) = 32),
    idempotency_key           text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    status                    text NOT NULL CHECK (status IN ('ACTIVE','REVOKED')),
    granted_by_actor_type     text NOT NULL CHECK (granted_by_actor_type = 'USER'),
    granted_by_actor_id       text NOT NULL,
    revoked_by_actor_type     text CHECK (revoked_by_actor_type IN ('USER','OPERATOR','SYSTEM')),
    revoked_by_actor_id       text,
    revocation_reason         text CHECK (revocation_reason IS NULL OR length(revocation_reason) BETWEEN 1 AND 500),
    revoked_at                timestamptz,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key),
    CONSTRAINT delegated_capabilities_instrument_classes CHECK (cardinality(instrument_classes) >= 1
        AND instrument_classes <@ ARRAY['STABLE_VALUE','SPOT_TOKEN','SPECULATIVE_TOKEN']::text[]),
    CONSTRAINT delegated_capabilities_action_classes CHECK (cardinality(action_classes) >= 1
        AND action_classes <@ ARRAY['ACQUIRE_EXPOSURE','REDUCE_EXPOSURE','PROPOSE_WITHDRAWAL']::text[]),
    CONSTRAINT delegated_capabilities_chains CHECK (cardinality(chain_allowlist) >= 1
        AND chain_allowlist <@ ARRAY['sbx-solana','sbx-ethereum']::text[]),
    CONSTRAINT delegated_capabilities_destinations CHECK (cardinality(destination_allowlist) BETWEEN 1 AND 64
        AND cp_delegation_addresses_valid(destination_allowlist)),
    CONSTRAINT delegated_capabilities_limits CHECK (max_single_trade_minor > 0
        AND max_single_trade_minor <= max_daily_notional_minor AND max_daily_notional_minor <= max_notional_minor),
    CONSTRAINT delegated_capabilities_lifetime CHECK (expires_at > granted_at AND expires_at <= granted_at + interval '90 days'),
    CONSTRAINT delegated_capabilities_revocation CHECK (
        (status = 'REVOKED') = (revoked_at IS NOT NULL)
        AND (revoked_at IS NULL) = (revoked_by_actor_type IS NULL)
        AND (revoked_at IS NULL) = (revoked_by_actor_id IS NULL)
        AND (revoked_at IS NULL) = (revocation_reason IS NULL))
);
CREATE INDEX delegated_capabilities_account_idx ON delegated_capabilities (account_id, created_at DESC);
CREATE INDEX delegated_capabilities_agent_idx ON delegated_capabilities (agent_id);
CREATE TRIGGER delegated_capabilities_updated_at BEFORE UPDATE ON delegated_capabilities
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A grant is the account owner's, to the account's own agent, for that agent's strategy, born ACTIVE.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_capability_birth() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    agent_account  uuid;
    agent_strategy uuid;
    owner          uuid;
    acct_kind      text;
BEGIN
    IF NEW.status IS DISTINCT FROM 'ACTIVE' OR NEW.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_CAPABILITY_NOT_BORN_ACTIVE: capability % must be granted ACTIVE and unrevoked', NEW.id
            USING ERRCODE = 'DG001';
    END IF;
    SELECT account_id, strategy_id INTO agent_account, agent_strategy FROM agents WHERE id = NEW.agent_id;
    SELECT owner_user_id, kind INTO owner, acct_kind FROM accounts WHERE id = NEW.account_id;
    IF agent_account IS DISTINCT FROM NEW.account_id OR agent_strategy IS DISTINCT FROM NEW.strategy_id
       OR acct_kind IS DISTINCT FROM 'CUSTOMER' OR owner::text IS DISTINCT FROM NEW.granted_by_actor_id THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: capability % must be granted by the owner of customer account % to one of its own agents for that agent''s strategy',
            NEW.id, NEW.account_id USING ERRCODE = 'DG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_capability_birth() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_capabilities_birth BEFORE INSERT ON delegated_capabilities
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_capability_birth();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_capability_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('NONE','ACTIVE'), ('ACTIVE','REVOKED'));
$$;
-- +goose StatementEnd

-- Terms are frozen; the revocation columns are written once, together with ACTIVE -> REVOKED; a USER
-- revocation is the account owner's.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_capability_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    revocation text[] := ARRAY['revoked_by_actor_type','revoked_by_actor_id','revocation_reason','revoked_at'];
    owner      uuid;
BEGIN
    IF (to_jsonb(NEW) - revocation - ARRAY['status','updated_at']) IS DISTINCT FROM
       (to_jsonb(OLD) - revocation - ARRAY['status','updated_at']) THEN
        RAISE EXCEPTION 'DELEGATION_CAPABILITY_FROZEN: capability % can only be revoked, never widened, extended or re-targeted', OLD.id
            USING ERRCODE = 'DG002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_delegation_capability_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: capability % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'DG001';
    END IF;
    IF OLD.status = NEW.status AND (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
        RAISE EXCEPTION 'DELEGATION_CAPABILITY_FROZEN: capability % revocation details change only with the revocation', OLD.id
            USING ERRCODE = 'DG002';
    END IF;
    IF NEW.status = 'REVOKED' AND OLD.status = 'ACTIVE' AND NEW.revoked_by_actor_type = 'USER' THEN
        SELECT owner_user_id INTO owner FROM accounts WHERE id = NEW.account_id;
        IF owner::text IS DISTINCT FROM NEW.revoked_by_actor_id THEN
            RAISE EXCEPTION 'DELEGATION_BOUNDARY: only the owner of account % may revoke capability % as a user', NEW.account_id, NEW.id
                USING ERRCODE = 'DG003';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_capability_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_capabilities_guard BEFORE UPDATE ON delegated_capabilities
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_capability_guard();

CREATE TABLE delegated_capability_transitions (
    id             uuid PRIMARY KEY,
    capability_id  uuid NOT NULL REFERENCES delegated_capabilities(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delegated_capability_transitions_idx ON delegated_capability_transitions (capability_id, occurred_at);
CREATE TRIGGER delegated_capability_transitions_immutable BEFORE UPDATE OR DELETE ON delegated_capability_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_capability_transition_row_legal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT cp_delegation_capability_transition_legal(NEW.from_status, NEW.to_status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: a transition row cannot describe capability % moving % -> %',
            NEW.capability_id, NEW.from_status, NEW.to_status USING ERRCODE = 'DG001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER delegated_capability_transitions_legal BEFORE INSERT ON delegated_capability_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_capability_transition_row_legal();
-- A capability changes status at most once in its life, so the single-edge binding of 00726 suffices.
CREATE TRIGGER delegated_capability_transitions_flag_edge AFTER INSERT ON delegated_capability_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('capability_id', 'from_status', 'to_status', 'delegated_capability_status');
CREATE CONSTRAINT TRIGGER delegated_capabilities_require_transition AFTER UPDATE OF status ON delegated_capabilities
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'delegated_capability_status');

-- ---------------------------------------------------------------------------
-- 2. Signer keys (public halves only)
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_signer_keys (
    id          uuid PRIMARY KEY,
    account_id  uuid NOT NULL REFERENCES accounts(id),
    chain_id    text NOT NULL CHECK (chain_id IN ('sbx-solana','sbx-ethereum')),
    public_key  text NOT NULL UNIQUE CHECK (cp_delegation_addresses_valid(ARRAY[public_key])),
    derivation  text NOT NULL CHECK (derivation = 'hmac-sha256/delegated-signer/v1'),
    created_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, chain_id)
);
CREATE TRIGGER delegated_signer_keys_immutable BEFORE UPDATE OR DELETE ON delegated_signer_keys
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 3. Venues: the sandbox counterparties a delegated swap settles against, each backed by a house book
-- ---------------------------------------------------------------------------

CREATE TABLE delegation_venues (
    id                     uuid PRIMARY KEY,
    name                   text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    chain_id               text NOT NULL CHECK (chain_id IN ('sbx-solana','sbx-ethereum')),
    address                text NOT NULL CHECK (cp_delegation_addresses_valid(ARRAY[address])),
    house_account_id       uuid NOT NULL REFERENCES house_accounts(id),
    spread_bps             integer NOT NULL CHECK (spread_bps BETWEEN 0 AND 1000),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain_id, address)
);
CREATE TRIGGER delegation_venues_immutable BEFORE UPDATE OR DELETE ON delegation_venues
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 4. Capability usage: one row per use, admitted only inside the grant
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_capability_usage (
    id              uuid PRIMARY KEY,
    capability_id   uuid NOT NULL REFERENCES delegated_capabilities(id),
    account_id      uuid NOT NULL REFERENCES accounts(id),
    agent_id        uuid NOT NULL REFERENCES agents(id),
    subject_kind    text NOT NULL CHECK (subject_kind IN ('SWAP','WITHDRAWAL')),
    subject_id      uuid NOT NULL,
    action_class    text NOT NULL CHECK (action_class IN ('ACQUIRE_EXPOSURE','REDUCE_EXPOSURE','PROPOSE_WITHDRAWAL')),
    notional_minor  bigint NOT NULL CHECK (notional_minor > 0),
    used_at         timestamptz NOT NULL,
    usage_day       date NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subject_kind, subject_id),
    CONSTRAINT delegated_capability_usage_day CHECK (usage_day = (used_at AT TIME ZONE 'UTC')::date)
);
CREATE INDEX delegated_capability_usage_day_idx ON delegated_capability_usage (capability_id, usage_day);
CREATE TRIGGER delegated_capability_usage_immutable BEFORE UPDATE OR DELETE ON delegated_capability_usage
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The capability's row is locked AND written (a status write that changes nothing), so a concurrent
-- admission for the same capability either waits and then counts this one, or -- under REPEATABLE READ
-- or SERIALIZABLE -- fails to serialize. Locking alone would not make a REPEATABLE READ writer retry.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_usage_admit() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap        delegated_capabilities%ROWTYPE;
    day_total  numeric;
    all_total  numeric;
BEGIN
    SELECT * INTO cap FROM delegated_capabilities WHERE id = NEW.capability_id FOR UPDATE;
    UPDATE delegated_capabilities SET status = status WHERE id = NEW.capability_id;
    IF cap.id IS NULL OR cap.account_id IS DISTINCT FROM NEW.account_id OR cap.agent_id IS DISTINCT FROM NEW.agent_id
       OR (NEW.subject_kind = 'WITHDRAWAL') IS DISTINCT FROM (NEW.action_class = 'PROPOSE_WITHDRAWAL') THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: usage % of capability % is not by its own agent on its own account', NEW.id, NEW.capability_id
            USING ERRCODE = 'DG003';
    END IF;
    IF cap.status IS DISTINCT FROM 'ACTIVE' OR NEW.used_at < cap.granted_at OR NEW.used_at >= cap.expires_at THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: capability % is % and valid from % to %, not at %',
            cap.id, cap.status, cap.granted_at, cap.expires_at, NEW.used_at USING ERRCODE = 'DG004';
    END IF;
    IF NOT (NEW.action_class = ANY (cap.action_classes)) THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: capability % does not grant %', cap.id, NEW.action_class USING ERRCODE = 'DG004';
    END IF;
    IF NEW.notional_minor > cap.max_single_trade_minor THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: % exceeds the single-trade limit % of capability %',
            NEW.notional_minor, cap.max_single_trade_minor, cap.id USING ERRCODE = 'DG004';
    END IF;
    SELECT coalesce(sum(notional_minor), 0) INTO day_total FROM delegated_capability_usage
     WHERE capability_id = cap.id AND usage_day = NEW.usage_day;
    SELECT coalesce(sum(notional_minor), 0) INTO all_total FROM delegated_capability_usage WHERE capability_id = cap.id;
    IF day_total + NEW.notional_minor > cap.max_daily_notional_minor THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: % on % would bring capability % to % against a daily limit of %',
            NEW.notional_minor, NEW.usage_day, cap.id, day_total + NEW.notional_minor, cap.max_daily_notional_minor
            USING ERRCODE = 'DG004';
    END IF;
    IF all_total + NEW.notional_minor > cap.max_notional_minor THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: % would bring capability % to % against a lifetime limit of %',
            NEW.notional_minor, cap.id, all_total + NEW.notional_minor, cap.max_notional_minor USING ERRCODE = 'DG004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_usage_admit() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_capability_usage_admit BEFORE INSERT ON delegated_capability_usage
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_usage_admit();

-- ---------------------------------------------------------------------------
-- 5. Delegated swaps
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_swaps (
    id                      uuid PRIMARY KEY,
    capability_id           uuid NOT NULL REFERENCES delegated_capabilities(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    agent_id                uuid NOT NULL REFERENCES agents(id),
    request_nonce           numeric(20,0) NOT NULL CHECK (request_nonce > 0),
    intent_id               uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    usage_id                uuid NOT NULL UNIQUE REFERENCES delegated_capability_usage(id),
    reservation_id          uuid NOT NULL UNIQUE REFERENCES asset_reservations(id),
    venue_id                uuid NOT NULL REFERENCES delegation_venues(id),
    mode                    text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    action_class            text NOT NULL CHECK (action_class IN ('ACQUIRE_EXPOSURE','REDUCE_EXPOSURE')),
    chain_id                text NOT NULL CHECK (chain_id IN ('sbx-solana','sbx-ethereum')),
    wallet                  text NOT NULL CHECK (cp_delegation_addresses_valid(ARRAY[wallet])),
    pay_asset_id            uuid NOT NULL REFERENCES assets(id),
    receive_asset_id        uuid NOT NULL REFERENCES assets(id),
    amount_in               numeric(38,0) NOT NULL CHECK (amount_in > 0),
    amount_out              numeric(38,0) NOT NULL CHECK (amount_out > 0),
    min_amount_out          numeric(38,0) NOT NULL CHECK (min_amount_out > 0),
    notional_usd_minor      bigint NOT NULL CHECK (notional_usd_minor > 0),
    requested_at            timestamptz NOT NULL,
    quote_expires_at        timestamptz NOT NULL,
    requires_approval       boolean NOT NULL,
    plan                    jsonb NOT NULL,
    plan_hash               bytea NOT NULL UNIQUE CHECK (length(plan_hash) = 32),
    status                  text NOT NULL CHECK (status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED',
                                                            'UNKNOWN','SETTLED','REJECTED','CANCELLED','FAILED')),
    -- Set once as the swap advances; never changed once set (DG002).
    message                 bytea CHECK (message IS NULL OR length(message) BETWEEN 1 AND 4096),
    recent_blockhash        text,
    blockhash_height        bigint CHECK (blockhash_height IS NULL OR blockhash_height >= 0),
    signed_at               timestamptz,
    tx_id                   text UNIQUE,
    signed_tx               bytea CHECK (signed_tx IS NULL OR length(signed_tx) BETWEEN 1 AND 4096),
    effect_id               uuid UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    failure_code            text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (capability_id, request_nonce),
    CONSTRAINT delegated_swaps_two_assets CHECK (pay_asset_id <> receive_asset_id),
    CONSTRAINT delegated_swaps_slippage_bound CHECK (min_amount_out <= amount_out),
    CONSTRAINT delegated_swaps_quote_window CHECK (quote_expires_at > requested_at),
    CONSTRAINT delegated_swaps_signed_has_tx CHECK (
        status IN ('ADMITTED','AWAITING_APPROVAL','APPROVED','REJECTED','CANCELLED')
        OR (tx_id IS NOT NULL AND signed_tx IS NOT NULL AND message IS NOT NULL AND signed_at IS NOT NULL
            AND recent_blockhash IS NOT NULL AND blockhash_height IS NOT NULL AND effect_id IS NOT NULL)),
    CONSTRAINT delegated_swaps_settled_has_journal CHECK ((status = 'SETTLED') = (journal_transaction_id IS NOT NULL)),
    CONSTRAINT delegated_swaps_failure_code CHECK ((status IN ('REJECTED','CANCELLED','FAILED')) = (failure_code IS NOT NULL))
);
CREATE INDEX delegated_swaps_open_idx ON delegated_swaps (status, created_at)
    WHERE status NOT IN ('SETTLED','REJECTED','CANCELLED','FAILED');
CREATE INDEX delegated_swaps_account_idx ON delegated_swaps (account_id, created_at DESC);
CREATE INDEX delegated_swaps_venue_idx ON delegated_swaps (venue_id, status);
CREATE TRIGGER delegated_swaps_updated_at BEFORE UPDATE ON delegated_swaps FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A swap is born ADMITTED, for its own capability's account and agent, with its own usage, reservation and
-- intent, on a chain and against a venue inside the capability's allowlists.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_birth() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap  delegated_capabilities%ROWTYPE;
    u    delegated_capability_usage%ROWTYPE;
    v    delegation_venues%ROWTYPE;
    r    record;
    i    record;
    pa   record;
    ra   record;
BEGIN
    IF NEW.status IS DISTINCT FROM 'ADMITTED' OR NEW.message IS NOT NULL OR NEW.tx_id IS NOT NULL OR NEW.signed_tx IS NOT NULL
       OR NEW.effect_id IS NOT NULL OR NEW.journal_transaction_id IS NOT NULL OR NEW.failure_code IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_SWAP_NOT_BORN_ADMITTED: swap % must be admitted with no outcome', NEW.id USING ERRCODE = 'DG001';
    END IF;
    SELECT * INTO cap FROM delegated_capabilities WHERE id = NEW.capability_id;
    SELECT * INTO u FROM delegated_capability_usage WHERE id = NEW.usage_id;
    SELECT * INTO v FROM delegation_venues WHERE id = NEW.venue_id;
    SELECT account_id, asset_id, quantity, economic_intent_id INTO r FROM asset_reservations WHERE id = NEW.reservation_id;
    SELECT account_id, kind, actor_type, agent_id, delegation_id, asset_id, quantity INTO i
      FROM economic_intents WHERE id = NEW.intent_id;
    SELECT kind, chain, status INTO pa FROM assets WHERE id = NEW.pay_asset_id;
    SELECT kind, chain, status INTO ra FROM assets WHERE id = NEW.receive_asset_id;
    IF cap.account_id IS DISTINCT FROM NEW.account_id OR cap.agent_id IS DISTINCT FROM NEW.agent_id
       OR cap.mode IS DISTINCT FROM NEW.mode OR cap.requires_human_approval IS DISTINCT FROM NEW.requires_approval
       OR u.capability_id IS DISTINCT FROM NEW.capability_id OR u.subject_kind IS DISTINCT FROM 'SWAP'
       OR u.subject_id IS DISTINCT FROM NEW.id OR u.action_class IS DISTINCT FROM NEW.action_class
       OR u.notional_minor IS DISTINCT FROM NEW.notional_usd_minor
       OR v.chain_id IS DISTINCT FROM NEW.chain_id
       OR r.account_id IS DISTINCT FROM NEW.account_id OR r.asset_id IS DISTINCT FROM NEW.pay_asset_id
       OR r.quantity IS DISTINCT FROM NEW.amount_in OR r.economic_intent_id IS DISTINCT FROM NEW.intent_id
       OR i.account_id IS DISTINCT FROM NEW.account_id OR i.kind IS DISTINCT FROM NEW.action_class
       OR i.actor_type IS DISTINCT FROM 'AGENT' OR i.agent_id IS DISTINCT FROM NEW.agent_id
       OR i.delegation_id IS DISTINCT FROM NEW.capability_id OR i.asset_id IS DISTINCT FROM NEW.pay_asset_id
       OR i.quantity IS DISTINCT FROM NEW.amount_in
       OR pa.kind IS DISTINCT FROM 'SIMULATED' OR pa.chain IS DISTINCT FROM NEW.chain_id
       OR ra.kind IS DISTINCT FROM 'SIMULATED' OR ra.chain IS DISTINCT FROM NEW.chain_id THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: swap % disagrees with its capability, usage, venue, reservation, intent or assets', NEW.id
            USING ERRCODE = 'DG003';
    END IF;
    IF NOT (NEW.chain_id = ANY (cap.chain_allowlist)) OR NOT (v.address = ANY (cap.destination_allowlist))
       OR NOT (NEW.action_class = ANY (cap.action_classes)) THEN
        RAISE EXCEPTION 'DELEGATION_OUTSIDE_GRANT: swap % on % against venue % is outside capability %', NEW.id, NEW.chain_id, v.address, cap.id
            USING ERRCODE = 'DG004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_birth() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_swaps_birth BEFORE INSERT ON delegated_swaps FOR EACH ROW EXECUTE FUNCTION cp_delegation_swap_birth();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('NONE','ADMITTED'),
        ('ADMITTED','AWAITING_APPROVAL'), ('ADMITTED','SIGNED'), ('ADMITTED','REJECTED'),
        ('AWAITING_APPROVAL','APPROVED'), ('AWAITING_APPROVAL','CANCELLED'), ('AWAITING_APPROVAL','REJECTED'),
        ('APPROVED','SIGNED'), ('APPROVED','REJECTED'),
        ('SIGNED','SUBMITTED'), ('SIGNED','UNKNOWN'), ('SIGNED','FAILED'),
        ('SUBMITTED','SETTLED'), ('SUBMITTED','FAILED'),
        ('UNKNOWN','SETTLED'), ('UNKNOWN','FAILED'));
$$;
-- +goose StatementEnd

-- Identity frozen, advancing columns set once, legal edges only, and the licensing of APPROVED, CANCELLED
-- and SIGNED.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    once text[] := ARRAY['message','recent_blockhash','blockhash_height','signed_at','tx_id','signed_tx','effect_id',
                         'journal_transaction_id','failure_code'];
    col  text;
    o    jsonb := to_jsonb(OLD);
    n    jsonb := to_jsonb(NEW);
    cap  delegated_capabilities%ROWTYPE;
BEGIN
    IF (n - once - ARRAY['status','updated_at']) IS DISTINCT FROM (o - once - ARRAY['status','updated_at']) THEN
        RAISE EXCEPTION 'DELEGATION_SWAP_FROZEN: swap % may not change its plan, amounts or parties', OLD.id USING ERRCODE = 'DG002';
    END IF;
    FOREACH col IN ARRAY once LOOP
        IF o->col IS DISTINCT FROM 'null'::jsonb AND o->col IS DISTINCT FROM n->col THEN
            RAISE EXCEPTION 'DELEGATION_SWAP_FROZEN: swap % already set %', OLD.id, col USING ERRCODE = 'DG002';
        END IF;
    END LOOP;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_delegation_swap_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: swap % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'DG001';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NEW.status IN ('APPROVED','CANCELLED') THEN
        IF NOT EXISTS (SELECT 1 FROM delegated_swap_approvals a WHERE a.swap_id = NEW.id AND a.plan_hash = NEW.plan_hash
                          AND a.decision = CASE NEW.status WHEN 'APPROVED' THEN 'APPROVE' ELSE 'DECLINE' END) THEN
            RAISE EXCEPTION 'DELEGATION_UNLICENSED: swap % cannot become % without that human decision on its plan', NEW.id, NEW.status
                USING ERRCODE = 'DG005';
        END IF;
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NEW.status = 'SIGNED' THEN
        -- A share lock: a revocation either committed before this signature (and is seen) or waits for
        -- it (and the pre-submission check then stops the transaction going out).
        SELECT * INTO cap FROM delegated_capabilities WHERE id = NEW.capability_id FOR SHARE;
        IF cap.status IS DISTINCT FROM 'ACTIVE' OR NEW.signed_at IS NULL OR NEW.signed_at < cap.granted_at
           OR NEW.signed_at >= cap.expires_at OR NEW.message IS NULL THEN
            RAISE EXCEPTION 'DELEGATION_UNLICENSED: swap % cannot be signed at % under capability % (% until %)',
                NEW.id, NEW.signed_at, cap.id, cap.status, cap.expires_at USING ERRCODE = 'DG005';
        END IF;
        IF NEW.requires_approval AND NOT EXISTS (SELECT 1 FROM delegated_swap_approvals a WHERE a.swap_id = NEW.id
                AND a.decision = 'APPROVE' AND a.plan_hash = NEW.plan_hash) THEN
            RAISE EXCEPTION 'DELEGATION_UNLICENSED: swap % needs the human approval its capability requires', NEW.id
                USING ERRCODE = 'DG005';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM delegated_signing_decisions d WHERE d.swap_id = NEW.id AND d.verdict = 'APPROVED'
                AND d.plan_hash = NEW.plan_hash AND d.message_sha256 = sha256(NEW.message)) THEN
            RAISE EXCEPTION 'DELEGATION_UNLICENSED: swap % has no APPROVED signing decision over the message it signed', NEW.id
                USING ERRCODE = 'DG005';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_swaps_guard BEFORE UPDATE ON delegated_swaps FOR EACH ROW EXECUTE FUNCTION cp_delegation_swap_guard();

CREATE TABLE delegated_swap_transitions (
    id           uuid PRIMARY KEY,
    swap_id      uuid NOT NULL REFERENCES delegated_swaps(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL,
    actor_id     text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delegated_swap_transitions_idx ON delegated_swap_transitions (swap_id, occurred_at);
CREATE TRIGGER delegated_swap_transitions_immutable BEFORE UPDATE OR DELETE ON delegated_swap_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_transition_row_legal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT cp_delegation_swap_transition_legal(NEW.from_status, NEW.to_status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: a transition row cannot describe swap % moving % -> %',
            NEW.swap_id, NEW.from_status, NEW.to_status USING ERRCODE = 'DG001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER delegated_swap_transitions_legal BEFORE INSERT ON delegated_swap_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_swap_transition_row_legal();
-- A swap may take several steps in one transaction (signing, then settling an observed submission). The
-- machine is acyclic and its legality is enforced above for every writer, so the accumulating edge binding
-- of 00733 cannot license an edge twice.
CREATE TRIGGER delegated_swap_transitions_flag_edge AFTER INSERT ON delegated_swap_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('swap_id', 'from_status', 'to_status', 'delegated_swap_status');
CREATE CONSTRAINT TRIGGER delegated_swaps_require_transition AFTER UPDATE OF status ON delegated_swaps
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'delegated_swap_status');

-- ---------------------------------------------------------------------------
-- 6. Human decisions on swaps whose capability requires one
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_swap_approvals (
    id                   uuid PRIMARY KEY,
    swap_id              uuid NOT NULL UNIQUE REFERENCES delegated_swaps(id),
    decision             text NOT NULL CHECK (decision IN ('APPROVE','DECLINE')),
    plan_hash            bytea NOT NULL CHECK (length(plan_hash) = 32),
    approver_actor_type  text NOT NULL CHECK (approver_actor_type = 'USER'),
    approver_actor_id    text NOT NULL,
    authenticated_at     timestamptz NOT NULL,
    decided_at           timestamptz NOT NULL,
    reason               text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    created_at           timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER delegated_swap_approvals_immutable BEFORE UPDATE OR DELETE ON delegated_swap_approvals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_approval_binds() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s      delegated_swaps%ROWTYPE;
    owner  uuid;
BEGIN
    SELECT * INTO s FROM delegated_swaps WHERE id = NEW.swap_id;
    SELECT owner_user_id INTO owner FROM accounts WHERE id = s.account_id;
    IF s.plan_hash IS DISTINCT FROM NEW.plan_hash OR s.status IS DISTINCT FROM 'AWAITING_APPROVAL' OR NOT s.requires_approval
       OR owner::text IS DISTINCT FROM NEW.approver_actor_id THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: decision on swap % must be the account owner''s, on the plan awaiting approval', NEW.swap_id
            USING ERRCODE = 'DG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_approval_binds() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_swap_approvals_binds BEFORE INSERT ON delegated_swap_approvals
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_swap_approval_binds();

-- ---------------------------------------------------------------------------
-- 7. Signing decisions: one per subject (a swap here; a withdrawal from 00753)
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_signing_decisions (
    id                 uuid PRIMARY KEY,
    swap_id            uuid UNIQUE REFERENCES delegated_swaps(id),
    withdrawal_id      uuid UNIQUE,
    account_id         uuid NOT NULL REFERENCES accounts(id),
    capability_id      uuid NOT NULL REFERENCES delegated_capabilities(id),
    capability_hash    bytea NOT NULL CHECK (length(capability_hash) = 32),
    plan_hash          bytea NOT NULL CHECK (length(plan_hash) = 32),
    message_sha256     bytea NOT NULL CHECK (length(message_sha256) = 32),
    signer_public_key  text NOT NULL,
    verdict            text NOT NULL CHECK (verdict IN ('APPROVED','REJECTED')),
    reason_codes       text[] NOT NULL,
    policy_version     text NOT NULL,
    policy_hash        text NOT NULL,
    inspector_version  text NOT NULL,
    decided_at         timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delegated_signing_decisions_one_subject CHECK (num_nonnulls(swap_id, withdrawal_id) = 1),
    CONSTRAINT delegated_signing_decisions_verdict_reasons CHECK ((verdict = 'APPROVED') = (cardinality(reason_codes) = 0))
);
CREATE TRIGGER delegated_signing_decisions_immutable BEFORE UPDATE OR DELETE ON delegated_signing_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A decision names its subject's account, capability, plan and exact compiled message, and the signer key
-- registered for that account on the subject's chain. 00753 extends it to withdrawals.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_decision_binds() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s  delegated_swaps%ROWTYPE;
BEGIN
    IF NEW.swap_id IS NULL THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: signing decision % names no subject this schema knows', NEW.id USING ERRCODE = 'DG003';
    END IF;
    SELECT * INTO s FROM delegated_swaps WHERE id = NEW.swap_id;
    IF s.account_id IS DISTINCT FROM NEW.account_id OR s.capability_id IS DISTINCT FROM NEW.capability_id
       OR s.plan_hash IS DISTINCT FROM NEW.plan_hash OR s.message IS NULL OR sha256(s.message) IS DISTINCT FROM NEW.message_sha256
       OR NOT EXISTS (SELECT 1 FROM delegated_signer_keys k WHERE k.account_id = s.account_id AND k.chain_id = s.chain_id
                         AND k.public_key = NEW.signer_public_key AND k.public_key = s.wallet) THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: signing decision % does not describe swap %''s account, capability, plan, message and signer',
            NEW.id, NEW.swap_id USING ERRCODE = 'DG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_decision_binds() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_signing_decisions_binds BEFORE INSERT ON delegated_signing_decisions
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_decision_binds();

-- ---------------------------------------------------------------------------
-- 8. A settled swap is its SIM_TRADE and its applied effect, checked at COMMIT
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_swap_outcome_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    j      record;
    legs   record;
    eff    record;
    house  uuid;
BEGIN
    SELECT house_account_id INTO house FROM delegation_venues WHERE id = NEW.venue_id;
    SELECT kind, reference_type, reference_id, reversal_of INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM 'SIM_TRADE' OR j.reference_type IS DISTINCT FROM 'delegated_swap'
       OR j.reference_id IS DISTINCT FROM NEW.id::text OR j.reversal_of IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: swap % names journal % which is not its SIM_TRADE', NEW.id, NEW.journal_transaction_id
            USING ERRCODE = 'DG006';
    END IF;
    SELECT count(*) AS n,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'WALLET'
                              AND e.asset_id = NEW.pay_asset_id AND e.side = 'CREDIT' AND e.quantity = NEW.amount_in) AS c1,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'TRADING_OUTFLOW'
                              AND e.asset_id = NEW.pay_asset_id AND e.side = 'DEBIT' AND e.quantity = NEW.amount_in) AS c2,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'WALLET'
                              AND e.asset_id = NEW.receive_asset_id AND e.side = 'DEBIT' AND e.quantity = NEW.amount_out) AS c3,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'TRADING_INFLOW'
                              AND e.asset_id = NEW.receive_asset_id AND e.side = 'CREDIT' AND e.quantity = NEW.amount_out) AS c4,
           count(*) FILTER (WHERE la.owner_type = 'HOUSE' AND la.owner_id = house AND la.code = 'HOUSE_INVENTORY'
                              AND e.asset_id = NEW.pay_asset_id AND e.side = 'DEBIT' AND e.quantity = NEW.amount_in) AS h1,
           count(*) FILTER (WHERE la.owner_type = 'HOUSE' AND la.owner_id = house AND la.code = 'HOUSE_TRADING_INFLOW'
                              AND e.asset_id = NEW.pay_asset_id AND e.side = 'CREDIT' AND e.quantity = NEW.amount_in) AS h2,
           count(*) FILTER (WHERE la.owner_type = 'HOUSE' AND la.owner_id = house AND la.code = 'HOUSE_TRADING_OUTFLOW'
                              AND e.asset_id = NEW.receive_asset_id AND e.side = 'DEBIT' AND e.quantity = NEW.amount_out) AS h3,
           count(*) FILTER (WHERE la.owner_type = 'HOUSE' AND la.owner_id = house AND la.code = 'HOUSE_INVENTORY'
                              AND e.asset_id = NEW.receive_asset_id AND e.side = 'CREDIT' AND e.quantity = NEW.amount_out) AS h4
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF legs.n <> 8 OR legs.c1 <> 1 OR legs.c2 <> 1 OR legs.c3 <> 1 OR legs.c4 <> 1
       OR legs.h1 <> 1 OR legs.h2 <> 1 OR legs.h3 <> 1 OR legs.h4 <> 1 THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: journal % does not exchange exactly % for % between account % and house %',
            NEW.journal_transaction_id, NEW.amount_in, NEW.amount_out, NEW.account_id, house USING ERRCODE = 'DG006';
    END IF;
    SELECT status, subsystem, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'AGENT_AUTHORITY'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: swap % is SETTLED but effect % did not apply journal %',
            NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'DG006';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_swap_outcome_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER delegated_swaps_outcome_matches AFTER UPDATE OF status ON delegated_swaps
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.status = 'SETTLED')
    EXECUTE FUNCTION cp_delegation_swap_outcome_matches();

-- ---------------------------------------------------------------------------
-- 9. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON delegated_capabilities, delegated_capability_transitions, delegated_signer_keys,
    delegation_venues, delegated_capability_usage, delegated_swaps, delegated_swap_transitions,
    delegated_swap_approvals, delegated_signing_decisions TO cp_app;
GRANT UPDATE (status, revoked_by_actor_type, revoked_by_actor_id, revocation_reason, revoked_at)
    ON delegated_capabilities TO cp_app;
GRANT UPDATE (status, message, recent_blockhash, blockhash_height, signed_at, tx_id, signed_tx, effect_id,
              journal_transaction_id, failure_code) ON delegated_swaps TO cp_app;
GRANT SELECT ON delegated_capabilities, delegated_capability_transitions, delegated_signer_keys, delegation_venues,
    delegated_capability_usage, delegated_swaps, delegated_swap_transitions, delegated_swap_approvals,
    delegated_signing_decisions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: delegated authority, its use and its signing decisions are never dropped
