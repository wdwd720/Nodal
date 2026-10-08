-- +goose Up
-- Customer-to-customer transfers of SIMULATED value and the sandbox faucet (ADR-0020,
-- docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5 and §10; Go: internal/transfers, internal/sandboxfund).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. internal_transfers is the register of transfers between two customer accounts. A transfer is
--      born PENDING with no outcome, is initiated by the user who owns the sender account, never names
--      the same account twice, has a frozen identity, and moves only along PENDING -> COMPLETED,
--      PENDING -> REJECTED and COMPLETED -> REVERSED. Its decision columns change only together with
--      the status that leaves PENDING and its reversal columns only with COMPLETED -> REVERSED, and
--      every status change is bound to a transition row naming both endpoints (the 00726 edge binding).
--   2. A COMPLETED transfer names the P2P_TRANSFER journal that moved exactly its quantity of exactly
--      its asset between exactly its two parties (sender Dr TRANSFER_OUT / Cr WALLET, recipient
--      Dr WALLET / Cr TRANSFER_IN) and the APPLIED economic effect that applied that journal. A
--      REVERSED transfer names the COMPENSATION journal with reversal_of = the original journal that
--      mirrors those four legs, and the effect is COMPENSATED by it. Checked at COMMIT, so the order of
--      the statements inside the transaction does not matter and the end state must be coherent.
--   3. Recipient handles, per-sender recipient rules (ALLOW / BLOCK / CLEAR) and sender modes (OPEN /
--      ALLOWLIST_ONLY) are append-only facts written by the owner of the account they configure.
--   4. sandbox_fundings records every faucet grant. A grant can only be of a SIMULATED asset, names the
--      SANDBOX_FUNDING journal that credited exactly it (Dr WALLET / Cr CAPITAL of its account and asset)
--      and the APPLIED effect for that journal, and is append-only.
--
-- Custom SQLSTATEs: PT001 a transfer, handle, rule or mode not written by the owner of the account,
-- PT002 transfer identity, decision or reversal changed outside its one legal status change,
-- PT003 transfer not born PENDING, PT004 illegal transfer status transition, PT005 transfer outcome
-- does not match its journal or its economic effect, SF001 faucet grant of an asset that is not
-- SIMULATED, SF002 faucet grant that does not match its journal or its economic effect.

-- ---------------------------------------------------------------------------
-- 1. Owner check shared by every table a customer configures
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_transfers_require_owner() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    account_col text := TG_ARGV[0];
    user_col    text := TG_ARGV[1];
    acct        uuid;
    usr         uuid;
    owner       uuid;
BEGIN
    EXECUTE format('SELECT ($1).%I, ($1).%I', account_col, user_col) INTO acct, usr USING NEW;
    SELECT owner_user_id INTO owner FROM accounts WHERE id = acct;
    IF owner IS NULL OR owner IS DISTINCT FROM usr THEN
        RAISE EXCEPTION 'TRANSFER_NOT_BY_OWNER: % row names account % and user %, but the account belongs to %',
            TG_TABLE_NAME, acct, usr, owner USING ERRCODE = 'PT001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_transfers_require_owner() SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 2. Recipient handles, recipient rules, sender modes
-- ---------------------------------------------------------------------------

