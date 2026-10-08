-- +goose Up
-- Sandbox stable assets and the persistent simulated-chain host (ADR-0020,
-- docs/architecture/ADVANCED_FINANCIAL_MODEL.md §3-§7; internal/devnethost, internal/stableasset).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. devnet_events is the append-only, gapless, per-network history of the simulated chains: every
--      accepted submission (raw bytes, chain, tx id, time), every block production, every operator
--      call and every fault. A row can never change or disappear, and a row can only be the next one.
--   2. A platform stable asset is a registry row bound to exactly one SIMULATED asset on a simulated
--      chain whose mint_address is the devnet mint, and to one STABLE_ISSUER facility. Its status moves
--      only along ACTIVE <-> PAUSED, ACTIVE|PAUSED -> RETIRED, and every change is bound to a transition
--      row naming both endpoints. Everything but the status is frozen.
--   3. Mint and burn requests move only along their state machines, every change bound to a transition
--      row; an ISSUED or REDEEMED request names the journal transaction it posted.
--   4. Every STABLE_ISSUANCE and STABLE_REDEMPTION journal transaction is checked at COMMIT against the
--      registry (deferred): outstanding supply never exceeds the issuance cap, an issuance carries its own
--      full backing in one eligible cash-equivalent reserve asset at the target peg and eligibility
--      factor (valued at par), and a redemption pays out no more than the peg value of what it retires.
--   5. Reserve snapshots are append-only and hash-linked: each names its predecessor's hash.
--
-- Custom SQLSTATEs:
--   DH001 a devnet event that is not the next of its network's history.
--   SV001 an illegal stable asset, mint request or burn request status transition.
--   SV002 a frozen column of a stable asset, reserve asset, mint request or burn request changed.
--   SV003 outstanding supply above the issuance cap.
--   SV004 an issuance without its full eligible cash-equivalent backing.
--   SV005 a redemption paying out more than the peg value of what it retires.
--   SV006 a registry row inconsistent with the asset it names.
--   SV007 a reserve snapshot that does not extend its asset's snapshot chain.

-- ---------------------------------------------------------------------------
-- 1. The simulated-chain history (internal/devnethost)
-- ---------------------------------------------------------------------------

