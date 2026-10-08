-- +goose Up
-- The foundation of the advanced financial capabilities (ADR-0020,
-- docs/architecture/ADVANCED_FINANCIAL_MODEL.md).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A SIMULATED asset kind exists, and it is always value domain SIMULATED. Migration 00710 already
--      makes SIMULATED structurally unable to move to or from any other domain (VD002).
--   2. Two new ledger owner types, HOUSE (the principal / treasury book) and FACILITY (platform-operated
--      economic facilities: stable issuer, lending pool, insurance fund, clearing house, bridge,
--      prediction market). Their ledger accounts can exist ONLY in value domain SIMULATED. In this
--      schema version the house book and every facility are incapable of holding real value.
--   3. The chart of accounts grows by 47 codes. Owner type, normal side, negative-balance permission
--      and (for FACILITY codes) the facility kind are stated per code in SQL and compared with
--      internal/ledger by parity tests, so the copies cannot drift unnoticed.
--   4. 31 new journal kinds, every one SIMULATED-ONLY: a transaction of such a kind may touch only
--      SIMULATED accounts (SM001), may never declare a value-domain conversion, and must balance per
--      party -- for every (owner_type, owner_id, asset) -- not only per asset (SM002). Value moving
--      between parties is therefore always an explicit pair, such as TRANSFER_OUT / TRANSFER_IN.
--   5. 15 new capability gates, all high risk.
--   6. economic_intents: the typed intent family. A row can never be in mode LIVE: the CHECK lists only
--      PAPER, SANDBOX and DEVNET. Live advanced activity is not disabled here, it is unrepresentable.
--   7. economic_effects: the exactly-once register every advanced mutation goes through. One row per
--      economic effect key (UNIQUE), immutable identity, a legal status machine enforced in SQL, every
--      status change bound to a transition row that names both endpoints (00726), and APPLIED is
--      impossible for a ledger-posting effect without the journal transaction it applied.
--   8. advanced_risk_decisions: append-only record of every Risk Kernel V2 decision.
--   9. Six new reconciliation kinds for the facilities whose books must agree with their own registers.
--
-- Custom SQLSTATEs: SM001 non-simulated account in a simulated-only transaction, SM002 per-party
-- imbalance, FA001 facility/house account without a matching row or of the wrong kind, EF001 illegal
-- effect transition, EF002 intent or effect identity changed. (An APPLIED ledger-posting effect without
-- its journal is refused by the CHECK economic_effects_applied_has_journal, SQLSTATE 23514.)

-- ---------------------------------------------------------------------------
-- 1. SIMULATED assets
-- ---------------------------------------------------------------------------