CREATE TABLE transfer_handles (
    id                     uuid PRIMARY KEY,
    account_id             uuid NOT NULL UNIQUE REFERENCES accounts(id),
    -- Lower case, so "Alice" and "alice" can never be two people.
    handle                 text NOT NULL UNIQUE CHECK (handle ~ '^[a-z0-9][a-z0-9_.-]{2,31}$'),
    registered_by_user_id  uuid NOT NULL REFERENCES users(id),
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER transfer_handles_immutable BEFORE UPDATE OR DELETE ON transfer_handles
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER transfer_handles_owner BEFORE INSERT ON transfer_handles
    FOR EACH ROW EXECUTE FUNCTION cp_transfers_require_owner('account_id', 'registered_by_user_id');

CREATE TABLE transfer_recipient_rules (
    id                    uuid PRIMARY KEY,
    sender_account_id     uuid NOT NULL REFERENCES accounts(id),
    recipient_account_id  uuid NOT NULL REFERENCES accounts(id),
    -- The latest row for (sender, recipient) is the rule in force. CLEAR withdraws an earlier one.
    rule                  text NOT NULL CHECK (rule IN ('ALLOW','BLOCK','CLEAR')),
    set_by_user_id        uuid NOT NULL REFERENCES users(id),
    reason                text CHECK (reason IS NULL OR length(reason) BETWEEN 1 AND 500),
    effective_at          timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfer_recipient_rules_distinct_parties CHECK (sender_account_id <> recipient_account_id)
);
CREATE INDEX transfer_recipient_rules_lookup_idx
    ON transfer_recipient_rules (sender_account_id, recipient_account_id, effective_at DESC, id DESC);
CREATE TRIGGER transfer_recipient_rules_immutable BEFORE UPDATE OR DELETE ON transfer_recipient_rules
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER transfer_recipient_rules_owner BEFORE INSERT ON transfer_recipient_rules
    FOR EACH ROW EXECUTE FUNCTION cp_transfers_require_owner('sender_account_id', 'set_by_user_id');

CREATE TABLE transfer_sender_modes (
    id               uuid PRIMARY KEY,
    account_id       uuid NOT NULL REFERENCES accounts(id),
    -- The latest row is the mode in force; no row means OPEN.
    mode             text NOT NULL CHECK (mode IN ('OPEN','ALLOWLIST_ONLY')),
    set_by_user_id   uuid NOT NULL REFERENCES users(id),
    effective_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX transfer_sender_modes_lookup_idx ON transfer_sender_modes (account_id, effective_at DESC, id DESC);
CREATE TRIGGER transfer_sender_modes_immutable BEFORE UPDATE OR DELETE ON transfer_sender_modes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER transfer_sender_modes_owner BEFORE INSERT ON transfer_sender_modes
    FOR EACH ROW EXECUTE FUNCTION cp_transfers_require_owner('account_id', 'set_by_user_id');

-- ---------------------------------------------------------------------------
-- 3. The transfer register
-- ---------------------------------------------------------------------------

CREATE TABLE internal_transfers (
    id                               uuid PRIMARY KEY,
    intent_id                        uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    sender_account_id                uuid NOT NULL REFERENCES accounts(id),
    sender_user_id                   uuid NOT NULL REFERENCES users(id),
    recipient_account_id             uuid NOT NULL REFERENCES accounts(id),
    -- The handle the sender addressed, when it was a handle. Display and evidence only.
    recipient_handle                 text,
    asset_id                         uuid NOT NULL REFERENCES assets(id),
    quantity                         numeric(38,0) NOT NULL CHECK (quantity > 0),
    -- The USD value the limits were applied to (rounded up), never a balance.
    notional_usd_minor               bigint NOT NULL CHECK (notional_usd_minor >= 0),
    mode                             text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    -- Stored here and nowhere else: never in a journal description, metadata or content hash.
    memo                             text CHECK (memo IS NULL OR char_length(memo) BETWEEN 1 AND 280),
    idempotency_key                  text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    request_hash                     bytea NOT NULL CHECK (length(request_hash) = 32),
    policy_version                   text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    policy_hash                      text NOT NULL CHECK (length(policy_hash) = 64),
    status                           text NOT NULL CHECK (status IN ('PENDING','COMPLETED','REJECTED','REVERSED')),
    -- Decision columns: written once, by the update that leaves PENDING.
    rejection_code                   text CHECK (rejection_code IS NULL OR rejection_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    reason_codes                     text[] NOT NULL DEFAULT '{}',
    risk_decision_id                 uuid REFERENCES advanced_risk_decisions(id),
    effect_id                        uuid UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id           uuid UNIQUE REFERENCES journal_transactions(id),
    reservation_id                   uuid,
    evidence_ref                     text,
    evidence_sha256                  bytea CHECK (evidence_sha256 IS NULL OR length(evidence_sha256) = 32),
    decided_at                       timestamptz,
    -- Reversal columns: written once, by COMPLETED -> REVERSED.
    reversal_journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    reversal_reason_code             text CHECK (reversal_reason_code IS NULL OR reversal_reason_code ~ '^[A-Z][A-Z0-9_]{2,63}$'),
    reversed_by_actor_id             text,
    reversal_evidence_ref            text,
    reversed_at                      timestamptz,
    correlation_id                   text,
    created_at                       timestamptz NOT NULL,
    updated_at                       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (sender_account_id, idempotency_key),
    CONSTRAINT internal_transfers_distinct_parties CHECK (sender_account_id <> recipient_account_id),
    CONSTRAINT internal_transfers_decided_at CHECK ((status = 'PENDING') = (decided_at IS NULL)),
    CONSTRAINT internal_transfers_rejection_has_code CHECK (
        (status = 'REJECTED') = (rejection_code IS NOT NULL) AND (status <> 'REJECTED' OR cardinality(reason_codes) > 0)),
    -- Written as comparisons rather than IN lists: these are rules about two statuses, not a second
    -- copy of the status enum, and the enum agreement suite reads every IN list as an enum.
    CONSTRAINT internal_transfers_settled_has_journal CHECK (
        (status <> 'COMPLETED' AND status <> 'REVERSED')
        OR (journal_transaction_id IS NOT NULL AND effect_id IS NOT NULL AND risk_decision_id IS NOT NULL AND evidence_ref IS NOT NULL)),
    CONSTRAINT internal_transfers_unsettled_has_no_journal CHECK (
        status = 'COMPLETED' OR status = 'REVERSED' OR (journal_transaction_id IS NULL AND effect_id IS NULL)),
    CONSTRAINT internal_transfers_reversal_complete CHECK (
        (status = 'REVERSED') = (reversal_journal_transaction_id IS NOT NULL)
        AND (status <> 'REVERSED' OR (reversal_reason_code IS NOT NULL AND reversed_by_actor_id IS NOT NULL
                                      AND reversal_evidence_ref IS NOT NULL AND reversed_at IS NOT NULL)))
);
CREATE INDEX internal_transfers_sender_idx ON internal_transfers (sender_account_id, created_at DESC, id DESC);
CREATE INDEX internal_transfers_recipient_idx ON internal_transfers (recipient_account_id, created_at DESC, id DESC);
CREATE INDEX internal_transfers_usage_idx ON internal_transfers (sender_account_id, created_at)
    WHERE status IN ('COMPLETED','REVERSED');
CREATE TRIGGER internal_transfers_updated_at BEFORE UPDATE ON internal_transfers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER internal_transfers_owner BEFORE INSERT ON internal_transfers
    FOR EACH ROW EXECUTE FUNCTION cp_transfers_require_owner('sender_account_id', 'sender_user_id');

-- +goose StatementBegin
CREATE FUNCTION cp_internal_transfer_born_pending() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM 'PENDING'
       OR NEW.rejection_code IS NOT NULL OR cardinality(NEW.reason_codes) <> 0 OR NEW.risk_decision_id IS NOT NULL
       OR NEW.effect_id IS NOT NULL OR NEW.journal_transaction_id IS NOT NULL OR NEW.reservation_id IS NOT NULL
       OR NEW.evidence_ref IS NOT NULL OR NEW.evidence_sha256 IS NOT NULL OR NEW.decided_at IS NOT NULL
       OR NEW.reversal_journal_transaction_id IS NOT NULL OR NEW.reversal_reason_code IS NOT NULL
       OR NEW.reversed_by_actor_id IS NOT NULL OR NEW.reversal_evidence_ref IS NOT NULL OR NEW.reversed_at IS NOT NULL THEN
        RAISE EXCEPTION 'TRANSFER_NOT_BORN_PENDING: transfer % must be created PENDING with no outcome', NEW.id
            USING ERRCODE = 'PT003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_internal_transfer_born_pending() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER internal_transfers_born_pending BEFORE INSERT ON internal_transfers
    FOR EACH ROW EXECUTE FUNCTION cp_internal_transfer_born_pending();

-- internal/transfers CanTransition mirrors this table; a parity test compares the two.
-- +goose StatementBegin
CREATE FUNCTION cp_internal_transfer_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('PENDING','COMPLETED'), ('PENDING','REJECTED'), ('COMPLETED','REVERSED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_internal_transfer_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.intent_id, NEW.sender_account_id, NEW.sender_user_id, NEW.recipient_account_id, NEW.recipient_handle,
        NEW.asset_id, NEW.quantity, NEW.notional_usd_minor, NEW.mode, NEW.memo, NEW.idempotency_key, NEW.request_hash,
        NEW.policy_version, NEW.policy_hash, NEW.correlation_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.intent_id, OLD.sender_account_id, OLD.sender_user_id, OLD.recipient_account_id, OLD.recipient_handle,
        OLD.asset_id, OLD.quantity, OLD.notional_usd_minor, OLD.mode, OLD.memo, OLD.idempotency_key, OLD.request_hash,
        OLD.policy_version, OLD.policy_hash, OLD.correlation_id, OLD.created_at) THEN
        RAISE EXCEPTION 'TRANSFER_IDENTITY_FROZEN: transfer % may change status only', OLD.id USING ERRCODE = 'PT002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_internal_transfer_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'TRANSFER_TRANSITION_ILLEGAL: transfer % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'PT004';
    END IF;
    -- The decision is written by the one update that leaves PENDING, and never again.
    IF NOT (OLD.status = 'PENDING' AND NEW.status IS DISTINCT FROM 'PENDING')
       AND (NEW.rejection_code, NEW.reason_codes, NEW.risk_decision_id, NEW.effect_id, NEW.journal_transaction_id,
            NEW.reservation_id, NEW.evidence_ref, NEW.evidence_sha256, NEW.decided_at)
           IS DISTINCT FROM
           (OLD.rejection_code, OLD.reason_codes, OLD.risk_decision_id, OLD.effect_id, OLD.journal_transaction_id,
            OLD.reservation_id, OLD.evidence_ref, OLD.evidence_sha256, OLD.decided_at) THEN
        RAISE EXCEPTION 'TRANSFER_DECISION_FROZEN: the decision of transfer % is written once, when it leaves PENDING', OLD.id
            USING ERRCODE = 'PT002';
    END IF;
    -- The reversal is written by COMPLETED -> REVERSED, and never again.
    IF NOT (OLD.status = 'COMPLETED' AND NEW.status = 'REVERSED')
       AND (NEW.reversal_journal_transaction_id, NEW.reversal_reason_code, NEW.reversed_by_actor_id,
            NEW.reversal_evidence_ref, NEW.reversed_at)
           IS DISTINCT FROM
           (OLD.reversal_journal_transaction_id, OLD.reversal_reason_code, OLD.reversed_by_actor_id,
            OLD.reversal_evidence_ref, OLD.reversed_at) THEN
        RAISE EXCEPTION 'TRANSFER_REVERSAL_FROZEN: the reversal of transfer % is written once, by COMPLETED -> REVERSED', OLD.id
            USING ERRCODE = 'PT002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_internal_transfer_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER internal_transfers_guard BEFORE UPDATE ON internal_transfers
    FOR EACH ROW EXECUTE FUNCTION cp_internal_transfer_guard();

-- A settled transfer and the ledger say the same thing. Deferred to COMMIT: the journal, the effect and
-- the transfer are written by different statements of one transaction, in whatever order.
-- +goose StatementBegin
CREATE FUNCTION cp_internal_transfer_outcome_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    j       record;
    legs    record;
    eff     record;
BEGIN
    IF NEW.status = 'COMPLETED' THEN
        SELECT kind, reference_type, reference_id, reversal_of INTO j
          FROM journal_transactions WHERE id = NEW.journal_transaction_id;
        IF j.kind IS DISTINCT FROM 'P2P_TRANSFER' OR j.reference_type IS DISTINCT FROM 'p2p_transfer'
           OR j.reference_id IS DISTINCT FROM NEW.id::text OR j.reversal_of IS NOT NULL THEN
            RAISE EXCEPTION 'TRANSFER_JOURNAL_MISMATCH: transfer % names journal % which is not its P2P_TRANSFER',
                NEW.id, NEW.journal_transaction_id USING ERRCODE = 'PT005';
        END IF;
        SELECT count(*) AS n,
               count(*) FILTER (WHERE la.owner_id = NEW.sender_account_id AND la.code = 'TRANSFER_OUT' AND e.side = 'DEBIT') AS a,
               count(*) FILTER (WHERE la.owner_id = NEW.sender_account_id AND la.code = 'WALLET' AND e.side = 'CREDIT') AS b,
               count(*) FILTER (WHERE la.owner_id = NEW.recipient_account_id AND la.code = 'WALLET' AND e.side = 'DEBIT') AS c,
               count(*) FILTER (WHERE la.owner_id = NEW.recipient_account_id AND la.code = 'TRANSFER_IN' AND e.side = 'CREDIT') AS d,
               coalesce(bool_and(la.owner_type = 'CUSTOMER' AND e.asset_id = NEW.asset_id AND e.quantity = NEW.quantity), false) AS exact
          INTO legs
          FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.journal_transaction_id;
        IF legs.n <> 4 OR legs.a <> 1 OR legs.b <> 1 OR legs.c <> 1 OR legs.d <> 1 OR NOT legs.exact THEN
            RAISE EXCEPTION 'TRANSFER_JOURNAL_MISMATCH: journal % does not move exactly % of asset % from % to %',
                NEW.journal_transaction_id, NEW.quantity, NEW.asset_id, NEW.sender_account_id, NEW.recipient_account_id
                USING ERRCODE = 'PT005';
        END IF;
        SELECT status, subsystem, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
        IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'TRANSFERS'
           OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
            RAISE EXCEPTION 'TRANSFER_EFFECT_MISMATCH: transfer % is COMPLETED but effect % did not apply journal %',
                NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'PT005';
        END IF;
    ELSIF NEW.status = 'REVERSED' THEN
        SELECT kind, reference_type, reference_id, reversal_of INTO j
          FROM journal_transactions WHERE id = NEW.reversal_journal_transaction_id;
        IF j.kind IS DISTINCT FROM 'COMPENSATION' OR j.reversal_of IS DISTINCT FROM NEW.journal_transaction_id
           OR j.reference_type IS DISTINCT FROM 'p2p_transfer_reversal' OR j.reference_id IS DISTINCT FROM NEW.id::text THEN
            RAISE EXCEPTION 'TRANSFER_JOURNAL_MISMATCH: transfer % names reversal % which does not compensate journal %',
                NEW.id, NEW.reversal_journal_transaction_id, NEW.journal_transaction_id USING ERRCODE = 'PT005';
        END IF;
        SELECT count(*) AS n,
               count(*) FILTER (WHERE la.owner_id = NEW.sender_account_id AND la.code = 'WALLET' AND e.side = 'DEBIT') AS a,
               count(*) FILTER (WHERE la.owner_id = NEW.sender_account_id AND la.code = 'TRANSFER_OUT' AND e.side = 'CREDIT') AS b,
               count(*) FILTER (WHERE la.owner_id = NEW.recipient_account_id AND la.code = 'TRANSFER_IN' AND e.side = 'DEBIT') AS c,
               count(*) FILTER (WHERE la.owner_id = NEW.recipient_account_id AND la.code = 'WALLET' AND e.side = 'CREDIT') AS d,
               coalesce(bool_and(la.owner_type = 'CUSTOMER' AND e.asset_id = NEW.asset_id AND e.quantity = NEW.quantity), false) AS exact
          INTO legs
          FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.reversal_journal_transaction_id;
        IF legs.n <> 4 OR legs.a <> 1 OR legs.b <> 1 OR legs.c <> 1 OR legs.d <> 1 OR NOT legs.exact THEN
            RAISE EXCEPTION 'TRANSFER_JOURNAL_MISMATCH: reversal % does not return exactly % of asset % from % to %',
                NEW.reversal_journal_transaction_id, NEW.quantity, NEW.asset_id, NEW.recipient_account_id, NEW.sender_account_id
                USING ERRCODE = 'PT005';
        END IF;
        SELECT status, compensation_journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
        IF eff.status IS DISTINCT FROM 'COMPENSATED'
           OR eff.compensation_journal_transaction_id IS DISTINCT FROM NEW.reversal_journal_transaction_id THEN
            RAISE EXCEPTION 'TRANSFER_EFFECT_MISMATCH: transfer % is REVERSED but effect % was not compensated by journal %',
                NEW.id, NEW.effect_id, NEW.reversal_journal_transaction_id USING ERRCODE = 'PT005';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_internal_transfer_outcome_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER internal_transfers_outcome_matches AFTER UPDATE OF status ON internal_transfers
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.status IN ('COMPLETED','REVERSED'))
    EXECUTE FUNCTION cp_internal_transfer_outcome_matches();

CREATE TABLE internal_transfer_transitions (
    id              uuid PRIMARY KEY,
    transfer_id     uuid NOT NULL REFERENCES internal_transfers(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX internal_transfer_transitions_idx ON internal_transfer_transitions (transfer_id, occurred_at);
CREATE TRIGGER internal_transfer_transitions_immutable BEFORE UPDATE OR DELETE ON internal_transfer_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER internal_transfer_transitions_flag_edge AFTER INSERT ON internal_transfer_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('transfer_id', 'from_status', 'to_status', 'internal_transfer_status');
CREATE CONSTRAINT TRIGGER internal_transfers_require_transition AFTER UPDATE OF status ON internal_transfers
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'internal_transfer_status');

-- ---------------------------------------------------------------------------
-- 4. Sandbox faucet grants
-- ---------------------------------------------------------------------------

CREATE TABLE sandbox_fundings (
    id                      uuid PRIMARY KEY,
    account_id              uuid NOT NULL REFERENCES accounts(id),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 200),
    request_hash            bytea NOT NULL CHECK (length(request_hash) = 32),
    policy_version          text NOT NULL CHECK (length(policy_version) BETWEEN 1 AND 100),
    policy_hash             text NOT NULL CHECK (length(policy_hash) = 64),
    environment             text NOT NULL CHECK (environment IN ('LOCAL','TEST','DEV','STAGING','PROD')),
    actor_type              text NOT NULL CHECK (actor_type IN ('USER','SERVICE','SYSTEM')),
    actor_id                text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    evidence_ref            text NOT NULL,
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    correlation_id          text,
    granted_at              timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, idempotency_key)
);
CREATE INDEX sandbox_fundings_window_idx ON sandbox_fundings (account_id, asset_id, granted_at DESC);
CREATE TRIGGER sandbox_fundings_immutable BEFORE UPDATE OR DELETE ON sandbox_fundings
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_sandbox_funding_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    akind   text;
    j       record;
    legs    record;
    eff     record;
BEGIN
    SELECT kind INTO akind FROM assets WHERE id = NEW.asset_id;
    IF akind IS DISTINCT FROM 'SIMULATED' THEN
        RAISE EXCEPTION 'SANDBOX_FUNDING_NOT_SIMULATED: the faucet grants only SIMULATED assets; asset % is %',
            NEW.asset_id, akind USING ERRCODE = 'SF001';
    END IF;
    SELECT kind, reference_type, reference_id INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM 'SANDBOX_FUNDING' OR j.reference_type IS DISTINCT FROM 'sandbox_funding'
       OR j.reference_id IS DISTINCT FROM NEW.id::text THEN
        RAISE EXCEPTION 'SANDBOX_FUNDING_JOURNAL_MISMATCH: grant % names journal % which is not its SANDBOX_FUNDING',
            NEW.id, NEW.journal_transaction_id USING ERRCODE = 'SF002';
    END IF;
    SELECT count(*) AS n,
           count(*) FILTER (WHERE la.code = 'WALLET' AND e.side = 'DEBIT') AS a,
           count(*) FILTER (WHERE la.code = 'CAPITAL' AND e.side = 'CREDIT') AS b,
           coalesce(bool_and(la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id
                             AND e.asset_id = NEW.asset_id AND e.quantity = NEW.quantity), false) AS exact
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF legs.n <> 2 OR legs.a <> 1 OR legs.b <> 1 OR NOT legs.exact THEN
        RAISE EXCEPTION 'SANDBOX_FUNDING_JOURNAL_MISMATCH: journal % does not credit exactly % of asset % to account %',
            NEW.journal_transaction_id, NEW.quantity, NEW.asset_id, NEW.account_id USING ERRCODE = 'SF002';
    END IF;
    SELECT status, subsystem, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'SANDBOX_FUNDING'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'SANDBOX_FUNDING_EFFECT_MISMATCH: grant % names effect % which did not apply journal %',
            NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'SF002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_sandbox_funding_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER sandbox_fundings_match BEFORE INSERT ON sandbox_fundings
    FOR EACH ROW EXECUTE FUNCTION cp_sandbox_funding_matches();

-- ---------------------------------------------------------------------------
-- 5. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON transfer_handles, transfer_recipient_rules, transfer_sender_modes,
                        internal_transfers, internal_transfer_transitions, sandbox_fundings TO cp_app;
GRANT UPDATE (status, rejection_code, reason_codes, risk_decision_id, effect_id, journal_transaction_id, reservation_id,
              evidence_ref, evidence_sha256, decided_at, reversal_journal_transaction_id, reversal_reason_code,
              reversed_by_actor_id, reversal_evidence_ref, reversed_at) ON internal_transfers TO cp_app;
GRANT SELECT ON transfer_handles, transfer_recipient_rules, transfer_sender_modes, internal_transfers,
                internal_transfer_transitions, sandbox_fundings TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: transfer and faucet history is financial history and is never dropped
