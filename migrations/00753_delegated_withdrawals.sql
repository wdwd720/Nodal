-- +goose Up
-- Delegated agent withdrawals (ADR-0020, docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5-§7, goal §17;
-- Go: internal/delegation). Capability AGENT_WITHDRAWALS, modes SANDBOX and DEVNET.
--
-- An agent never withdraws: the settlement compiler forbids SIMULATED_WITHDRAW to agents at every level.
-- An agent PROPOSES a withdrawal under a capability that grants PROPOSE_WITHDRAWAL; the customer's
-- withdrawal policy auto-approves it only in low-risk test mode, otherwise the account's owner decides;
-- the withdrawal itself is a service's WITHDRAW intent, signed through the delegated signing boundary of
-- 00752 and posted as SANDBOX_WITHDRAWAL.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. Withdrawal destinations are the account owner's allowlist: written by the owner, frozen (an address
--      is never edited, only removed, ACTIVE -> REMOVED, bound to a transition row), and aged from the
--      moment they were added.
--   2. Withdrawal policies are versioned, immutable, written by the account owner, and a withdrawal is
--      evaluated under the account's latest version.
--   3. A delegated withdrawal is proposed for exactly one capability use, capital reservation and WITHDRAW
--      intent of its own account, to an ACTIVE destination of that account with that exact chain and
--      address, inside the capability's allowlists, within the policy's per-withdrawal, daily (UTC day,
--      serialized per account) and 24-hour velocity limits, and auto-approvable only in LOCAL, TEST or
--      DEV and only inside every auto-approval bound of its policy.
--   4. The destination, amount, asset, chain and plan are frozen from the proposal: an approval binds the
--      plan hash, so no address can be substituted and no amount mutated after approval. Everything a
--      withdrawal gains as it advances is set once, along an acyclic state machine bound to transition rows.
--   5. A frozen account stops withdrawals at every step the database sees before submission: a withdrawal
--      cannot be proposed, approved or signed while its account is not ACTIVE or an ACCOUNT_FREEZE,
--      WITHDRAWALS_DISABLE, GLOBAL_NEW_RISK_KILL or CAPABILITY_DISABLE (AGENT_WITHDRAWALS) switch is on.
--   6. A withdrawal reaches SIGNED only with an approval bound to its plan hash (a strong-authenticated
--      owner's when the policy asks for it), an ACTIVE capability valid at the signing time, its cooling
--      period elapsed, its destination still ACTIVE, and an APPROVED signing decision over the exact message.
--   7. A SETTLED withdrawal names the SANDBOX_WITHDRAWAL journal (Dr WITHDRAWN / Cr WALLET) of exactly its
--      amount and asset, and the APPLIED effect that applied it.
--
-- Custom SQLSTATEs:
--   DW001 an illegal destination or withdrawal status transition, or a transition row describing one.
--   DW002 a frozen column of a destination or withdrawal changed, or a set-once column changed.
--   DW003 a destination, policy, withdrawal, approval or signing decision that crosses an account boundary
--         or disagrees with the rows it names.
--   DW004 a withdrawal beyond its policy: a per-withdrawal, daily or velocity limit, an auto-approval
--         outside its bounds, or an approval without the strong authentication the policy requires.
--   DW005 a withdrawal proposed, approved or signed while its account is frozen.
--   DW006 a withdrawal that reaches AWAITING_APPROVAL, APPROVED, DECLINED or SIGNED without what licenses it.
--   DW007 a SETTLED withdrawal whose journal or effect is not exactly its SANDBOX_WITHDRAWAL.

-- ---------------------------------------------------------------------------
-- 0. Shared helpers
-- ---------------------------------------------------------------------------

-- Whether withdrawals of an account are stopped right now: the account is not ACTIVE, or a switch that
-- blocks the WITHDRAW action class (internal/killswitch Blocks) applies to it.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawals_frozen(p_account uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT NOT EXISTS (SELECT 1 FROM accounts WHERE id = p_account AND status = 'ACTIVE')
        OR EXISTS (SELECT 1 FROM kill_switches k WHERE k.active AND (
               (k.kind = 'ACCOUNT_FREEZE' AND k.scope_id = p_account::text)
            OR k.kind IN ('WITHDRAWALS_DISABLE','GLOBAL_NEW_RISK_KILL')
            OR (k.kind = 'CAPABILITY_DISABLE' AND k.scope_id = 'AGENT_WITHDRAWALS')));
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_withdrawals_frozen(uuid) SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 1. Destinations
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_withdrawal_destinations (
    id                     uuid PRIMARY KEY,
    account_id             uuid NOT NULL REFERENCES accounts(id),
    chain_id               text NOT NULL CHECK (chain_id IN ('sbx-solana','sbx-ethereum')),
    address                text NOT NULL CHECK (cp_delegation_addresses_valid(ARRAY[address])),
    label                  text NOT NULL CHECK (length(label) BETWEEN 1 AND 120),
    status                 text NOT NULL CHECK (status IN ('ACTIVE','REMOVED')),
    added_by_actor_type    text NOT NULL CHECK (added_by_actor_type = 'USER'),
    added_by_actor_id      text NOT NULL,
    added_at               timestamptz NOT NULL,
    removed_by_actor_type  text CHECK (removed_by_actor_type IN ('USER','OPERATOR','SYSTEM')),
    removed_by_actor_id    text,
    removed_at             timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delegated_withdrawal_destinations_removal CHECK (
        (status = 'REMOVED') = (removed_at IS NOT NULL)
        AND (removed_at IS NULL) = (removed_by_actor_type IS NULL)
        AND (removed_at IS NULL) = (removed_by_actor_id IS NULL))
);
CREATE UNIQUE INDEX delegated_withdrawal_destinations_active_key
    ON delegated_withdrawal_destinations (account_id, chain_id, address) WHERE status = 'ACTIVE';
CREATE TRIGGER delegated_withdrawal_destinations_updated_at BEFORE UPDATE ON delegated_withdrawal_destinations
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_destination_birth() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    owner      uuid;
    acct_kind  text;
BEGIN
    IF NEW.status IS DISTINCT FROM 'ACTIVE' OR NEW.removed_at IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_DESTINATION_NOT_BORN_ACTIVE: destination % must be added ACTIVE', NEW.id USING ERRCODE = 'DW001';
    END IF;
    SELECT owner_user_id, kind INTO owner, acct_kind FROM accounts WHERE id = NEW.account_id;
    IF acct_kind IS DISTINCT FROM 'CUSTOMER' OR owner::text IS DISTINCT FROM NEW.added_by_actor_id THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: only the owner of customer account % may allowlist a withdrawal destination', NEW.account_id
            USING ERRCODE = 'DW003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_destination_birth() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawal_destinations_birth BEFORE INSERT ON delegated_withdrawal_destinations
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_destination_birth();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_destination_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('NONE','ACTIVE'), ('ACTIVE','REMOVED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_destination_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    removal text[] := ARRAY['removed_by_actor_type','removed_by_actor_id','removed_at'];
    owner   uuid;
BEGIN
    IF (to_jsonb(NEW) - removal - ARRAY['status','updated_at']) IS DISTINCT FROM
       (to_jsonb(OLD) - removal - ARRAY['status','updated_at']) THEN
        RAISE EXCEPTION 'DELEGATION_DESTINATION_FROZEN: destination % is never edited, only removed', OLD.id USING ERRCODE = 'DW002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_delegation_destination_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: destination % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'DW001';
    END IF;
    IF OLD.status = NEW.status AND (to_jsonb(NEW) - 'updated_at') IS DISTINCT FROM (to_jsonb(OLD) - 'updated_at') THEN
        RAISE EXCEPTION 'DELEGATION_DESTINATION_FROZEN: destination % removal details change only with the removal', OLD.id
            USING ERRCODE = 'DW002';
    END IF;
    IF NEW.status = 'REMOVED' AND OLD.status = 'ACTIVE' AND NEW.removed_by_actor_type = 'USER' THEN
        SELECT owner_user_id INTO owner FROM accounts WHERE id = NEW.account_id;
        IF owner::text IS DISTINCT FROM NEW.removed_by_actor_id THEN
            RAISE EXCEPTION 'DELEGATION_BOUNDARY: only the owner of account % may remove destination % as a user', NEW.account_id, NEW.id
                USING ERRCODE = 'DW003';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_destination_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawal_destinations_guard BEFORE UPDATE ON delegated_withdrawal_destinations
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_destination_guard();

CREATE TABLE delegated_withdrawal_destination_transitions (
    id              uuid PRIMARY KEY,
    destination_id  uuid NOT NULL REFERENCES delegated_withdrawal_destinations(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delegated_withdrawal_destination_transitions_idx
    ON delegated_withdrawal_destination_transitions (destination_id, occurred_at);
CREATE TRIGGER delegated_withdrawal_destination_transitions_immutable
    BEFORE UPDATE OR DELETE ON delegated_withdrawal_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_destination_transition_row_legal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT cp_delegation_destination_transition_legal(NEW.from_status, NEW.to_status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: a transition row cannot describe destination % moving % -> %',
            NEW.destination_id, NEW.from_status, NEW.to_status USING ERRCODE = 'DW001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER delegated_withdrawal_destination_transitions_legal BEFORE INSERT ON delegated_withdrawal_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_destination_transition_row_legal();
-- A destination changes status at most once, so the single-edge binding of 00726 suffices.
CREATE TRIGGER delegated_withdrawal_destination_transitions_flag_edge AFTER INSERT ON delegated_withdrawal_destination_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('destination_id', 'from_status', 'to_status', 'delegated_destination_status');
CREATE CONSTRAINT TRIGGER delegated_withdrawal_destinations_require_transition AFTER UPDATE OF status ON delegated_withdrawal_destinations
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'delegated_destination_status');

-- ---------------------------------------------------------------------------
-- 2. Withdrawal policies: versioned and immutable
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_withdrawal_policies (
    id                            uuid PRIMARY KEY,
    account_id                    uuid NOT NULL REFERENCES accounts(id),
    version                       integer NOT NULL CHECK (version >= 1),
    auto_approve_enabled          boolean NOT NULL,
    auto_approve_max_minor        bigint NOT NULL CHECK (auto_approve_max_minor >= 0),
    auto_approve_max_risk_score   integer NOT NULL CHECK (auto_approve_max_risk_score BETWEEN 0 AND 100),
    max_risk_score                integer NOT NULL CHECK (max_risk_score BETWEEN 0 AND 100),
    per_withdrawal_limit_minor    bigint NOT NULL CHECK (per_withdrawal_limit_minor > 0),
    daily_limit_minor             bigint NOT NULL,
    velocity_max_count            integer NOT NULL CHECK (velocity_max_count BETWEEN 1 AND 1000),
    min_destination_age_seconds   bigint NOT NULL CHECK (min_destination_age_seconds BETWEEN 0 AND 31536000),
    min_account_age_seconds       bigint NOT NULL CHECK (min_account_age_seconds BETWEEN 0 AND 31536000),
    cooling_period_seconds        bigint NOT NULL CHECK (cooling_period_seconds BETWEEN 0 AND 604800),
    strong_auth_required          boolean NOT NULL,
    strong_auth_max_age_seconds   bigint NOT NULL CHECK (strong_auth_max_age_seconds BETWEEN 1 AND 86400),
    policy_hash                   bytea NOT NULL CHECK (length(policy_hash) = 32),
    created_by_actor_type         text NOT NULL CHECK (created_by_actor_type = 'USER'),
    created_by_actor_id           text NOT NULL,
    effective_at                  timestamptz NOT NULL,
    created_at                    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, version),
    CONSTRAINT delegated_withdrawal_policies_limits CHECK (daily_limit_minor >= per_withdrawal_limit_minor
        AND auto_approve_max_minor <= per_withdrawal_limit_minor AND auto_approve_max_risk_score <= max_risk_score)
);
CREATE TRIGGER delegated_withdrawal_policies_immutable BEFORE UPDATE OR DELETE ON delegated_withdrawal_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_policy_birth() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    owner      uuid;
    acct_kind  text;
    latest     integer;
BEGIN
    PERFORM pg_advisory_xact_lock(hashtextextended('delegation/withdrawals/' || NEW.account_id::text, 0));
    SELECT owner_user_id, kind INTO owner, acct_kind FROM accounts WHERE id = NEW.account_id;
    SELECT coalesce(max(version), 0) INTO latest FROM delegated_withdrawal_policies WHERE account_id = NEW.account_id;
    IF acct_kind IS DISTINCT FROM 'CUSTOMER' OR owner::text IS DISTINCT FROM NEW.created_by_actor_id OR NEW.version <> latest + 1 THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: withdrawal policy version % of account % must be its owner''s next version (after %)',
            NEW.version, NEW.account_id, latest USING ERRCODE = 'DW003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_policy_birth() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawal_policies_birth BEFORE INSERT ON delegated_withdrawal_policies
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_policy_birth();

-- ---------------------------------------------------------------------------
-- 3. Delegated withdrawals
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_withdrawals (
    id                      uuid PRIMARY KEY,
    capability_id           uuid NOT NULL REFERENCES delegated_capabilities(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    agent_id                uuid NOT NULL REFERENCES agents(id),
    proposal_nonce          numeric(20,0) NOT NULL CHECK (proposal_nonce > 0),
    intent_id               uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    usage_id                uuid NOT NULL UNIQUE REFERENCES delegated_capability_usage(id),
    reservation_id          uuid NOT NULL UNIQUE REFERENCES asset_reservations(id),
    policy_id               uuid NOT NULL REFERENCES delegated_withdrawal_policies(id),
    destination_id          uuid NOT NULL REFERENCES delegated_withdrawal_destinations(id),
    environment             text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    mode                    text NOT NULL CHECK (mode IN ('SANDBOX','DEVNET')),
    chain_id                text NOT NULL CHECK (chain_id IN ('sbx-solana','sbx-ethereum')),
    wallet                  text NOT NULL CHECK (cp_delegation_addresses_valid(ARRAY[wallet])),
    destination_address     text NOT NULL CHECK (cp_delegation_addresses_valid(ARRAY[destination_address])),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    amount                  numeric(38,0) NOT NULL CHECK (amount > 0),
    notional_usd_minor      bigint NOT NULL CHECK (notional_usd_minor > 0),
    proposed_at             timestamptz NOT NULL,
    withdrawal_day          date NOT NULL,
    approval_expires_at     timestamptz NOT NULL,
    executable_at           timestamptz NOT NULL,
    evaluation_outcome      text NOT NULL CHECK (evaluation_outcome IN ('AUTO_APPROVE','HUMAN_REQUIRED')),
    risk_score              integer NOT NULL CHECK (risk_score BETWEEN 0 AND 100),
    evaluation              jsonb NOT NULL,
    plan                    jsonb NOT NULL,
    plan_hash               bytea NOT NULL UNIQUE CHECK (length(plan_hash) = 32),
    status                  text NOT NULL CHECK (status IN ('PROPOSED','AWAITING_APPROVAL','APPROVED','SIGNED','SUBMITTED',
                                                            'UNKNOWN','SETTLED','REJECTED','DECLINED','CANCELLED','FAILED')),
    -- Set once as the withdrawal advances; never changed once set (DW002).
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
    UNIQUE (capability_id, proposal_nonce),
    CONSTRAINT delegated_withdrawals_day CHECK (withdrawal_day = (proposed_at AT TIME ZONE 'UTC')::date),
    CONSTRAINT delegated_withdrawals_windows CHECK (approval_expires_at > proposed_at AND executable_at >= proposed_at),
    CONSTRAINT delegated_withdrawals_auto_only_in_test CHECK (evaluation_outcome <> 'AUTO_APPROVE' OR environment IN ('LOCAL','TEST','DEV')),
    CONSTRAINT delegated_withdrawals_signed_has_tx CHECK (
        status IN ('PROPOSED','AWAITING_APPROVAL','APPROVED','REJECTED','DECLINED','CANCELLED')
        OR (tx_id IS NOT NULL AND signed_tx IS NOT NULL AND message IS NOT NULL AND signed_at IS NOT NULL
            AND recent_blockhash IS NOT NULL AND blockhash_height IS NOT NULL AND effect_id IS NOT NULL)),
    CONSTRAINT delegated_withdrawals_settled_has_journal CHECK ((status = 'SETTLED') = (journal_transaction_id IS NOT NULL)),
    CONSTRAINT delegated_withdrawals_failure_code CHECK (
        (status IN ('REJECTED','DECLINED','CANCELLED','FAILED')) = (failure_code IS NOT NULL))
);
CREATE INDEX delegated_withdrawals_open_idx ON delegated_withdrawals (status, created_at)
    WHERE status NOT IN ('SETTLED','REJECTED','DECLINED','CANCELLED','FAILED');
CREATE INDEX delegated_withdrawals_account_day_idx ON delegated_withdrawals (account_id, withdrawal_day);
CREATE TRIGGER delegated_withdrawals_updated_at BEFORE UPDATE ON delegated_withdrawals
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- A withdrawal is proposed for its own capability, usage, reservation and intent, to an ACTIVE allowlisted
-- destination of its account with that exact chain and address, under the account's latest policy and
-- inside every limit of it, while the account is not frozen. Proposals of one account are serialized.
-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_birth() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cap        delegated_capabilities%ROWTYPE;
    u          delegated_capability_usage%ROWTYPE;
    d          delegated_withdrawal_destinations%ROWTYPE;
    p          delegated_withdrawal_policies%ROWTYPE;
    r          record;
    i          record;
    a          record;
    acct_born  timestamptz;
    latest     integer;
    day_total  numeric;
    recent     integer;
BEGIN
    IF NEW.status IS DISTINCT FROM 'PROPOSED' OR NEW.message IS NOT NULL OR NEW.tx_id IS NOT NULL OR NEW.signed_tx IS NOT NULL
       OR NEW.effect_id IS NOT NULL OR NEW.journal_transaction_id IS NOT NULL OR NEW.failure_code IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_WITHDRAWAL_NOT_BORN_PROPOSED: withdrawal % must be proposed with no outcome', NEW.id
            USING ERRCODE = 'DW001';
    END IF;
    PERFORM pg_advisory_xact_lock(hashtextextended('delegation/withdrawals/' || NEW.account_id::text, 0));
    IF cp_delegation_withdrawals_frozen(NEW.account_id) THEN
        RAISE EXCEPTION 'DELEGATION_ACCOUNT_FROZEN: account % may not propose withdrawals now', NEW.account_id USING ERRCODE = 'DW005';
    END IF;
    SELECT * INTO cap FROM delegated_capabilities WHERE id = NEW.capability_id;
    SELECT * INTO u FROM delegated_capability_usage WHERE id = NEW.usage_id;
    SELECT * INTO d FROM delegated_withdrawal_destinations WHERE id = NEW.destination_id;
    SELECT * INTO p FROM delegated_withdrawal_policies WHERE id = NEW.policy_id;
    SELECT coalesce(max(version), 0) INTO latest FROM delegated_withdrawal_policies WHERE account_id = NEW.account_id;
    SELECT account_id, asset_id, quantity, economic_intent_id INTO r FROM asset_reservations WHERE id = NEW.reservation_id;
    SELECT account_id, kind, actor_type, delegation_id, asset_id, quantity INTO i FROM economic_intents WHERE id = NEW.intent_id;
    SELECT kind, chain INTO a FROM assets WHERE id = NEW.asset_id;
    SELECT created_at INTO acct_born FROM accounts WHERE id = NEW.account_id;
    IF cap.account_id IS DISTINCT FROM NEW.account_id OR cap.agent_id IS DISTINCT FROM NEW.agent_id OR cap.mode IS DISTINCT FROM NEW.mode
       OR cap.environment IS DISTINCT FROM NEW.environment
       OR NOT ('PROPOSE_WITHDRAWAL' = ANY (cap.action_classes)) OR NOT (NEW.chain_id = ANY (cap.chain_allowlist))
       OR NOT (NEW.destination_address = ANY (cap.destination_allowlist))
       OR u.capability_id IS DISTINCT FROM NEW.capability_id OR u.subject_kind IS DISTINCT FROM 'WITHDRAWAL'
       OR u.subject_id IS DISTINCT FROM NEW.id OR u.notional_minor IS DISTINCT FROM NEW.notional_usd_minor
       OR d.account_id IS DISTINCT FROM NEW.account_id OR d.status IS DISTINCT FROM 'ACTIVE'
       OR d.chain_id IS DISTINCT FROM NEW.chain_id OR d.address IS DISTINCT FROM NEW.destination_address
       OR p.account_id IS DISTINCT FROM NEW.account_id OR p.version IS DISTINCT FROM latest
       OR r.account_id IS DISTINCT FROM NEW.account_id OR r.asset_id IS DISTINCT FROM NEW.asset_id
       OR r.quantity IS DISTINCT FROM NEW.amount OR r.economic_intent_id IS DISTINCT FROM NEW.intent_id
       OR i.account_id IS DISTINCT FROM NEW.account_id OR i.kind IS DISTINCT FROM 'WITHDRAW'
       OR i.actor_type IS DISTINCT FROM 'SERVICE' OR i.delegation_id IS DISTINCT FROM NEW.capability_id
       OR i.asset_id IS DISTINCT FROM NEW.asset_id OR i.quantity IS DISTINCT FROM NEW.amount
       OR a.kind IS DISTINCT FROM 'SIMULATED' OR a.chain IS DISTINCT FROM NEW.chain_id
       OR extract(epoch FROM (NEW.executable_at - NEW.proposed_at)) IS DISTINCT FROM p.cooling_period_seconds::numeric THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: withdrawal % disagrees with its capability, usage, destination, policy, reservation, intent or asset',
            NEW.id USING ERRCODE = 'DW003';
    END IF;
    SELECT coalesce(sum(notional_usd_minor), 0) INTO day_total FROM delegated_withdrawals
     WHERE account_id = NEW.account_id AND withdrawal_day = NEW.withdrawal_day
       AND status NOT IN ('REJECTED','DECLINED','CANCELLED','FAILED');
    SELECT count(*) INTO recent FROM delegated_withdrawals
     WHERE account_id = NEW.account_id AND proposed_at > NEW.proposed_at - interval '24 hours' AND proposed_at <= NEW.proposed_at
       AND status NOT IN ('REJECTED','DECLINED','CANCELLED','FAILED');
    IF NEW.notional_usd_minor > p.per_withdrawal_limit_minor OR day_total + NEW.notional_usd_minor > p.daily_limit_minor
       OR recent + 1 > p.velocity_max_count OR NEW.risk_score > p.max_risk_score THEN
        RAISE EXCEPTION 'DELEGATION_POLICY_LIMIT: withdrawal % of % (day total %, % in 24h, risk %) is beyond policy % of account %',
            NEW.id, NEW.notional_usd_minor, day_total, recent, NEW.risk_score, p.version, NEW.account_id USING ERRCODE = 'DW004';
    END IF;
    IF NEW.evaluation_outcome = 'AUTO_APPROVE' AND NOT (p.auto_approve_enabled
          AND NEW.notional_usd_minor <= p.auto_approve_max_minor AND NEW.risk_score <= p.auto_approve_max_risk_score
          AND greatest(extract(epoch FROM (NEW.proposed_at - d.added_at)), 0) >= p.min_destination_age_seconds
          AND greatest(extract(epoch FROM (NEW.proposed_at - acct_born)), 0) >= p.min_account_age_seconds) THEN
        RAISE EXCEPTION 'DELEGATION_POLICY_LIMIT: withdrawal % is outside every auto-approval bound of policy % of account %',
            NEW.id, p.version, NEW.account_id USING ERRCODE = 'DW004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_withdrawal_birth() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawals_birth BEFORE INSERT ON delegated_withdrawals
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_withdrawal_birth();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('NONE','PROPOSED'),
        ('PROPOSED','APPROVED'), ('PROPOSED','AWAITING_APPROVAL'),
        ('AWAITING_APPROVAL','APPROVED'), ('AWAITING_APPROVAL','DECLINED'), ('AWAITING_APPROVAL','CANCELLED'),
        ('AWAITING_APPROVAL','REJECTED'),
        ('APPROVED','SIGNED'), ('APPROVED','CANCELLED'), ('APPROVED','REJECTED'),
        ('SIGNED','SUBMITTED'), ('SIGNED','UNKNOWN'), ('SIGNED','FAILED'),
        ('SUBMITTED','SETTLED'), ('SUBMITTED','FAILED'),
        ('UNKNOWN','SETTLED'), ('UNKNOWN','FAILED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_guard() RETURNS trigger
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
        RAISE EXCEPTION 'DELEGATION_WITHDRAWAL_FROZEN: withdrawal % may not change its destination, amount, asset or plan', OLD.id
            USING ERRCODE = 'DW002';
    END IF;
    FOREACH col IN ARRAY once LOOP
        IF o->col IS DISTINCT FROM 'null'::jsonb AND o->col IS DISTINCT FROM n->col THEN
            RAISE EXCEPTION 'DELEGATION_WITHDRAWAL_FROZEN: withdrawal % already set %', OLD.id, col USING ERRCODE = 'DW002';
        END IF;
    END LOOP;
    IF OLD.status IS NOT DISTINCT FROM NEW.status THEN
        RETURN NEW;
    END IF;
    IF NOT cp_delegation_withdrawal_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: withdrawal % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'DW001';
    END IF;
    IF NEW.status IN ('APPROVED','SIGNED') AND cp_delegation_withdrawals_frozen(NEW.account_id) THEN
        RAISE EXCEPTION 'DELEGATION_ACCOUNT_FROZEN: withdrawal % cannot become % while account % is frozen', NEW.id, NEW.status, NEW.account_id
            USING ERRCODE = 'DW005';
    END IF;
    IF NEW.status = 'AWAITING_APPROVAL' AND NEW.evaluation_outcome IS DISTINCT FROM 'HUMAN_REQUIRED' THEN
        RAISE EXCEPTION 'DELEGATION_UNLICENSED: withdrawal % was not evaluated as needing a human', NEW.id USING ERRCODE = 'DW006';
    END IF;
    IF NEW.status IN ('APPROVED','DECLINED') AND NOT EXISTS (
            SELECT 1 FROM delegated_withdrawal_approvals a WHERE a.withdrawal_id = NEW.id AND a.plan_hash = NEW.plan_hash
               AND a.decision = CASE NEW.status WHEN 'APPROVED' THEN 'APPROVE' ELSE 'DECLINE' END) THEN
        RAISE EXCEPTION 'DELEGATION_UNLICENSED: withdrawal % cannot become % without that decision on its plan', NEW.id, NEW.status
            USING ERRCODE = 'DW006';
    END IF;
    IF NEW.status = 'SIGNED' THEN
        -- A share lock, as for swaps: a revocation is ordered before or after this signature, never across it.
        SELECT * INTO cap FROM delegated_capabilities WHERE id = NEW.capability_id FOR SHARE;
        IF cap.status IS DISTINCT FROM 'ACTIVE' OR NEW.signed_at IS NULL OR NEW.signed_at < cap.granted_at
           OR NEW.signed_at >= cap.expires_at OR NEW.signed_at < NEW.executable_at OR NEW.message IS NULL
           OR NOT EXISTS (SELECT 1 FROM delegated_withdrawal_approvals a WHERE a.withdrawal_id = NEW.id
                             AND a.decision = 'APPROVE' AND a.plan_hash = NEW.plan_hash)
           OR NOT EXISTS (SELECT 1 FROM delegated_withdrawal_destinations dd WHERE dd.id = NEW.destination_id AND dd.status = 'ACTIVE')
           OR NOT EXISTS (SELECT 1 FROM delegated_signing_decisions sd WHERE sd.withdrawal_id = NEW.id AND sd.verdict = 'APPROVED'
                             AND sd.plan_hash = NEW.plan_hash AND sd.message_sha256 = sha256(NEW.message)) THEN
            RAISE EXCEPTION 'DELEGATION_UNLICENSED: withdrawal % cannot be signed at % (capability %, approval, cooling until %, destination, decision)',
                NEW.id, NEW.signed_at, cap.status, NEW.executable_at USING ERRCODE = 'DW006';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_withdrawal_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawals_guard BEFORE UPDATE ON delegated_withdrawals
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_withdrawal_guard();

CREATE TABLE delegated_withdrawal_transitions (
    id             uuid PRIMARY KEY,
    withdrawal_id  uuid NOT NULL REFERENCES delegated_withdrawals(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    actor_type     text NOT NULL,
    actor_id       text NOT NULL,
    reason         text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    occurred_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX delegated_withdrawal_transitions_idx ON delegated_withdrawal_transitions (withdrawal_id, occurred_at);
CREATE TRIGGER delegated_withdrawal_transitions_immutable BEFORE UPDATE OR DELETE ON delegated_withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_transition_row_legal() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT cp_delegation_withdrawal_transition_legal(NEW.from_status, NEW.to_status) THEN
        RAISE EXCEPTION 'DELEGATION_TRANSITION_ILLEGAL: a transition row cannot describe withdrawal % moving % -> %',
            NEW.withdrawal_id, NEW.from_status, NEW.to_status USING ERRCODE = 'DW001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER delegated_withdrawal_transitions_legal BEFORE INSERT ON delegated_withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_withdrawal_transition_row_legal();
-- Acyclic and enforced for every writer above, so the accumulating binding of 00733 applies.
CREATE TRIGGER delegated_withdrawal_transitions_flag_edge AFTER INSERT ON delegated_withdrawal_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('withdrawal_id', 'from_status', 'to_status', 'delegated_withdrawal_status');
CREATE CONSTRAINT TRIGGER delegated_withdrawals_require_transition AFTER UPDATE OF status ON delegated_withdrawals
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'delegated_withdrawal_status');

-- ---------------------------------------------------------------------------
-- 4. Approvals: the policy's (auto, test mode only) or the account owner's, bound to the plan hash
-- ---------------------------------------------------------------------------

CREATE TABLE delegated_withdrawal_approvals (
    id                   uuid PRIMARY KEY,
    withdrawal_id        uuid NOT NULL UNIQUE REFERENCES delegated_withdrawals(id),
    decision             text NOT NULL CHECK (decision IN ('APPROVE','DECLINE')),
    plan_hash            bytea NOT NULL CHECK (length(plan_hash) = 32),
    approver_actor_type  text NOT NULL CHECK (approver_actor_type IN ('USER','SERVICE')),
    approver_actor_id    text NOT NULL,
    authenticated_at     timestamptz,
    decided_at           timestamptz NOT NULL,
    reason               text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    created_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT delegated_withdrawal_approvals_user_authenticated CHECK (approver_actor_type <> 'USER' OR authenticated_at IS NOT NULL)
);
CREATE TRIGGER delegated_withdrawal_approvals_immutable BEFORE UPDATE OR DELETE ON delegated_withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_approval_binds() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    w      delegated_withdrawals%ROWTYPE;
    p      delegated_withdrawal_policies%ROWTYPE;
    owner  uuid;
BEGIN
    SELECT * INTO w FROM delegated_withdrawals WHERE id = NEW.withdrawal_id;
    SELECT * INTO p FROM delegated_withdrawal_policies WHERE id = w.policy_id;
    SELECT owner_user_id INTO owner FROM accounts WHERE id = w.account_id;
    IF w.plan_hash IS DISTINCT FROM NEW.plan_hash OR NEW.decided_at >= w.approval_expires_at
       OR (NEW.approver_actor_type = 'SERVICE' AND (NEW.decision <> 'APPROVE' OR w.status IS DISTINCT FROM 'PROPOSED'
                                                  OR w.evaluation_outcome IS DISTINCT FROM 'AUTO_APPROVE'))
       OR (NEW.approver_actor_type = 'USER' AND (owner::text IS DISTINCT FROM NEW.approver_actor_id
                                               OR w.status IS DISTINCT FROM 'AWAITING_APPROVAL')) THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: decision on withdrawal % must bind its plan, before its deadline, by its policy (auto) or its owner (human)',
            NEW.withdrawal_id USING ERRCODE = 'DW003';
    END IF;
    IF NEW.approver_actor_type = 'USER' AND p.strong_auth_required
       AND (NEW.authenticated_at > NEW.decided_at + interval '2 minutes'
            OR extract(epoch FROM (NEW.decided_at - NEW.authenticated_at)) > p.strong_auth_max_age_seconds) THEN
        RAISE EXCEPTION 'DELEGATION_POLICY_LIMIT: policy % of account % requires an authentication within % seconds of the decision on withdrawal %',
            p.version, w.account_id, p.strong_auth_max_age_seconds, NEW.withdrawal_id USING ERRCODE = 'DW004';
    END IF;
    IF NEW.decision = 'APPROVE' AND cp_delegation_withdrawals_frozen(w.account_id) THEN
        RAISE EXCEPTION 'DELEGATION_ACCOUNT_FROZEN: withdrawal % cannot be approved while account % is frozen', NEW.withdrawal_id, w.account_id
            USING ERRCODE = 'DW005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_withdrawal_approval_binds() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER delegated_withdrawal_approvals_binds BEFORE INSERT ON delegated_withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION cp_delegation_withdrawal_approval_binds();

-- ---------------------------------------------------------------------------
-- 5. Signing decisions now cover withdrawals too
-- ---------------------------------------------------------------------------

ALTER TABLE delegated_signing_decisions ADD CONSTRAINT delegated_signing_decisions_withdrawal_fk
    FOREIGN KEY (withdrawal_id) REFERENCES delegated_withdrawals(id);

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_delegation_decision_binds() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s  delegated_swaps%ROWTYPE;
    w  delegated_withdrawals%ROWTYPE;
BEGIN
    IF NEW.swap_id IS NOT NULL THEN
        SELECT * INTO s FROM delegated_swaps WHERE id = NEW.swap_id;
        IF s.account_id IS DISTINCT FROM NEW.account_id OR s.capability_id IS DISTINCT FROM NEW.capability_id
           OR s.plan_hash IS DISTINCT FROM NEW.plan_hash OR s.message IS NULL OR sha256(s.message) IS DISTINCT FROM NEW.message_sha256
           OR NOT EXISTS (SELECT 1 FROM delegated_signer_keys k WHERE k.account_id = s.account_id AND k.chain_id = s.chain_id
                             AND k.public_key = NEW.signer_public_key AND k.public_key = s.wallet) THEN
            RAISE EXCEPTION 'DELEGATION_BOUNDARY: signing decision % does not describe swap %''s account, capability, plan, message and signer',
                NEW.id, NEW.swap_id USING ERRCODE = 'DG003';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO w FROM delegated_withdrawals WHERE id = NEW.withdrawal_id;
    IF w.id IS NULL OR w.account_id IS DISTINCT FROM NEW.account_id OR w.capability_id IS DISTINCT FROM NEW.capability_id
       OR w.plan_hash IS DISTINCT FROM NEW.plan_hash OR w.message IS NULL OR sha256(w.message) IS DISTINCT FROM NEW.message_sha256
       OR NOT EXISTS (SELECT 1 FROM delegated_signer_keys k WHERE k.account_id = w.account_id AND k.chain_id = w.chain_id
                         AND k.public_key = NEW.signer_public_key AND k.public_key = w.wallet) THEN
        RAISE EXCEPTION 'DELEGATION_BOUNDARY: signing decision % does not describe withdrawal %''s account, capability, plan, message and signer',
            NEW.id, NEW.withdrawal_id USING ERRCODE = 'DW003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd

-- ---------------------------------------------------------------------------
-- 6. A settled withdrawal is its SANDBOX_WITHDRAWAL and its applied effect, checked at COMMIT
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_delegation_withdrawal_outcome_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    j     record;
    legs  record;
    eff   record;
BEGIN
    SELECT kind, reference_type, reference_id, reversal_of INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM 'SANDBOX_WITHDRAWAL' OR j.reference_type IS DISTINCT FROM 'delegated_withdrawal'
       OR j.reference_id IS DISTINCT FROM NEW.id::text OR j.reversal_of IS NOT NULL THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: withdrawal % names journal % which is not its SANDBOX_WITHDRAWAL',
            NEW.id, NEW.journal_transaction_id USING ERRCODE = 'DW007';
    END IF;
    SELECT count(*) AS n,
           count(*) FILTER (WHERE la.code = 'WITHDRAWN' AND e.side = 'DEBIT') AS dr,
           count(*) FILTER (WHERE la.code = 'WALLET' AND e.side = 'CREDIT') AS cr,
           coalesce(bool_and(la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND e.asset_id = NEW.asset_id
                             AND e.quantity = NEW.amount), false) AS exact
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF legs.n <> 2 OR legs.dr <> 1 OR legs.cr <> 1 OR NOT legs.exact THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: journal % does not withdraw exactly % of asset % from account %',
            NEW.journal_transaction_id, NEW.amount, NEW.asset_id, NEW.account_id USING ERRCODE = 'DW007';
    END IF;
    SELECT status, subsystem, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'WITHDRAWALS'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'DELEGATION_SETTLEMENT_MISMATCH: withdrawal % is SETTLED but effect % did not apply journal %',
            NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'DW007';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_delegation_withdrawal_outcome_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER delegated_withdrawals_outcome_matches AFTER UPDATE OF status ON delegated_withdrawals
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.status = 'SETTLED')
    EXECUTE FUNCTION cp_delegation_withdrawal_outcome_matches();

-- ---------------------------------------------------------------------------
-- 7. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON delegated_withdrawal_destinations, delegated_withdrawal_destination_transitions,
    delegated_withdrawal_policies, delegated_withdrawals, delegated_withdrawal_transitions,
    delegated_withdrawal_approvals TO cp_app;
GRANT UPDATE (status, removed_by_actor_type, removed_by_actor_id, removed_at) ON delegated_withdrawal_destinations TO cp_app;
GRANT UPDATE (status, message, recent_blockhash, blockhash_height, signed_at, tx_id, signed_tx, effect_id,
              journal_transaction_id, failure_code) ON delegated_withdrawals TO cp_app;
GRANT SELECT ON delegated_withdrawal_destinations, delegated_withdrawal_destination_transitions,
    delegated_withdrawal_policies, delegated_withdrawals, delegated_withdrawal_transitions,
    delegated_withdrawal_approvals TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: withdrawal proposals, approvals and their history are never dropped