ALTER TABLE assets DROP CONSTRAINT assets_kind_check;
ALTER TABLE assets ADD CONSTRAINT assets_kind_check CHECK (
    kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022','FIAT','CREDIT','NATIVE_ASSET','SIMULATED'));

ALTER TABLE assets DROP CONSTRAINT assets_kind_domain_agree;
ALTER TABLE assets ADD CONSTRAINT assets_kind_domain_agree CHECK (
    (kind = 'CREDIT'       AND value_domain = 'INTERNAL_CREDIT')
    OR (kind = 'NATIVE_ASSET' AND value_domain = 'INTERNAL_NATIVE_ASSET')
    OR (kind = 'SIMULATED'    AND value_domain = 'SIMULATED')
    OR (kind = 'FIAT'         AND value_domain IS NULL)
    OR (kind IN ('NATIVE','SPL_TOKEN','SPL_TOKEN_2022')
        AND value_domain IN ('SELF_CUSTODIAL_CRYPTO','HOSTED_CRYPTO','SIMULATED')));

-- A SIMULATED asset lives on the sandbox chain or on a simulated public chain (internal/devnet), and
-- nothing else may. Stated as an equivalence so neither side can be true alone.
ALTER TABLE assets ADD CONSTRAINT assets_simulated_chain_check CHECK (
    (kind = 'SIMULATED') = (chain = 'nodal-sandbox' OR chain ~ '^sbx-[a-z0-9-]+$'));
-- On the sandbox chain there is no mint; the asset's own id stands in, exactly as for internal assets.
ALTER TABLE assets ADD CONSTRAINT assets_sandbox_mint_is_own_id CHECK (
    chain <> 'nodal-sandbox' OR mint_address = id::text);

-- ---------------------------------------------------------------------------
-- 2. Facilities and house books
-- ---------------------------------------------------------------------------

CREATE TABLE facilities (
    id                     uuid PRIMARY KEY,
    kind                   text NOT NULL CHECK (kind IN ('STABLE_ISSUER','LENDING_POOL','INSURANCE_FUND',
                                                         'CLEARING_HOUSE','BRIDGE','PREDICTION_MARKET')),
    name                   text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    -- Not a choice. A facility holds simulated value in this schema version, and the column exists so
    -- the fact is visible on the row rather than implied by a constraint somewhere else.
    value_domain           text NOT NULL DEFAULT 'SIMULATED' CHECK (value_domain = 'SIMULATED'),
    status                 text NOT NULL CHECK (status IN ('ACTIVE','SUSPENDED','CLOSED')),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (kind, name)
);
CREATE TRIGGER facilities_updated_at BEFORE UPDATE ON facilities FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE house_accounts (
    id                     uuid PRIMARY KEY,
    name                   text NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 200),
    strategy               text NOT NULL CHECK (strategy IN ('LIQUIDITY_PROVISION','MARKET_MAKING','HEDGING',
                                                             'INVENTORY_REBALANCING','TREASURY_CONVERSION')),
    value_domain           text NOT NULL DEFAULT 'SIMULATED' CHECK (value_domain = 'SIMULATED'),
    status                 text NOT NULL CHECK (status IN ('ACTIVE','KILLED','CLOSED')),
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER house_accounts_updated_at BEFORE UPDATE ON house_accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 3. The chart of accounts, in SQL, generated from internal/ledger
-- ---------------------------------------------------------------------------

ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_owner_type_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_owner_type_check CHECK (
    owner_type IN ('CUSTOMER','PLATFORM','HOUSE','FACILITY'));

ALTER TABLE ledger_accounts DROP CONSTRAINT ledger_accounts_code_check;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_code_check CHECK (code IN (
    'WALLET','CAPITAL','TRADING_OUTFLOW','TRADING_INFLOW','FEES_NETWORK','FEES_VENUE',
    'FEES_PLATFORM','DEFICIT','RECONCILIATION_ADJUSTMENT','PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT',
    'CREDIT_BALANCE','CREDIT_ISSUANCE','NATIVE_ASSET_BALANCE','PAYOUT_RESERVED','MARKET_RESERVE','MARKET_INVENTORY',
    'PAYOUT_CLEARING','PAYOUT_SETTLED','TRANSFER_OUT','TRANSFER_IN','FEES_SPREAD','FEES_PERFORMANCE',
    'MARGIN_COLLATERAL','MARGIN_LOAN','INTEREST_EXPENSE','DERIVATIVES_MARGIN','REALIZED_PNL','OPTION_PREMIUM_PAID',
    'OPTION_PREMIUM_RECEIVED','UNSETTLED_RECEIVABLE','BRIDGE_IN_FLIGHT','CORPORATE_ACTIONS','DIVIDENDS','WITHDRAWN',
    'SPREAD_REVENUE','PERFORMANCE_FEE_ACCRUED','PERFORMANCE_FEE_DEFERRED','PERFORMANCE_FEE_REVENUE','HOUSE_INVENTORY','HOUSE_CAPITAL',
    'HOUSE_TRADING_OUTFLOW','HOUSE_TRADING_INFLOW','HOUSE_FEES','STABLE_RESERVE','STABLE_RESERVE_FUNDING','STABLE_ISSUED',
    'STABLE_OUTSTANDING','POOL_CASH','POOL_CAPITAL','LOAN_RECEIVABLE','INTEREST_INCOME','INSURANCE_FUND_CASH',
    'INSURANCE_FUND_CAPITAL','DEFICIT_RECEIVABLE','CLEARING_CASH','CLEARING_CAPITAL','CLEARING_RESULT','BRIDGE_LOCKED',
    'BRIDGE_LOCK_FUNDING','BRIDGE_MINTED','BRIDGE_OUTSTANDING','PREDICTION_ESCROW','PREDICTION_COLLATERAL','PREDICTION_SHARES_ISSUED',
    'PREDICTION_SHARES_OUTSTANDING'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_ledger_code_allows_negative(p_code text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_code IN ('RECONCILIATION_ADJUSTMENT','PLATFORM_ADJUSTMENT','REALIZED_PNL','CORPORATE_ACTIONS','CLEARING_RESULT');
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_ledger_code_normal_side(p_code text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE WHEN p_code IN (
        'WALLET','TRADING_OUTFLOW','FEES_NETWORK','FEES_VENUE','FEES_PLATFORM','PLATFORM_FEE_RECEIVABLE',
        'CREDIT_BALANCE','NATIVE_ASSET_BALANCE','PAYOUT_RESERVED','MARKET_RESERVE','MARKET_INVENTORY','PAYOUT_SETTLED',
        'TRANSFER_OUT','FEES_SPREAD','FEES_PERFORMANCE','MARGIN_COLLATERAL','INTEREST_EXPENSE','DERIVATIVES_MARGIN',
        'OPTION_PREMIUM_PAID','UNSETTLED_RECEIVABLE','BRIDGE_IN_FLIGHT','WITHDRAWN','PERFORMANCE_FEE_ACCRUED','HOUSE_INVENTORY',
        'HOUSE_TRADING_OUTFLOW','HOUSE_FEES','STABLE_RESERVE','STABLE_ISSUED','POOL_CASH','LOAN_RECEIVABLE',
        'INSURANCE_FUND_CASH','DEFICIT_RECEIVABLE','CLEARING_CASH','BRIDGE_LOCKED','BRIDGE_MINTED','PREDICTION_ESCROW',
        'PREDICTION_SHARES_ISSUED')
        THEN 'DEBIT' ELSE 'CREDIT' END;
$$;
-- +goose StatementEnd

-- Which owner type a code belongs to. Until now only Go knew; a PLATFORM row carrying WALLET was a Go
-- refusal and a SQL shrug. With four owner types and segregated books the pairing is a control.
-- +goose StatementBegin
CREATE FUNCTION cp_ledger_code_owner_type(p_code text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN p_code IN ('PLATFORM_FEE_RECEIVABLE','PLATFORM_FEE_REVENUE','PLATFORM_ADJUSTMENT','MARKET_RESERVE',
                        'MARKET_INVENTORY','PAYOUT_CLEARING','PAYOUT_SETTLED','SPREAD_REVENUE',
                        'PERFORMANCE_FEE_ACCRUED','PERFORMANCE_FEE_DEFERRED','PERFORMANCE_FEE_REVENUE') THEN 'PLATFORM'
        WHEN p_code IN ('HOUSE_INVENTORY','HOUSE_CAPITAL','HOUSE_TRADING_OUTFLOW','HOUSE_TRADING_INFLOW','HOUSE_FEES') THEN 'HOUSE'
        WHEN p_code IN ('STABLE_RESERVE','STABLE_RESERVE_FUNDING','STABLE_ISSUED','STABLE_OUTSTANDING','POOL_CASH','POOL_CAPITAL',
                        'LOAN_RECEIVABLE','INTEREST_INCOME','INSURANCE_FUND_CASH','INSURANCE_FUND_CAPITAL','DEFICIT_RECEIVABLE',
                        'CLEARING_CASH','CLEARING_CAPITAL','CLEARING_RESULT','BRIDGE_LOCKED','BRIDGE_LOCK_FUNDING',
                        'BRIDGE_MINTED','BRIDGE_OUTSTANDING','PREDICTION_ESCROW','PREDICTION_COLLATERAL',
                        'PREDICTION_SHARES_ISSUED','PREDICTION_SHARES_OUTSTANDING') THEN 'FACILITY'
        ELSE 'CUSTOMER' END;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_ledger_code_facility_kind(p_code text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE
        WHEN p_code IN ('STABLE_RESERVE','STABLE_RESERVE_FUNDING','STABLE_ISSUED','STABLE_OUTSTANDING') THEN 'STABLE_ISSUER'
        WHEN p_code IN ('POOL_CASH','POOL_CAPITAL','LOAN_RECEIVABLE','INTEREST_INCOME') THEN 'LENDING_POOL'
        WHEN p_code IN ('INSURANCE_FUND_CASH','INSURANCE_FUND_CAPITAL','DEFICIT_RECEIVABLE') THEN 'INSURANCE_FUND'
        WHEN p_code IN ('CLEARING_CASH','CLEARING_CAPITAL','CLEARING_RESULT') THEN 'CLEARING_HOUSE'
        WHEN p_code IN ('BRIDGE_LOCKED','BRIDGE_LOCK_FUNDING','BRIDGE_MINTED','BRIDGE_OUTSTANDING') THEN 'BRIDGE'
        WHEN p_code IN ('PREDICTION_ESCROW','PREDICTION_COLLATERAL','PREDICTION_SHARES_ISSUED',
                        'PREDICTION_SHARES_OUTSTANDING') THEN 'PREDICTION_MARKET'
        ELSE NULL END;
$$;
-- +goose StatementEnd

ALTER TABLE ledger_accounts
    ADD CONSTRAINT ledger_accounts_owner_matches_chart
    CHECK (owner_type = cp_ledger_code_owner_type(code));

-- The house book and every facility hold simulated value only.
ALTER TABLE ledger_accounts
    ADD CONSTRAINT ledger_accounts_segregated_books_are_simulated
    CHECK (owner_type IN ('CUSTOMER','PLATFORM') OR value_domain = 'SIMULATED');

-- A HOUSE account must name a house book; a FACILITY account must name a facility of the kind its
-- code belongs to. Checked when the account is created, which is the only time owner and code are
-- written (00604 restricts cp_app's UPDATE to status).
-- +goose StatementBegin
CREATE FUNCTION cp_ledger_account_owner_exists() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    fkind text;
BEGIN
    IF NEW.owner_type = 'HOUSE' THEN
        IF NOT EXISTS (SELECT 1 FROM house_accounts WHERE id = NEW.owner_id) THEN
            RAISE EXCEPTION 'SEGREGATED_BOOK_UNKNOWN: HOUSE ledger account names house book % which does not exist', NEW.owner_id
                USING ERRCODE = 'FA001';
        END IF;
    ELSIF NEW.owner_type = 'FACILITY' THEN
        SELECT kind INTO fkind FROM facilities WHERE id = NEW.owner_id;
        IF fkind IS NULL THEN
            RAISE EXCEPTION 'SEGREGATED_BOOK_UNKNOWN: FACILITY ledger account names facility % which does not exist', NEW.owner_id
                USING ERRCODE = 'FA001';
        END IF;
        IF fkind IS DISTINCT FROM cp_ledger_code_facility_kind(NEW.code) THEN
            RAISE EXCEPTION 'SEGREGATED_BOOK_KIND: code % belongs to % facilities and facility % is %',
                NEW.code, cp_ledger_code_facility_kind(NEW.code), NEW.owner_id, fkind
                USING ERRCODE = 'FA001';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_ledger_account_owner_exists() SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER ledger_accounts_owner_exists BEFORE INSERT ON ledger_accounts
    FOR EACH ROW EXECUTE FUNCTION cp_ledger_account_owner_exists();

-- ---------------------------------------------------------------------------
-- 4. Journal kinds and the simulated-only rule
-- ---------------------------------------------------------------------------

ALTER TABLE journal_transactions DROP CONSTRAINT journal_transactions_kind_check;
ALTER TABLE journal_transactions ADD CONSTRAINT journal_transactions_kind_check CHECK (kind IN (
    'FUNDING_SETTLED','FUNDING_REVERSAL','FUNDING_REVERSAL_DEFICIT','TRADE_FILL','FEE','WITHDRAWAL_SETTLED',
    'COMPENSATION','CORRECTION','RECONCILIATION_ADJUSTMENT','SEED','CREDIT_ISSUED','CREDIT_REVERSED',
    'CREDIT_SPENT','NATIVE_TRADE','INTERNAL_PURCHASE','CREATOR_EARNING','PAYOUT_RESERVED','PAYOUT_SETTLED',
    'PAYOUT_RETURNED','SANDBOX_FUNDING','FACILITY_FUNDING','HOUSE_FUNDING','P2P_TRANSFER','SIM_TRADE',
    'EQUITY_SETTLEMENT','CORPORATE_ACTION','MARGIN_COLLATERAL_MOVE','MARGIN_BORROW','MARGIN_REPAY','MARGIN_INTEREST',
    'LIQUIDATION','DEFICIT_COVERAGE','DERIVATIVE_MARGIN_MOVE','VARIATION_MARGIN','FUNDING_PAYMENT','OPTION_PREMIUM',
    'OPTION_SETTLEMENT','FUTURE_SETTLEMENT','STABLE_ISSUANCE','STABLE_REDEMPTION','BRIDGE_SOURCE_FINALIZED','BRIDGE_DESTINATION_FINALIZED',
    'BRIDGE_REFUND','PREDICTION_SET_ISSUANCE','PREDICTION_SET_REDEMPTION','PREDICTION_PAYOUT','PREDICTION_REFUND','PERFORMANCE_FEE_ACCRUAL',
    'PERFORMANCE_FEE_CRYSTALLIZATION','SANDBOX_WITHDRAWAL'));

-- +goose StatementBegin
CREATE FUNCTION cp_journal_kind_is_simulated_only(p_kind text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_kind IN (
        'SANDBOX_FUNDING','FACILITY_FUNDING','HOUSE_FUNDING','P2P_TRANSFER','SIM_TRADE','EQUITY_SETTLEMENT',
        'CORPORATE_ACTION','MARGIN_COLLATERAL_MOVE','MARGIN_BORROW','MARGIN_REPAY','MARGIN_INTEREST','LIQUIDATION',
        'DEFICIT_COVERAGE','DERIVATIVE_MARGIN_MOVE','VARIATION_MARGIN','FUNDING_PAYMENT','OPTION_PREMIUM','OPTION_SETTLEMENT',
        'FUTURE_SETTLEMENT','STABLE_ISSUANCE','STABLE_REDEMPTION','BRIDGE_SOURCE_FINALIZED','BRIDGE_DESTINATION_FINALIZED','BRIDGE_REFUND',
        'PREDICTION_SET_ISSUANCE','PREDICTION_SET_REDEMPTION','PREDICTION_PAYOUT','PREDICTION_REFUND','PERFORMANCE_FEE_ACCRUAL','PERFORMANCE_FEE_CRYSTALLIZATION',
        'SANDBOX_WITHDRAWAL');
$$;
-- +goose StatementEnd

ALTER TABLE journal_transactions ADD CONSTRAINT journal_transactions_simulated_only_never_converts CHECK (
    NOT cp_journal_kind_is_simulated_only(kind) OR conversion_from IS NULL);

-- +goose StatementBegin
CREATE FUNCTION cp_check_simulated_only() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    k        text;
    bad      record;
BEGIN
    SELECT kind INTO k FROM journal_transactions WHERE id = NEW.transaction_id;
    IF k IS NULL OR NOT cp_journal_kind_is_simulated_only(k) THEN
        RETURN NULL;
    END IF;

    SELECT la.owner_type, la.owner_id, la.code, la.value_domain INTO bad
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id
       AND la.value_domain <> 'SIMULATED'
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'SIMULATED_ONLY: % transaction % touches %/%/% in value domain %',
            k, NEW.transaction_id, bad.owner_type, bad.owner_id, bad.code, bad.value_domain
            USING ERRCODE = 'SM001';
    END IF;

    SELECT la.owner_type, la.owner_id, e.asset_id,
           sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END) AS net
      INTO bad
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.transaction_id
     GROUP BY la.owner_type, la.owner_id, e.asset_id
    HAVING sum(CASE WHEN e.side = 'DEBIT' THEN e.quantity ELSE -e.quantity END) <> 0
     LIMIT 1;
    IF FOUND THEN
        RAISE EXCEPTION 'PER_PARTY_UNBALANCED: % transaction % leaves %/% asset % out of balance by %',
            k, NEW.transaction_id, bad.owner_type, bad.owner_id, bad.asset_id, bad.net
            USING ERRCODE = 'SM002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_check_simulated_only() SET search_path = pg_catalog, public, pg_temp;

CREATE CONSTRAINT TRIGGER journal_entries_simulated_only AFTER INSERT ON journal_entries
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_check_simulated_only();

-- ---------------------------------------------------------------------------
-- 5. Capability gates
-- ---------------------------------------------------------------------------

ALTER TABLE capability_gates DROP CONSTRAINT capability_gates_capability_check;
ALTER TABLE capability_gates ADD CONSTRAINT capability_gates_capability_check CHECK (capability IN (
    'LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS','SOCIAL_DATA_PERSISTENCE','MARKETPLACE',
    'CROSS_CHAIN','PREDICTION_MARKETS','SECURITIES','CEX_TRADING','CREDIT_PURCHASE','NATIVE_ASSET_CREATION',
    'NATIVE_MARKET_TRADING','PAYOUT_RESERVE','PAYOUT_SETTLE','HOSTED_TRADING','HOSTED_FUNDING','AGENT_BOUNDED_DISCRETION',
    'AGENT_AUTONOMOUS_SELECTION','AGENT_AUTONOMOUS_PORTFOLIO','STABLE_ASSET_MINT','MARGIN','LEVERAGE','DERIVATIVES',
    'EQUITIES','INTERNAL_MATCHING','P2P_TRANSFERS','PRINCIPAL_TRADING','SPREAD_ENGINE','PERFORMANCE_FEES',
    'STRATEGY_MARKETPLACE','USER_CODE_SANDBOX','AGENT_SIGNING','AGENT_WITHDRAWALS','CROSS_CHAIN_BUYING_POWER'));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_gate_is_high_risk(p_capability text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT p_capability IN (
        'LIVE_FUNDING','LIVE_MANUAL_TRADING','LIVE_AGENT_TRADING','WITHDRAWALS','MARKETPLACE','CROSS_CHAIN',
        'PREDICTION_MARKETS','SECURITIES','CEX_TRADING','CREDIT_PURCHASE','NATIVE_MARKET_TRADING','PAYOUT_RESERVE',
        'PAYOUT_SETTLE','HOSTED_TRADING','HOSTED_FUNDING','AGENT_BOUNDED_DISCRETION','AGENT_AUTONOMOUS_SELECTION','AGENT_AUTONOMOUS_PORTFOLIO',
        'STABLE_ASSET_MINT','MARGIN','LEVERAGE','DERIVATIVES','EQUITIES','INTERNAL_MATCHING',
        'P2P_TRANSFERS','PRINCIPAL_TRADING','SPREAD_ENGINE','PERFORMANCE_FEES','STRATEGY_MARKETPLACE','USER_CODE_SANDBOX',
        'AGENT_SIGNING','AGENT_WITHDRAWALS','CROSS_CHAIN_BUYING_POWER');
$$;
-- +goose StatementEnd

-- One kill switch kind for every advanced capability: the treasury kill switch, the code-sandbox
-- kill switch, the stable mint/burn pause and every per-capability circuit breaker are
-- CAPABILITY_DISABLE scoped to the capability's name.
ALTER TABLE kill_switches DROP CONSTRAINT kill_switches_kind_check;
ALTER TABLE kill_switches ADD CONSTRAINT kill_switches_kind_check CHECK (kind IN (
    'GLOBAL_NEW_RISK_KILL','ACCOUNT_FREEZE','AGENT_PAUSE','STRATEGY_VERSION_DISABLE','VENUE_DISABLE',
    'INSTRUMENT_CLOSE_ONLY','INSTRUMENT_HALT','CHAIN_DISABLE_NEW_ACTIONS','PROVIDER_DISABLE_NEW_ACTIONS',
    'FUNDING_DISABLE','WITHDRAWALS_DISABLE','MODEL_DISABLE','CAPABILITY_DISABLE'));

-- ---------------------------------------------------------------------------
-- 6. Economic intents
-- ---------------------------------------------------------------------------

CREATE TABLE economic_intents (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES accounts(id),
    kind                text NOT NULL CHECK (kind IN (
                            'ACQUIRE_EXPOSURE','REDUCE_EXPOSURE','TRANSFER_VALUE','BORROW','REPAY',
                            'OPEN_POSITION','CLOSE_POSITION','EXERCISE_OPTION','MINT_STABLE_ASSET','BURN_STABLE_ASSET',
                            'PROVIDE_LIQUIDITY','TAKE_LIQUIDITY','WITHDRAW','BRIDGE_VALUE',
                            'CREATE_PREDICTION_POSITION','RESOLVE_PREDICTION_POSITION')),
    -- LIVE is absent on purpose: an advanced intent that would move real value cannot be written down.
    mode                text NOT NULL CHECK (mode IN ('PAPER','SANDBOX','DEVNET')),
    capability          text NOT NULL CHECK (capability IN (
                            'STABLE_ASSET_MINT','MARGIN','LEVERAGE','DERIVATIVES','EQUITIES','INTERNAL_MATCHING',
                            'P2P_TRANSFERS','PRINCIPAL_TRADING','SPREAD_ENGINE','PERFORMANCE_FEES','STRATEGY_MARKETPLACE',
                            'USER_CODE_SANDBOX','AGENT_SIGNING','AGENT_WITHDRAWALS','CROSS_CHAIN_BUYING_POWER','PREDICTION_MARKETS')),
    actor_type          text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','AGENT','SYSTEM')),
    actor_id            text NOT NULL,
    agent_id            uuid,
    strategy_ref        text,
    delegation_id       uuid,
    instrument_id       uuid REFERENCES instruments(id),
    asset_id            uuid REFERENCES assets(id),
    quantity            numeric(38,0) CHECK (quantity IS NULL OR quantity > 0),
    notional_usd_minor  bigint CHECK (notional_usd_minor IS NULL OR notional_usd_minor >= 0),
    -- Kind-specific typed terms, validated by the Go type that owns the kind and covered by content_hash.
    -- The economic STATE an intent leads to lives in normalized tables; this is the immutable request.
    terms               jsonb NOT NULL DEFAULT '{}'::jsonb,
    content_hash        bytea NOT NULL CHECK (length(content_hash) = 32),
    idempotency_key     text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    status              text NOT NULL CHECK (status IN ('RECEIVED','ROUTED','RISK_APPROVED','RESERVED','EXECUTING',
                                                        'COMPLETED','REJECTED','FAILED','UNKNOWN','RECONCILED','CANCELLED')),
    rejection_code      text,
    correlation_id      text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now(),
    terminal_at         timestamptz,
    UNIQUE (account_id, idempotency_key),
    -- An agent intent names its agent; a human intent names none.
    CHECK ((actor_type = 'AGENT') = (agent_id IS NOT NULL))
);
CREATE INDEX economic_intents_account_idx ON economic_intents (account_id, created_at DESC, id);
CREATE INDEX economic_intents_open_idx ON economic_intents (status) WHERE status NOT IN ('COMPLETED','REJECTED','FAILED','RECONCILED','CANCELLED');
CREATE TRIGGER economic_intents_updated_at BEFORE UPDATE ON economic_intents FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_economic_intent_identity_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.account_id, NEW.kind, NEW.mode, NEW.capability, NEW.actor_type, NEW.actor_id, NEW.agent_id,
        NEW.strategy_ref, NEW.delegation_id, NEW.instrument_id, NEW.asset_id, NEW.quantity, NEW.notional_usd_minor,
        NEW.terms, NEW.content_hash, NEW.idempotency_key, NEW.created_at)
       IS DISTINCT FROM
       (OLD.account_id, OLD.kind, OLD.mode, OLD.capability, OLD.actor_type, OLD.actor_id, OLD.agent_id,
        OLD.strategy_ref, OLD.delegation_id, OLD.instrument_id, OLD.asset_id, OLD.quantity, OLD.notional_usd_minor,
        OLD.terms, OLD.content_hash, OLD.idempotency_key, OLD.created_at) THEN
        RAISE EXCEPTION 'INTENT_IDENTITY_FROZEN: economic intent % may change status only', OLD.id USING ERRCODE = 'EF002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER economic_intents_identity_frozen BEFORE UPDATE ON economic_intents
    FOR EACH ROW EXECUTE FUNCTION cp_economic_intent_identity_frozen();

CREATE TABLE economic_intent_transitions (
    id              uuid PRIMARY KEY,
    intent_id       uuid NOT NULL REFERENCES economic_intents(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX economic_intent_transitions_idx ON economic_intent_transitions (intent_id, occurred_at);
CREATE TRIGGER economic_intent_transitions_immutable BEFORE UPDATE OR DELETE ON economic_intent_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER economic_intent_transitions_flag_edge AFTER INSERT ON economic_intent_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('intent_id', 'from_status', 'to_status', 'econ_intent_status');
CREATE CONSTRAINT TRIGGER economic_intents_require_transition AFTER UPDATE OF status ON economic_intents
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'econ_intent_status');

-- ---------------------------------------------------------------------------
-- 7. Economic effects: the exactly-once register
-- ---------------------------------------------------------------------------

CREATE TABLE economic_effects (
    id                      uuid PRIMARY KEY,
    -- Deterministic per economic effect (e.g. 'p2p:<transfer id>'). The UNIQUE index is the
    -- exactly-once guarantee: a second attempt finds the first.
    effect_key              text NOT NULL UNIQUE CHECK (length(effect_key) BETWEEN 1 AND 300),
    subsystem               text NOT NULL CHECK (subsystem IN (
                                'SANDBOX_FUNDING','STABLE_ASSET','MARGIN','DERIVATIVES','EQUITIES','ORDERS',
                                'TRANSFERS','TREASURY','FEES','MARKETPLACE','SANDBOX_RUNTIME','AGENT_AUTHORITY',
                                'WITHDRAWALS','BRIDGE','PREDICTION_MARKETS')),
    effect_kind             text NOT NULL CHECK (effect_kind ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    intent_id               uuid REFERENCES economic_intents(id),
    account_id              uuid REFERENCES accounts(id),
    -- sha256 of the canonical request. Same key with a different request is a reuse, refused in Go.
    request_hash            bytea NOT NULL CHECK (length(request_hash) = 32),
    posts_ledger            boolean NOT NULL,
    status                  text NOT NULL CHECK (status IN ('PENDING','APPLIED','UNKNOWN','FAILED','COMPENSATED')),
    journal_transaction_id  uuid REFERENCES journal_transactions(id),
    compensation_journal_transaction_id uuid REFERENCES journal_transactions(id),
    reservation_id          uuid,
    external_ref            text,
    evidence_ref            text,
    evidence_sha256         bytea CHECK (evidence_sha256 IS NULL OR length(evidence_sha256) = 32),
    failure_code            text,
    correlation_id          text,
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    -- A ledger-posting effect is not APPLIED until the journal transaction exists and is named.
    CONSTRAINT economic_effects_applied_has_journal CHECK (
        NOT posts_ledger OR status NOT IN ('APPLIED','COMPENSATED') OR journal_transaction_id IS NOT NULL),
    CONSTRAINT economic_effects_compensated_has_journal CHECK (
        status <> 'COMPENSATED' OR NOT posts_ledger OR compensation_journal_transaction_id IS NOT NULL),
    CONSTRAINT economic_effects_failed_has_code CHECK (status <> 'FAILED' OR failure_code IS NOT NULL)
);
CREATE INDEX economic_effects_intent_idx ON economic_effects (intent_id) WHERE intent_id IS NOT NULL;
CREATE INDEX economic_effects_account_idx ON economic_effects (account_id, created_at DESC) WHERE account_id IS NOT NULL;
CREATE INDEX economic_effects_unresolved_idx ON economic_effects (subsystem, status) WHERE status IN ('PENDING','UNKNOWN');
CREATE UNIQUE INDEX economic_effects_journal_once ON economic_effects (journal_transaction_id) WHERE journal_transaction_id IS NOT NULL;
CREATE TRIGGER economic_effects_updated_at BEFORE UPDATE ON economic_effects FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_effect_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('PENDING','APPLIED'), ('PENDING','UNKNOWN'), ('PENDING','FAILED'),
        ('UNKNOWN','APPLIED'), ('UNKNOWN','FAILED'),
        ('APPLIED','COMPENSATED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_economic_effect_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.effect_key, NEW.subsystem, NEW.effect_kind, NEW.intent_id, NEW.account_id, NEW.request_hash,
        NEW.posts_ledger, NEW.created_at)
       IS DISTINCT FROM
       (OLD.effect_key, OLD.subsystem, OLD.effect_kind, OLD.intent_id, OLD.account_id, OLD.request_hash,
        OLD.posts_ledger, OLD.created_at) THEN
        RAISE EXCEPTION 'EFFECT_IDENTITY_FROZEN: economic effect % may not change identity', OLD.id USING ERRCODE = 'EF002';
    END IF;
    -- A journal link, once written, is a fact: it is never moved to another transaction or removed.
    IF OLD.journal_transaction_id IS NOT NULL AND NEW.journal_transaction_id IS DISTINCT FROM OLD.journal_transaction_id THEN
        RAISE EXCEPTION 'EFFECT_IDENTITY_FROZEN: economic effect % already applied journal %', OLD.id, OLD.journal_transaction_id
            USING ERRCODE = 'EF002';
    END IF;
    IF OLD.compensation_journal_transaction_id IS NOT NULL
       AND NEW.compensation_journal_transaction_id IS DISTINCT FROM OLD.compensation_journal_transaction_id THEN
        RAISE EXCEPTION 'EFFECT_IDENTITY_FROZEN: economic effect % already compensated', OLD.id USING ERRCODE = 'EF002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_effect_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'EFFECT_TRANSITION_ILLEGAL: economic effect % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'EF001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
CREATE TRIGGER economic_effects_guard BEFORE UPDATE ON economic_effects
    FOR EACH ROW EXECUTE FUNCTION cp_economic_effect_guard();

CREATE TABLE economic_effect_transitions (
    id              uuid PRIMARY KEY,
    effect_id       uuid NOT NULL REFERENCES economic_effects(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    evidence_ref    text,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX economic_effect_transitions_idx ON economic_effect_transitions (effect_id, occurred_at);
CREATE TRIGGER economic_effect_transitions_immutable BEFORE UPDATE OR DELETE ON economic_effect_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER economic_effect_transitions_flag_edge AFTER INSERT ON economic_effect_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('effect_id', 'from_status', 'to_status', 'econ_effect_status');
CREATE CONSTRAINT TRIGGER economic_effects_require_transition AFTER UPDATE OF status ON economic_effects
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'econ_effect_status');

-- ---------------------------------------------------------------------------
-- 8. Risk Kernel V2 decisions
-- ---------------------------------------------------------------------------

CREATE TABLE advanced_risk_decisions (
    id                  uuid PRIMARY KEY,
    intent_id           uuid REFERENCES economic_intents(id),
    account_id          uuid REFERENCES accounts(id),
    intent_kind         text NOT NULL,
    stage               text NOT NULL CHECK (stage IN ('PRE_TRADE','FINAL','CONTINUOUS')),
    verdict             text NOT NULL CHECK (verdict IN ('ALLOW','REJECT')),
    reason_codes        text[] NOT NULL,
    input_hash          text NOT NULL,
    policy_version      text NOT NULL,
    policy_hash         text NOT NULL,
    evaluator_version   text NOT NULL,
    decision_hash       text NOT NULL,
    input               jsonb NOT NULL,
    decision            jsonb NOT NULL,
    correlation_id      text,
    evaluated_at        timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    -- A verdict and its reasons cannot disagree.
    CHECK ((verdict = 'ALLOW') = (cardinality(reason_codes) = 0))
);
CREATE INDEX advanced_risk_decisions_intent_idx ON advanced_risk_decisions (intent_id) WHERE intent_id IS NOT NULL;
CREATE INDEX advanced_risk_decisions_account_idx ON advanced_risk_decisions (account_id, evaluated_at DESC) WHERE account_id IS NOT NULL;
CREATE TRIGGER advanced_risk_decisions_immutable BEFORE UPDATE OR DELETE ON advanced_risk_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 9. Reconciliation kinds for the facilities
-- ---------------------------------------------------------------------------

ALTER TABLE reconciliation_records DROP CONSTRAINT reconciliation_records_kind_check;
ALTER TABLE reconciliation_records ADD CONSTRAINT reconciliation_records_kind_check CHECK (kind IN (
    'EXECUTION','WALLET_BALANCE','FUNDING','LEDGER_INTERNAL','POSITION_LEDGER','SUBMISSION_UNKNOWN',
    'ECONOMIC_EFFECT','STABLE_RESERVE','BRIDGE','PREDICTION_ESCROW','CLEARING','DEVNET_CHAIN'));

-- ---------------------------------------------------------------------------
-- 10. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON facilities, house_accounts TO cp_app;
GRANT UPDATE (status) ON facilities, house_accounts TO cp_app;
GRANT SELECT, INSERT ON economic_intents, economic_intent_transitions TO cp_app;
GRANT UPDATE (status, rejection_code, terminal_at) ON economic_intents TO cp_app;
GRANT SELECT, INSERT ON economic_effects, economic_effect_transitions TO cp_app;
GRANT UPDATE (status, journal_transaction_id, compensation_journal_transaction_id, reservation_id,
              external_ref, evidence_ref, evidence_sha256, failure_code) ON economic_effects TO cp_app;
GRANT SELECT, INSERT ON advanced_risk_decisions TO cp_app;
GRANT SELECT ON facilities, house_accounts, economic_intents, economic_intent_transitions, economic_effects,
               economic_effect_transitions, advanced_risk_decisions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the chart of accounts, journal kinds and effect history are never dropped