CREATE TABLE devnet_events (
    id           uuid PRIMARY KEY,
    network      text NOT NULL CHECK (network ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    seq          bigint NOT NULL CHECK (seq >= 1),
    kind         text NOT NULL CHECK (kind IN ('GENESIS','CREATE_MINT','AIRDROP','SET_MINT_FROZEN','SUBMIT',
                                               'PRODUCE_BLOCKS','DROP_NEXT','TIMEOUT_BUT_LAND','TIMEOUT_AND_DROP',
                                               'DELAY_INCLUSION','CLEAR_FAULTS','REORG')),
    chain_id     text CHECK (chain_id IS NULL OR chain_id ~ '^sbx-[a-z0-9-]{1,28}$'),
    args         jsonb NOT NULL,
    raw_tx       bytea CHECK (raw_tx IS NULL OR length(raw_tx) BETWEEN 1 AND 4096),
    tx_id        text CHECK (tx_id IS NULL OR length(tx_id) BETWEEN 1 AND 88),
    outcome      text NOT NULL CHECK (outcome IN ('APPLIED','ACCEPTED','TIMEOUT')),
    state_hash   bytea NOT NULL CHECK (length(state_hash) = 32),
    prev_hash    bytea NOT NULL CHECK (length(prev_hash) = 32),
    event_hash   bytea NOT NULL CHECK (length(event_hash) = 32),
    occurred_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (network, seq),
    -- GENESIS is the first event and names no chain; every other event names one.
    CONSTRAINT devnet_events_genesis_first CHECK ((kind = 'GENESIS') = (seq = 1)),
    CONSTRAINT devnet_events_genesis_chainless CHECK ((kind = 'GENESIS') = (chain_id IS NULL)),
    -- A submission carries its exact bytes and tx id; nothing else does.
    CONSTRAINT devnet_events_submit_shape CHECK ((kind = 'SUBMIT') = (raw_tx IS NOT NULL AND tx_id IS NOT NULL)),
    CONSTRAINT devnet_events_submit_outcome CHECK ((kind = 'SUBMIT') = (outcome IN ('ACCEPTED','TIMEOUT')))
);
CREATE INDEX devnet_events_tx_idx ON devnet_events (network, chain_id, tx_id) WHERE tx_id IS NOT NULL;

-- +goose StatementBegin
CREATE FUNCTION cp_devnet_event_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest bigint;
BEGIN
    SELECT max(seq) INTO latest FROM devnet_events WHERE network = NEW.network;
    IF NEW.seq <> coalesce(latest, 0) + 1 THEN
        RAISE EXCEPTION 'DEVNET_HISTORY_GAP: network % event % is not the next after %', NEW.network, NEW.seq, coalesce(latest, 0)
            USING ERRCODE = 'DH001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_devnet_event_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER devnet_events_next BEFORE INSERT ON devnet_events
    FOR EACH ROW EXECUTE FUNCTION cp_devnet_event_is_next();
CREATE TRIGGER devnet_events_immutable BEFORE UPDATE OR DELETE ON devnet_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON devnet_events TO cp_app;
GRANT SELECT ON devnet_events TO cp_readonly, cp_ops;

-- ---------------------------------------------------------------------------
-- 2. The stable asset registry (internal/stableasset)
-- ---------------------------------------------------------------------------

CREATE TABLE stable_assets (
    id                          uuid PRIMARY KEY,
    name                        text NOT NULL UNIQUE CHECK (name ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    asset_id                    uuid NOT NULL UNIQUE REFERENCES assets(id),
    facility_id                 uuid NOT NULL UNIQUE REFERENCES facilities(id),
    network                     text NOT NULL CHECK (network ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    chain_id                    text NOT NULL CHECK (chain_id ~ '^sbx-[a-z0-9-]{1,28}$'),
    devnet_mint                 text NOT NULL CHECK (length(devnet_mint) BETWEEN 32 AND 44),
    decimals                    smallint NOT NULL CHECK (decimals BETWEEN 0 AND 18),
    status                      text NOT NULL CHECK (status IN ('ACTIVE','PAUSED','RETIRED')),
    issuance_cap                numeric(38,0) NOT NULL CHECK (issuance_cap > 0),
    mint_authority_mode         text NOT NULL CHECK (mint_authority_mode IN ('PLATFORM_SIGNER')),
    mint_authority              text NOT NULL CHECK (length(mint_authority) BETWEEN 32 AND 44),
    burn_authority_mode         text NOT NULL CHECK (burn_authority_mode IN ('HOLDER_SIGNED')),
    redemption_mode             text NOT NULL CHECK (redemption_mode IN ('INSTANT','QUEUED')),
    peg_quote_asset_id          uuid NOT NULL REFERENCES assets(id),
    peg_mantissa                numeric(38,0) NOT NULL CHECK (peg_mantissa > 0),
    peg_scale                   integer NOT NULL CHECK (peg_scale BETWEEN 0 AND 38),
    oracle_source               text NOT NULL CHECK (length(oracle_source) BETWEEN 1 AND 100),
    max_price_age_ms            bigint NOT NULL CHECK (max_price_age_ms > 0),
    peg_deviation_threshold_bps integer NOT NULL CHECK (peg_deviation_threshold_bps BETWEEN 1 AND 10000),
    attestable                  boolean NOT NULL,
    created_by_actor_type       text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id         text NOT NULL,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (chain_id, devnet_mint),
    CONSTRAINT stable_assets_peg_is_another_asset CHECK (peg_quote_asset_id <> asset_id)
);
CREATE TRIGGER stable_assets_updated_at BEFORE UPDATE ON stable_assets FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A registry row names a SIMULATED asset on its own chain whose mint is the devnet mint, and a
-- STABLE_ISSUER facility. Checked once, at insert; the columns are frozen afterwards.
-- +goose StatementBegin
CREATE FUNCTION cp_stable_asset_consistent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    a  record;
    fk text;
BEGIN
    SELECT kind, chain, mint_address, decimals, value_domain INTO a FROM assets WHERE id = NEW.asset_id;
    IF a.kind IS DISTINCT FROM 'SIMULATED' OR a.value_domain IS DISTINCT FROM 'SIMULATED'
       OR a.chain IS DISTINCT FROM NEW.chain_id OR a.mint_address IS DISTINCT FROM NEW.devnet_mint
       OR a.decimals IS DISTINCT FROM NEW.decimals THEN
        RAISE EXCEPTION 'STABLE_REGISTRY_INCONSISTENT: stable asset % must name a SIMULATED asset on % with mint % and % decimals',
            NEW.id, NEW.chain_id, NEW.devnet_mint, NEW.decimals USING ERRCODE = 'SV006';
    END IF;
    SELECT kind INTO fk FROM facilities WHERE id = NEW.facility_id;
    IF fk IS DISTINCT FROM 'STABLE_ISSUER' THEN
        RAISE EXCEPTION 'STABLE_REGISTRY_INCONSISTENT: stable asset % names facility % which is not a STABLE_ISSUER',
            NEW.id, NEW.facility_id USING ERRCODE = 'SV006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_stable_asset_consistent() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER stable_assets_consistent BEFORE INSERT ON stable_assets
    FOR EACH ROW EXECUTE FUNCTION cp_stable_asset_consistent();

-- +goose StatementBegin
CREATE FUNCTION cp_stable_asset_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('ACTIVE','PAUSED'), ('PAUSED','ACTIVE'), ('ACTIVE','RETIRED'), ('PAUSED','RETIRED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_stable_asset_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.name, NEW.asset_id, NEW.facility_id, NEW.network, NEW.chain_id, NEW.devnet_mint, NEW.decimals,
        NEW.issuance_cap, NEW.mint_authority_mode, NEW.mint_authority, NEW.burn_authority_mode, NEW.redemption_mode,
        NEW.peg_quote_asset_id, NEW.peg_mantissa, NEW.peg_scale, NEW.oracle_source, NEW.max_price_age_ms,
        NEW.peg_deviation_threshold_bps, NEW.attestable, NEW.created_by_actor_type, NEW.created_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.name, OLD.asset_id, OLD.facility_id, OLD.network, OLD.chain_id, OLD.devnet_mint, OLD.decimals,
        OLD.issuance_cap, OLD.mint_authority_mode, OLD.mint_authority, OLD.burn_authority_mode, OLD.redemption_mode,
        OLD.peg_quote_asset_id, OLD.peg_mantissa, OLD.peg_scale, OLD.oracle_source, OLD.max_price_age_ms,
        OLD.peg_deviation_threshold_bps, OLD.attestable, OLD.created_by_actor_type, OLD.created_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'STABLE_ASSET_FROZEN: stable asset % may change status only', OLD.id USING ERRCODE = 'SV002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_stable_asset_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'STABLE_TRANSITION_ILLEGAL: stable asset % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'SV001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER stable_assets_guard BEFORE UPDATE ON stable_assets
    FOR EACH ROW EXECUTE FUNCTION cp_stable_asset_guard();

CREATE TABLE stable_asset_transitions (
    id               uuid PRIMARY KEY,
    stable_asset_id  uuid NOT NULL REFERENCES stable_assets(id),
    from_status      text NOT NULL,
    to_status        text NOT NULL,
    actor_type       text NOT NULL,
    actor_id         text NOT NULL,
    reason           text NOT NULL CHECK (length(reason) > 0),
    evidence_ref     text,
    occurred_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stable_asset_transitions_idx ON stable_asset_transitions (stable_asset_id, occurred_at);
CREATE TRIGGER stable_asset_transitions_immutable BEFORE UPDATE OR DELETE ON stable_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER stable_asset_transitions_flag_edge AFTER INSERT ON stable_asset_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('stable_asset_id', 'from_status', 'to_status', 'stable_asset_status');
CREATE CONSTRAINT TRIGGER stable_assets_require_transition AFTER UPDATE OF status ON stable_assets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'stable_asset_status');

-- The reserve policy: which assets are eligible reserve, of which kind, at which eligibility factor, and
-- which of them pay for a mint. Fully reserved model; immutable once written.
CREATE TABLE stable_reserve_assets (
    id                      uuid PRIMARY KEY,
    stable_asset_id         uuid NOT NULL REFERENCES stable_assets(id),
    reserve_asset_id        uuid NOT NULL REFERENCES assets(id),
    reserve_kind            text NOT NULL CHECK (reserve_kind IN ('CASH_EQUIVALENT','TOKENIZED')),
    eligibility_factor_bps  integer NOT NULL CHECK (eligibility_factor_bps BETWEEN 0 AND 10000),
    accepted_for_mint       boolean NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (stable_asset_id, reserve_asset_id),
    CONSTRAINT stable_reserve_assets_mint_is_cash CHECK (
        NOT accepted_for_mint OR (reserve_kind = 'CASH_EQUIVALENT' AND eligibility_factor_bps > 0))
);
CREATE TRIGGER stable_reserve_assets_immutable BEFORE UPDATE OR DELETE ON stable_reserve_assets
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_stable_reserve_asset_consistent() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k    text;
    self uuid;
BEGIN
    SELECT kind INTO k FROM assets WHERE id = NEW.reserve_asset_id;
    SELECT asset_id INTO self FROM stable_assets WHERE id = NEW.stable_asset_id;
    IF k IS DISTINCT FROM 'SIMULATED' OR self = NEW.reserve_asset_id THEN
        RAISE EXCEPTION 'STABLE_REGISTRY_INCONSISTENT: reserve asset % of stable asset % must be another SIMULATED asset',
            NEW.reserve_asset_id, NEW.stable_asset_id USING ERRCODE = 'SV006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_stable_reserve_asset_consistent() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER stable_reserve_assets_consistent BEFORE INSERT ON stable_reserve_assets
    FOR EACH ROW EXECUTE FUNCTION cp_stable_reserve_asset_consistent();

-- ---------------------------------------------------------------------------
-- 3. Mint and burn requests
-- ---------------------------------------------------------------------------

CREATE TABLE stable_mint_requests (
    id                      uuid PRIMARY KEY,
    stable_asset_id         uuid NOT NULL REFERENCES stable_assets(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    intent_id               uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    mode                    text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    reserve_asset_id        uuid NOT NULL REFERENCES assets(id),
    reserve_quantity        numeric(38,0) NOT NULL CHECK (reserve_quantity > 0),
    reservation_id          uuid NOT NULL REFERENCES asset_reservations(id),
    recipient               text NOT NULL,
    plan                    jsonb NOT NULL,
    plan_hash               bytea NOT NULL CHECK (length(plan_hash) = 32),
    signing_plan            jsonb NOT NULL,
    chain_id                text NOT NULL CHECK (chain_id ~ '^sbx-[a-z0-9-]{1,28}$'),
    tx_id                   text NOT NULL UNIQUE,
    signed_tx               bytea NOT NULL CHECK (length(signed_tx) BETWEEN 1 AND 4096),
    recent_blockhash        text NOT NULL,
    status                  text NOT NULL CHECK (status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','ISSUED','FAILED')),
    failure_code            text,
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    final_slot              bigint,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- An ISSUED request names the STABLE_ISSUANCE it posted, and only an ISSUED one names any.
    CONSTRAINT stable_mint_requests_issued_has_journal CHECK ((status = 'ISSUED') = (journal_transaction_id IS NOT NULL)),
    CONSTRAINT stable_mint_requests_failed_has_code CHECK ((status = 'FAILED') = (failure_code IS NOT NULL))
);
CREATE INDEX stable_mint_requests_open_idx ON stable_mint_requests (stable_asset_id, status)
    WHERE status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN');
CREATE INDEX stable_mint_requests_account_idx ON stable_mint_requests (account_id, created_at DESC);
CREATE TRIGGER stable_mint_requests_updated_at BEFORE UPDATE ON stable_mint_requests FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE stable_burn_requests (
    id                      uuid PRIMARY KEY,
    stable_asset_id         uuid NOT NULL REFERENCES stable_assets(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    intent_id               uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    mode                    text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    payout_asset_id         uuid NOT NULL REFERENCES assets(id),
    payout_quantity         numeric(38,0) NOT NULL CHECK (payout_quantity > 0),
    reservation_id          uuid NOT NULL REFERENCES asset_reservations(id),
    holder                  text NOT NULL,
    plan                    jsonb NOT NULL,
    plan_hash               bytea NOT NULL CHECK (length(plan_hash) = 32),
    signing_plan            jsonb NOT NULL,
    chain_id                text NOT NULL CHECK (chain_id ~ '^sbx-[a-z0-9-]{1,28}$'),
    tx_id                   text NOT NULL UNIQUE,
    signed_tx               bytea NOT NULL CHECK (length(signed_tx) BETWEEN 1 AND 4096),
    recent_blockhash        text NOT NULL,
    status                  text NOT NULL CHECK (status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN',
                                                            'REDEMPTION_QUEUED','REDEEMED','FAILED')),
    failure_code            text,
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    final_slot              bigint,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT stable_burn_requests_redeemed_has_journal CHECK ((status = 'REDEEMED') = (journal_transaction_id IS NOT NULL)),
    CONSTRAINT stable_burn_requests_failed_has_code CHECK ((status = 'FAILED') = (failure_code IS NOT NULL))
);
CREATE INDEX stable_burn_requests_open_idx ON stable_burn_requests (stable_asset_id, status)
    WHERE status IN ('SUBMITTING','SUBMITTED','SUBMISSION_UNKNOWN','REDEMPTION_QUEUED');
CREATE INDEX stable_burn_requests_account_idx ON stable_burn_requests (account_id, created_at DESC);
CREATE TRIGGER stable_burn_requests_updated_at BEFORE UPDATE ON stable_burn_requests FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_stable_mint_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('SUBMITTING','SUBMITTED'), ('SUBMITTING','SUBMISSION_UNKNOWN'), ('SUBMITTING','ISSUED'), ('SUBMITTING','FAILED'),
        ('SUBMITTED','ISSUED'), ('SUBMITTED','FAILED'),
        ('SUBMISSION_UNKNOWN','ISSUED'), ('SUBMISSION_UNKNOWN','FAILED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_stable_burn_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('SUBMITTING','SUBMITTED'), ('SUBMITTING','SUBMISSION_UNKNOWN'), ('SUBMITTING','REDEMPTION_QUEUED'),
        ('SUBMITTING','REDEEMED'), ('SUBMITTING','FAILED'),
        ('SUBMITTED','REDEMPTION_QUEUED'), ('SUBMITTED','REDEEMED'), ('SUBMITTED','FAILED'),
        ('SUBMISSION_UNKNOWN','REDEMPTION_QUEUED'), ('SUBMISSION_UNKNOWN','REDEEMED'), ('SUBMISSION_UNKNOWN','FAILED'),
        ('REDEMPTION_QUEUED','REDEEMED'));
$$;
-- +goose StatementEnd

-- One guard for both request tables: identity frozen (SV002), legal edges only (SV001), and a journal
-- link once written is never moved.
-- +goose StatementBegin
CREATE FUNCTION cp_stable_request_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    legal boolean;
BEGIN
    IF (to_jsonb(NEW) - ARRAY['status','failure_code','journal_transaction_id','final_slot','updated_at'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['status','failure_code','journal_transaction_id','final_slot','updated_at']) THEN
        RAISE EXCEPTION 'STABLE_REQUEST_FROZEN: % % may change status and outcome only', TG_TABLE_NAME, OLD.id
            USING ERRCODE = 'SV002';
    END IF;
    IF OLD.journal_transaction_id IS NOT NULL AND NEW.journal_transaction_id IS DISTINCT FROM OLD.journal_transaction_id THEN
        RAISE EXCEPTION 'STABLE_REQUEST_FROZEN: % % already posted journal %', TG_TABLE_NAME, OLD.id, OLD.journal_transaction_id
            USING ERRCODE = 'SV002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status THEN
        IF TG_TABLE_NAME = 'stable_mint_requests' THEN
            legal := cp_stable_mint_transition_legal(OLD.status, NEW.status);
        ELSE
            legal := cp_stable_burn_transition_legal(OLD.status, NEW.status);
        END IF;
        IF NOT legal THEN
            RAISE EXCEPTION 'STABLE_TRANSITION_ILLEGAL: % % cannot move % -> %', TG_TABLE_NAME, OLD.id, OLD.status, NEW.status
                USING ERRCODE = 'SV001';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER stable_mint_requests_guard BEFORE UPDATE ON stable_mint_requests
    FOR EACH ROW EXECUTE FUNCTION cp_stable_request_guard();
CREATE TRIGGER stable_burn_requests_guard BEFORE UPDATE ON stable_burn_requests
    FOR EACH ROW EXECUTE FUNCTION cp_stable_request_guard();

CREATE TABLE stable_mint_request_transitions (
    id           uuid PRIMARY KEY,
    request_id   uuid NOT NULL REFERENCES stable_mint_requests(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL,
    actor_id     text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) > 0),
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stable_mint_request_transitions_idx ON stable_mint_request_transitions (request_id, occurred_at);
CREATE TRIGGER stable_mint_request_transitions_immutable BEFORE UPDATE OR DELETE ON stable_mint_request_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER stable_mint_request_transitions_flag_edge AFTER INSERT ON stable_mint_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('request_id', 'from_status', 'to_status', 'stable_mint_status');
CREATE CONSTRAINT TRIGGER stable_mint_requests_require_transition AFTER UPDATE OF status ON stable_mint_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'stable_mint_status');

CREATE TABLE stable_burn_request_transitions (
    id           uuid PRIMARY KEY,
    request_id   uuid NOT NULL REFERENCES stable_burn_requests(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    actor_type   text NOT NULL,
    actor_id     text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) > 0),
    occurred_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stable_burn_request_transitions_idx ON stable_burn_request_transitions (request_id, occurred_at);
CREATE TRIGGER stable_burn_request_transitions_immutable BEFORE UPDATE OR DELETE ON stable_burn_request_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER stable_burn_request_transitions_flag_edge AFTER INSERT ON stable_burn_request_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('request_id', 'from_status', 'to_status', 'stable_burn_status');
CREATE CONSTRAINT TRIGGER stable_burn_requests_require_transition AFTER UPDATE OF status ON stable_burn_requests
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'stable_burn_status');

-- ---------------------------------------------------------------------------
-- 4. The issuance and redemption rules, at COMMIT, for every journal of those kinds
-- ---------------------------------------------------------------------------

-- Every STABLE_ISSUANCE or STABLE_REDEMPTION journal names exactly one registered stable asset (its
-- issuer facility's STABLE_OUTSTANDING entry) and exactly one cash-equivalent reserve entry of that
-- facility. Then, exactly (numeric arithmetic, no rounding):
--   cap:        the facility's STABLE_OUTSTANDING balance <= issuance_cap                          (SV003)
--   issuance:   reserve_qty * factor * 10^(d_stable + peg_scale) >= qty * peg * 10000 * 10^(d_reserve)
--               i.e. the reserve paid, at par and the eligibility factor, covers the issued value (SV004)
--   redemption: payout_qty * 10^(d_stable + peg_scale) <= qty * peg * 10^(d_reserve)
--               i.e. the payout, at par, is at most the peg value of what is retired             (SV005)
-- +goose StatementBegin
CREATE FUNCTION cp_check_stable_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k          text;
    flag       text;
    n_out      integer;
    n_res      integer;
    sa_id      uuid;
    sa         stable_assets%ROWTYPE;
    qty        numeric;
    rq         numeric;
    r_asset    uuid;
    r_dec      integer;
    ra         stable_reserve_assets%ROWTYPE;
    bal        numeric;
BEGIN
    SELECT kind INTO k FROM journal_transactions WHERE id = NEW.transaction_id;
    IF k IS NULL OR k NOT IN ('STABLE_ISSUANCE','STABLE_REDEMPTION') THEN
        RETURN NULL;
    END IF;
    flag := 'cp.stable.checked.x' || translate(NEW.transaction_id::text, '-', '_');
    IF current_setting(flag, true) = '1' THEN
        RETURN NULL;
    END IF;
    PERFORM set_config(flag, '1', true);

    SELECT count(*) INTO n_out FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id AND la.owner_type = 'FACILITY' AND la.code = 'STABLE_OUTSTANDING';
    SELECT s.id, e.quantity INTO sa_id, qty
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
      JOIN stable_assets s ON s.facility_id = la.owner_id AND s.asset_id = e.asset_id
     WHERE e.transaction_id = NEW.transaction_id AND la.owner_type = 'FACILITY' AND la.code = 'STABLE_OUTSTANDING';
    SELECT * INTO sa FROM stable_assets WHERE id = sa_id;
    IF n_out <> 1 OR sa.id IS NULL THEN
        RAISE EXCEPTION 'STABLE_REGISTRY_INCONSISTENT: % transaction % must retire or issue exactly one registered stable asset',
            k, NEW.transaction_id USING ERRCODE = 'SV006';
    END IF;

    SELECT count(*) INTO n_res FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id AND la.owner_type = 'FACILITY' AND la.code = 'STABLE_RESERVE';
    SELECT e.asset_id, e.quantity INTO r_asset, rq
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id AND la.owner_type = 'FACILITY' AND la.code = 'STABLE_RESERVE'
       AND la.owner_id = sa.facility_id;
    SELECT * INTO ra FROM stable_reserve_assets WHERE stable_asset_id = sa.id AND reserve_asset_id = r_asset;
    SELECT decimals INTO r_dec FROM assets WHERE id = r_asset;
    IF n_res <> 1 OR rq IS NULL OR ra.id IS NULL OR ra.reserve_kind <> 'CASH_EQUIVALENT'
       OR (k = 'STABLE_ISSUANCE' AND NOT ra.accepted_for_mint) THEN
        IF k = 'STABLE_ISSUANCE' THEN
            RAISE EXCEPTION 'ISSUANCE_UNDER_RESERVED: issuance % of stable asset % is not paid in exactly one eligible cash-equivalent reserve asset',
                NEW.transaction_id, sa.id USING ERRCODE = 'SV004';
        END IF;
        RAISE EXCEPTION 'REDEMPTION_OVERPAYS: redemption % of stable asset % does not pay out exactly one cash-equivalent reserve asset',
            NEW.transaction_id, sa.id USING ERRCODE = 'SV005';
    END IF;

    SELECT coalesce(b.balance, 0) INTO bal
      FROM ledger_accounts la LEFT JOIN ledger_balances b ON b.ledger_account_id = la.id
     WHERE la.owner_type = 'FACILITY' AND la.owner_id = sa.facility_id AND la.code = 'STABLE_OUTSTANDING'
       AND la.asset_id = sa.asset_id;
    IF coalesce(bal, 0) > sa.issuance_cap THEN
        RAISE EXCEPTION 'ISSUANCE_CAP_EXCEEDED: stable asset % outstanding % exceeds its cap %', sa.id, bal, sa.issuance_cap
            USING ERRCODE = 'SV003';
    END IF;

    IF k = 'STABLE_ISSUANCE' THEN
        IF rq * ra.eligibility_factor_bps * (10::numeric ^ (sa.decimals + sa.peg_scale))
           < qty * sa.peg_mantissa * 10000 * (10::numeric ^ r_dec) THEN
            RAISE EXCEPTION 'ISSUANCE_UNDER_RESERVED: issuance % of % units of stable asset % is backed by only % of reserve asset %',
                NEW.transaction_id, qty, sa.id, rq, r_asset USING ERRCODE = 'SV004';
        END IF;
    ELSE
        IF rq * (10::numeric ^ (sa.decimals + sa.peg_scale)) > qty * sa.peg_mantissa * (10::numeric ^ r_dec) THEN
            RAISE EXCEPTION 'REDEMPTION_OVERPAYS: redemption % of % units of stable asset % pays out % of reserve asset %',
                NEW.transaction_id, qty, sa.id, rq, r_asset USING ERRCODE = 'SV005';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_stable_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER journal_entries_stable_rules AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_stable_journal();

-- ---------------------------------------------------------------------------
-- 5. Peg monitoring and reserve snapshots
-- ---------------------------------------------------------------------------

CREATE TABLE stable_peg_alerts (
    id               uuid PRIMARY KEY,
    stable_asset_id  uuid NOT NULL REFERENCES stable_assets(id),
    kind             text NOT NULL CHECK (kind IN ('PRICE_DEVIATION','PRICE_STALE','RESERVE_SHORTFALL',
                                                   'RESERVE_PRICE_STALE','SUPPLY_MISMATCH')),
    severity         text NOT NULL CHECK (severity IN ('WARNING','CRITICAL')),
    observed         jsonb NOT NULL,
    breaker_tripped  boolean NOT NULL,
    observed_at      timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX stable_peg_alerts_asset_idx ON stable_peg_alerts (stable_asset_id, observed_at DESC);
CREATE TRIGGER stable_peg_alerts_immutable BEFORE UPDATE OR DELETE ON stable_peg_alerts
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A machine-verifiable reserve and supply snapshot. NOT an attestation: a signed, hash-linked record of
-- what the sandbox books and the simulated chain said at one instant. body is the exact canonical JSON
-- that body_hash covers; prev_hash is the previous snapshot's body_hash.
CREATE TABLE stable_reserve_snapshots (
    id                 uuid PRIMARY KEY,
    stable_asset_id    uuid NOT NULL REFERENCES stable_assets(id),
    seq                bigint NOT NULL CHECK (seq >= 1),
    body               text NOT NULL,
    body_hash          bytea NOT NULL CHECK (length(body_hash) = 32),
    prev_hash          bytea CHECK (prev_hash IS NULL OR length(prev_hash) = 32),
    signature          bytea NOT NULL CHECK (length(signature) = 64),
    signer_public_key  bytea NOT NULL CHECK (length(signer_public_key) = 32),
    evidence_ref       text NOT NULL,
    taken_at           timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    UNIQUE (stable_asset_id, seq),
    CONSTRAINT stable_reserve_snapshots_first_has_no_prev CHECK ((seq = 1) = (prev_hash IS NULL))
);
CREATE TRIGGER stable_reserve_snapshots_immutable BEFORE UPDATE OR DELETE ON stable_reserve_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_stable_snapshot_extends_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    last record;
BEGIN
    SELECT seq, body_hash INTO last FROM stable_reserve_snapshots
     WHERE stable_asset_id = NEW.stable_asset_id ORDER BY seq DESC LIMIT 1;
    IF NEW.seq <> coalesce(last.seq, 0) + 1 OR NEW.prev_hash IS DISTINCT FROM last.body_hash THEN
        RAISE EXCEPTION 'SNAPSHOT_CHAIN_BROKEN: snapshot % of stable asset % does not extend the chain at %',
            NEW.seq, NEW.stable_asset_id, coalesce(last.seq, 0) USING ERRCODE = 'SV007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_stable_snapshot_extends_chain() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER stable_reserve_snapshots_extend BEFORE INSERT ON stable_reserve_snapshots
    FOR EACH ROW EXECUTE FUNCTION cp_stable_snapshot_extends_chain();

-- ---------------------------------------------------------------------------
-- 6. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON stable_assets, stable_asset_transitions, stable_reserve_assets,
    stable_mint_requests, stable_mint_request_transitions, stable_burn_requests, stable_burn_request_transitions,
    stable_peg_alerts, stable_reserve_snapshots TO cp_app;
GRANT UPDATE (status) ON stable_assets TO cp_app;
GRANT UPDATE (status, failure_code, journal_transaction_id, final_slot) ON stable_mint_requests, stable_burn_requests TO cp_app;
GRANT SELECT ON stable_assets, stable_asset_transitions, stable_reserve_assets, stable_mint_requests,
    stable_mint_request_transitions, stable_burn_requests, stable_burn_request_transitions, stable_peg_alerts,
    stable_reserve_snapshots TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the simulated-chain history and stable-asset books are never dropped
