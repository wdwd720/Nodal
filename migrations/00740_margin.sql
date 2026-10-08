-- +goose Up
-- Margin, leverage, borrowing and liquidation for simulated positions (ADR-0020;
-- docs/architecture/ADVANCED_FINANCIAL_MODEL.md; internal/margin).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A margin account belongs to one customer account, runs in PAPER or SANDBOX only, borrows only
--      from a LENDING_POOL facility, sends liquidation fees and deficits to an INSURANCE_FUND facility,
--      and is denominated in a SIMULATED numeraire. It is born NORMAL, its identity never changes, and
--      every status change follows the legal table and is bound to a transition row naming both ends.
--   2. A borrow lot's principal only shrinks, its accrual horizon only advances, and its capitalized
--      interest only grows by exactly the accruals recorded for it. Accruals are contiguous per lot:
--      each starts where the previous ended, so an accrual period can be charged once and only once.
--      A closed lot owes nothing and never changes again.
--   3. A liquidation case has a fixed step budget. A step can only be the next one, only within the
--      budget, only while the case is open, and only at the capped slippage below its recorded mark;
--      the case's step count moves one at a time and only over a recorded step. A case that cannot
--      progress ends in MANUAL_REVIEW, never in a loop.
--   4. History (transitions, accruals, collateral moves, steps, stress runs, policies) is append-only.
--
-- Custom SQLSTATEs (prefix MG):
--   MG001 a margin row born in a status other than its initial one
--   MG002 the identity of a margin row changed, or a set-once column was rewritten
--   MG003 an illegal status transition
--   MG004 a liquidation step out of sequence, beyond the case's budget, on a closed case, or uncounted
--   MG005 a borrow lot or accrual change that breaks the lot's arithmetic or contiguity
--   MG006 a margin account wired to the wrong facility kind or to a non-SIMULATED asset

-- ---------------------------------------------------------------------------
-- 1. Policies
-- ---------------------------------------------------------------------------

CREATE TABLE margin_policies (
    version      text PRIMARY KEY CHECK (length(version) BETWEEN 1 AND 100),
    policy_hash  text NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    document     jsonb NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER margin_policies_immutable BEFORE UPDATE OR DELETE ON margin_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 2. Margin accounts
-- ---------------------------------------------------------------------------

CREATE TABLE margin_accounts (
    id                          uuid PRIMARY KEY,
    account_id                  uuid NOT NULL UNIQUE REFERENCES accounts(id),
    margin_mode                 text NOT NULL CHECK (margin_mode IN ('CASH','ISOLATED','CROSS')),
    -- LIVE and DEVNET are absent on purpose: margin implements PAPER and SANDBOX only.
    mode                        text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    status                      text NOT NULL CHECK (status IN ('NORMAL','WARNING','MARGIN_CALL','LIQUIDATING','BANKRUPT','CLOSED')),
    policy_version              text NOT NULL REFERENCES margin_policies(version),
    numeraire_asset_id          uuid NOT NULL REFERENCES assets(id),
    isolated_asset_id           uuid REFERENCES assets(id),
    lending_pool_facility_id    uuid NOT NULL REFERENCES facilities(id),
    insurance_fund_facility_id  uuid NOT NULL REFERENCES facilities(id),
    opened_by_actor_type        text NOT NULL CHECK (opened_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    opened_by_actor_id          text NOT NULL CHECK (length(opened_by_actor_id) > 0),
    opened_at                   timestamptz NOT NULL,
    created_at                  timestamptz NOT NULL DEFAULT now(),
    updated_at                  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT margin_accounts_isolated_names_its_asset CHECK ((margin_mode = 'ISOLATED') = (isolated_asset_id IS NOT NULL)),
    CONSTRAINT margin_accounts_isolated_is_not_the_numeraire CHECK (isolated_asset_id IS DISTINCT FROM numeraire_asset_id)
);
CREATE INDEX margin_accounts_status_idx ON margin_accounts (status, id);
CREATE TRIGGER margin_accounts_updated_at BEFORE UPDATE ON margin_accounts FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_account_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('NORMAL','WARNING'), ('NORMAL','MARGIN_CALL'), ('NORMAL','LIQUIDATING'), ('NORMAL','CLOSED'),
        ('WARNING','NORMAL'), ('WARNING','MARGIN_CALL'), ('WARNING','LIQUIDATING'),
        ('MARGIN_CALL','NORMAL'), ('MARGIN_CALL','WARNING'), ('MARGIN_CALL','LIQUIDATING'),
        ('LIQUIDATING','NORMAL'), ('LIQUIDATING','WARNING'), ('LIQUIDATING','BANKRUPT'),
        ('BANKRUPT','CLOSED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_margin_account_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    fkind text;
    dom   text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'NORMAL' THEN
            RAISE EXCEPTION 'MARGIN_BIRTH: margin account % must be born NORMAL, not %', NEW.id, NEW.status USING ERRCODE = 'MG001';
        END IF;
        SELECT kind INTO fkind FROM facilities WHERE id = NEW.lending_pool_facility_id;
        IF fkind IS DISTINCT FROM 'LENDING_POOL' THEN
            RAISE EXCEPTION 'MARGIN_WIRING: margin account % borrows from facility % which is %, not a LENDING_POOL',
                NEW.id, NEW.lending_pool_facility_id, coalesce(fkind, 'missing') USING ERRCODE = 'MG006';
        END IF;
        SELECT kind INTO fkind FROM facilities WHERE id = NEW.insurance_fund_facility_id;
        IF fkind IS DISTINCT FROM 'INSURANCE_FUND' THEN
            RAISE EXCEPTION 'MARGIN_WIRING: margin account % insures with facility % which is %, not an INSURANCE_FUND',
                NEW.id, NEW.insurance_fund_facility_id, coalesce(fkind, 'missing') USING ERRCODE = 'MG006';
        END IF;
        SELECT value_domain INTO dom FROM assets WHERE id = NEW.numeraire_asset_id;
        IF dom IS DISTINCT FROM 'SIMULATED' THEN
            RAISE EXCEPTION 'MARGIN_WIRING: margin account % numeraire % is % value, not SIMULATED',
                NEW.id, NEW.numeraire_asset_id, coalesce(dom, 'no') USING ERRCODE = 'MG006';
        END IF;
        IF NEW.isolated_asset_id IS NOT NULL THEN
            SELECT value_domain INTO dom FROM assets WHERE id = NEW.isolated_asset_id;
            IF dom IS DISTINCT FROM 'SIMULATED' THEN
                RAISE EXCEPTION 'MARGIN_WIRING: margin account % isolates % which is % value, not SIMULATED',
                    NEW.id, NEW.isolated_asset_id, coalesce(dom, 'no') USING ERRCODE = 'MG006';
            END IF;
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id, NEW.account_id, NEW.margin_mode, NEW.mode, NEW.policy_version, NEW.numeraire_asset_id, NEW.isolated_asset_id,
        NEW.lending_pool_facility_id, NEW.insurance_fund_facility_id, NEW.opened_by_actor_type, NEW.opened_by_actor_id,
        NEW.opened_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.margin_mode, OLD.mode, OLD.policy_version, OLD.numeraire_asset_id, OLD.isolated_asset_id,
        OLD.lending_pool_facility_id, OLD.insurance_fund_facility_id, OLD.opened_by_actor_type, OLD.opened_by_actor_id,
        OLD.opened_at, OLD.created_at) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: margin account % may change status only', OLD.id USING ERRCODE = 'MG002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_margin_account_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'MARGIN_TRANSITION_ILLEGAL: margin account % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'MG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_account_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_accounts_guard BEFORE INSERT OR UPDATE ON margin_accounts
    FOR EACH ROW EXECUTE FUNCTION cp_margin_account_guard();

CREATE TABLE margin_account_transitions (
    id                 uuid PRIMARY KEY,
    margin_account_id  uuid NOT NULL REFERENCES margin_accounts(id),
    from_status        text NOT NULL,
    to_status          text NOT NULL,
    reason             text NOT NULL CHECK (length(reason) > 0),
    evaluation_hash    text,
    actor_type         text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id           text NOT NULL,
    occurred_at        timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_account_transitions_idx ON margin_account_transitions (margin_account_id, occurred_at);
CREATE TRIGGER margin_account_transitions_immutable BEFORE UPDATE OR DELETE ON margin_account_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER margin_account_transitions_flag_edge AFTER INSERT ON margin_account_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('margin_account_id', 'from_status', 'to_status', 'margin_account_status');
CREATE CONSTRAINT TRIGGER margin_accounts_require_transition AFTER UPDATE OF status ON margin_accounts
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'margin_account_status');

-- ---------------------------------------------------------------------------
-- 3. Borrow lots and interest accruals
-- ---------------------------------------------------------------------------

CREATE TABLE margin_borrow_lots (
    id                             uuid PRIMARY KEY,
    margin_account_id              uuid NOT NULL REFERENCES margin_accounts(id),
    asset_id                       uuid NOT NULL REFERENCES assets(id),
    principal                      numeric(38,0) NOT NULL CHECK (principal > 0),
    outstanding_principal          numeric(38,0) NOT NULL CHECK (outstanding_principal >= 0 AND outstanding_principal <= principal),
    outstanding_interest           numeric(38,0) NOT NULL CHECK (outstanding_interest >= 0),
    rate_bps                       bigint NOT NULL CHECK (rate_bps BETWEEN 0 AND 100000),
    opened_at                      timestamptz NOT NULL,
    accrued_through                timestamptz NOT NULL,
    closed_at                      timestamptz,
    close_reason                   text CHECK (close_reason IS NULL OR close_reason IN ('REPAID','DEFICIT_COVERED')),
    borrow_journal_transaction_id  uuid NOT NULL REFERENCES journal_transactions(id),
    effect_id                      uuid NOT NULL REFERENCES economic_effects(id),
    created_at                     timestamptz NOT NULL DEFAULT now(),
    updated_at                     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT margin_borrow_lots_horizon CHECK (accrued_through >= opened_at),
    CONSTRAINT margin_borrow_lots_closed_owes_nothing CHECK (
        closed_at IS NULL OR (outstanding_principal = 0 AND outstanding_interest = 0 AND close_reason IS NOT NULL)),
    CONSTRAINT margin_borrow_lots_reason_only_when_closed CHECK ((close_reason IS NULL) = (closed_at IS NULL))
);
CREATE INDEX margin_borrow_lots_open_idx ON margin_borrow_lots (margin_account_id, opened_at, id) WHERE closed_at IS NULL;
CREATE UNIQUE INDEX margin_borrow_lots_effect_once ON margin_borrow_lots (effect_id);
CREATE TRIGGER margin_borrow_lots_updated_at BEFORE UPDATE ON margin_borrow_lots FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE margin_interest_accruals (
    id                      uuid PRIMARY KEY,
    lot_id                  uuid NOT NULL REFERENCES margin_borrow_lots(id),
    margin_account_id       uuid NOT NULL REFERENCES margin_accounts(id),
    period_start            timestamptz NOT NULL,
    period_end              timestamptz NOT NULL,
    principal_basis         numeric(38,0) NOT NULL CHECK (principal_basis >= 0),
    rate_bps                bigint NOT NULL CHECK (rate_bps BETWEEN 0 AND 100000),
    amount                  numeric(38,0) NOT NULL CHECK (amount >= 0),
    journal_transaction_id  uuid REFERENCES journal_transactions(id),
    effect_id               uuid REFERENCES economic_effects(id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT margin_interest_accruals_period CHECK (period_end > period_start),
    -- One accrual per lot and period: the idempotency of interest is a key, not a convention.
    CONSTRAINT margin_interest_accruals_once UNIQUE (lot_id, period_start),
    CONSTRAINT margin_interest_accruals_end_once UNIQUE (lot_id, period_end),
    CONSTRAINT margin_interest_accruals_posted CHECK ((amount = 0) = (journal_transaction_id IS NULL)),
    CONSTRAINT margin_interest_accruals_effect CHECK ((journal_transaction_id IS NULL) = (effect_id IS NULL))
);
CREATE TRIGGER margin_interest_accruals_immutable BEFORE UPDATE OR DELETE ON margin_interest_accruals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_accrual_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lot margin_borrow_lots%ROWTYPE;
BEGIN
    SELECT * INTO lot FROM margin_borrow_lots WHERE id = NEW.lot_id;
    IF NOT FOUND OR lot.margin_account_id <> NEW.margin_account_id THEN
        RAISE EXCEPTION 'MARGIN_ACCRUAL: accrual names lot % of another account or no lot', NEW.lot_id USING ERRCODE = 'MG005';
    END IF;
    IF lot.closed_at IS NOT NULL THEN
        RAISE EXCEPTION 'MARGIN_ACCRUAL: lot % is closed and accrues nothing', NEW.lot_id USING ERRCODE = 'MG005';
    END IF;
    IF NEW.period_start <> lot.accrued_through THEN
        RAISE EXCEPTION 'MARGIN_ACCRUAL_GAP: lot % accrued through % and an accrual cannot start at %',
            NEW.lot_id, lot.accrued_through, NEW.period_start USING ERRCODE = 'MG005';
    END IF;
    IF NEW.rate_bps <> lot.rate_bps OR NEW.principal_basis <> lot.outstanding_principal THEN
        RAISE EXCEPTION 'MARGIN_ACCRUAL: accrual basis disagrees with lot %', NEW.lot_id USING ERRCODE = 'MG005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_accrual_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_interest_accruals_guard BEFORE INSERT ON margin_interest_accruals
    FOR EACH ROW EXECUTE FUNCTION cp_margin_accrual_guard();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_lot_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    accrued numeric(38,0);
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.outstanding_principal <> NEW.principal OR NEW.outstanding_interest <> 0
           OR NEW.accrued_through <> NEW.opened_at OR NEW.closed_at IS NOT NULL THEN
            RAISE EXCEPTION 'MARGIN_LOT_BIRTH: lot % must be born owing exactly its principal, accrued to its opening', NEW.id
                USING ERRCODE = 'MG005';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id, NEW.margin_account_id, NEW.asset_id, NEW.principal, NEW.rate_bps, NEW.opened_at,
        NEW.borrow_journal_transaction_id, NEW.effect_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.margin_account_id, OLD.asset_id, OLD.principal, OLD.rate_bps, OLD.opened_at,
        OLD.borrow_journal_transaction_id, OLD.effect_id, OLD.created_at) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: borrow lot % may change only its balances', OLD.id USING ERRCODE = 'MG002';
    END IF;
    IF OLD.closed_at IS NOT NULL THEN
        RAISE EXCEPTION 'MARGIN_LOT_CLOSED: borrow lot % is closed', OLD.id USING ERRCODE = 'MG005';
    END IF;
    IF NEW.outstanding_principal > OLD.outstanding_principal THEN
        RAISE EXCEPTION 'MARGIN_LOT_PRINCIPAL: lot % principal can only be repaid, never re-lent', OLD.id USING ERRCODE = 'MG005';
    END IF;
    IF NEW.accrued_through < OLD.accrued_through THEN
        RAISE EXCEPTION 'MARGIN_LOT_HORIZON: lot % accrual horizon cannot move back', OLD.id USING ERRCODE = 'MG005';
    END IF;
    IF NEW.accrued_through > OLD.accrued_through THEN
        -- An accrual step: principal unchanged, interest grows by exactly what was recorded.
        SELECT coalesce(sum(amount), 0) INTO accrued FROM margin_interest_accruals
         WHERE lot_id = OLD.id AND period_end > OLD.accrued_through AND period_end <= NEW.accrued_through;
        IF NOT EXISTS (SELECT 1 FROM margin_interest_accruals WHERE lot_id = OLD.id AND period_end = NEW.accrued_through) THEN
            RAISE EXCEPTION 'MARGIN_LOT_HORIZON: lot % cannot advance to % without an accrual ending there',
                OLD.id, NEW.accrued_through USING ERRCODE = 'MG005';
        END IF;
        IF NEW.outstanding_principal <> OLD.outstanding_principal
           OR NEW.outstanding_interest <> OLD.outstanding_interest + accrued THEN
            RAISE EXCEPTION 'MARGIN_LOT_INTEREST: lot % interest must grow by exactly its accruals (%)', OLD.id, accrued
                USING ERRCODE = 'MG005';
        END IF;
    ELSIF NEW.outstanding_interest > OLD.outstanding_interest THEN
        RAISE EXCEPTION 'MARGIN_LOT_INTEREST: lot % interest grew without an accrual', OLD.id USING ERRCODE = 'MG005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_lot_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_borrow_lots_guard BEFORE INSERT OR UPDATE ON margin_borrow_lots
    FOR EACH ROW EXECUTE FUNCTION cp_margin_lot_guard();

-- ---------------------------------------------------------------------------
-- 4. Collateral moves
-- ---------------------------------------------------------------------------

CREATE TABLE margin_collateral_moves (
    id                      uuid PRIMARY KEY,
    margin_account_id       uuid NOT NULL REFERENCES margin_accounts(id),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    direction               text NOT NULL CHECK (direction IN ('POST','RELEASE')),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    value_minor             bigint NOT NULL CHECK (value_minor >= 0),
    journal_transaction_id  uuid NOT NULL REFERENCES journal_transactions(id),
    effect_id               uuid NOT NULL REFERENCES economic_effects(id),
    intent_id               uuid REFERENCES economic_intents(id),
    occurred_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_collateral_moves_idx ON margin_collateral_moves (margin_account_id, direction, occurred_at);
CREATE UNIQUE INDEX margin_collateral_moves_journal_once ON margin_collateral_moves (journal_transaction_id);
CREATE TRIGGER margin_collateral_moves_immutable BEFORE UPDATE OR DELETE ON margin_collateral_moves
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 5. Margin calls
-- ---------------------------------------------------------------------------

CREATE TABLE margin_calls (
    id                 uuid PRIMARY KEY,
    margin_account_id  uuid NOT NULL REFERENCES margin_accounts(id),
    status             text NOT NULL CHECK (status IN ('OPEN','MET','LIQUIDATED')),
    issued_at          timestamptz NOT NULL,
    equity_units       numeric(38,0) NOT NULL,
    maintenance_units  numeric(38,0) NOT NULL CHECK (maintenance_units >= 0),
    deficiency_units   numeric(38,0) NOT NULL CHECK (deficiency_units > 0),
    evaluation_hash    text NOT NULL,
    resolved_at        timestamptz,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT margin_calls_deficiency CHECK (deficiency_units = maintenance_units - equity_units),
    CONSTRAINT margin_calls_resolved CHECK ((status = 'OPEN') = (resolved_at IS NULL))
);
CREATE UNIQUE INDEX margin_calls_one_open ON margin_calls (margin_account_id) WHERE status = 'OPEN';
CREATE TRIGGER margin_calls_updated_at BEFORE UPDATE ON margin_calls FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_call_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('OPEN','MET'), ('OPEN','LIQUIDATED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_margin_call_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'OPEN' THEN
            RAISE EXCEPTION 'MARGIN_BIRTH: margin call % must be born OPEN', NEW.id USING ERRCODE = 'MG001';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id, NEW.margin_account_id, NEW.issued_at, NEW.equity_units, NEW.maintenance_units, NEW.deficiency_units,
        NEW.evaluation_hash, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.margin_account_id, OLD.issued_at, OLD.equity_units, OLD.maintenance_units, OLD.deficiency_units,
        OLD.evaluation_hash, OLD.created_at) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: margin call % may change status only', OLD.id USING ERRCODE = 'MG002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_margin_call_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'MARGIN_TRANSITION_ILLEGAL: margin call % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'MG003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_call_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_calls_guard BEFORE INSERT OR UPDATE ON margin_calls
    FOR EACH ROW EXECUTE FUNCTION cp_margin_call_guard();

CREATE TABLE margin_call_transitions (
    id             uuid PRIMARY KEY,
    margin_call_id uuid NOT NULL REFERENCES margin_calls(id),
    from_status    text NOT NULL,
    to_status      text NOT NULL,
    reason         text NOT NULL CHECK (length(reason) > 0),
    actor_type     text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id       text NOT NULL,
    occurred_at    timestamptz NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_call_transitions_idx ON margin_call_transitions (margin_call_id, occurred_at);
CREATE TRIGGER margin_call_transitions_immutable BEFORE UPDATE OR DELETE ON margin_call_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER margin_call_transitions_flag_edge AFTER INSERT ON margin_call_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('margin_call_id', 'from_status', 'to_status', 'margin_call_status');
CREATE CONSTRAINT TRIGGER margin_calls_require_transition AFTER UPDATE OF status ON margin_calls
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'margin_call_status');

-- ---------------------------------------------------------------------------
-- 6. Liquidation cases and steps
-- ---------------------------------------------------------------------------

CREATE TABLE margin_liquidation_cases (
    id                                       uuid PRIMARY KEY,
    margin_account_id                        uuid NOT NULL REFERENCES margin_accounts(id),
    margin_call_id                           uuid NOT NULL UNIQUE REFERENCES margin_calls(id),
    status                                   text NOT NULL CHECK (status IN ('AWAITING_PRICE','AWAITING_VENUE','IN_PROGRESS',
                                                                             'COMPLETED','BANKRUPT','MANUAL_REVIEW')),
    policy_version                           text NOT NULL REFERENCES margin_policies(version),
    target                                   text NOT NULL CHECK (target IN ('INITIAL_MARGIN','MAINTENANCE_PLUS_BUFFER')),
    max_steps                                integer NOT NULL CHECK (max_steps BETWEEN 1 AND 100),
    steps_executed                           integer NOT NULL CHECK (steps_executed >= 0),
    opened_at                                timestamptz NOT NULL,
    closed_at                                timestamptz,
    last_reason                              text NOT NULL,
    deficit_units                            numeric(38,0) CHECK (deficit_units IS NULL OR deficit_units > 0),
    deficit_coverage_journal_transaction_id  uuid REFERENCES journal_transactions(id),
    deficit_covered_at                       timestamptz,
    created_at                               timestamptz NOT NULL DEFAULT now(),
    updated_at                               timestamptz NOT NULL DEFAULT now(),
    -- The step budget is a constraint, not a convention: no case ever records more steps than it was given.
    CONSTRAINT margin_liquidation_cases_step_budget CHECK (steps_executed <= max_steps),
    CONSTRAINT margin_liquidation_cases_closed CHECK (
        (status IN ('COMPLETED','BANKRUPT','MANUAL_REVIEW')) = (closed_at IS NOT NULL)),
    CONSTRAINT margin_liquidation_cases_deficit_only_bankrupt CHECK (status = 'BANKRUPT' OR deficit_units IS NULL),
    CONSTRAINT margin_liquidation_cases_bankrupt_has_deficit CHECK (status <> 'BANKRUPT' OR deficit_units IS NOT NULL),
    CONSTRAINT margin_liquidation_cases_coverage CHECK (
        (deficit_coverage_journal_transaction_id IS NULL) = (deficit_covered_at IS NULL)
        AND (deficit_coverage_journal_transaction_id IS NULL OR status = 'BANKRUPT'))
);
CREATE UNIQUE INDEX margin_liquidation_cases_one_unresolved ON margin_liquidation_cases (margin_account_id)
    WHERE status IN ('AWAITING_PRICE','AWAITING_VENUE','IN_PROGRESS','MANUAL_REVIEW');
CREATE INDEX margin_liquidation_cases_open_idx ON margin_liquidation_cases (status, id)
    WHERE status IN ('AWAITING_PRICE','AWAITING_VENUE','IN_PROGRESS');
CREATE TRIGGER margin_liquidation_cases_updated_at BEFORE UPDATE ON margin_liquidation_cases
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_case_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('AWAITING_PRICE','IN_PROGRESS'), ('AWAITING_PRICE','AWAITING_VENUE'), ('AWAITING_PRICE','COMPLETED'),
        ('AWAITING_PRICE','BANKRUPT'), ('AWAITING_PRICE','MANUAL_REVIEW'),
        ('AWAITING_VENUE','IN_PROGRESS'), ('AWAITING_VENUE','AWAITING_PRICE'), ('AWAITING_VENUE','COMPLETED'),
        ('AWAITING_VENUE','BANKRUPT'), ('AWAITING_VENUE','MANUAL_REVIEW'),
        ('IN_PROGRESS','AWAITING_PRICE'), ('IN_PROGRESS','AWAITING_VENUE'), ('IN_PROGRESS','COMPLETED'),
        ('IN_PROGRESS','BANKRUPT'), ('IN_PROGRESS','MANUAL_REVIEW'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_margin_case_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('IN_PROGRESS','AWAITING_PRICE','AWAITING_VENUE') OR NEW.steps_executed <> 0
           OR NEW.deficit_units IS NOT NULL OR NEW.deficit_coverage_journal_transaction_id IS NOT NULL THEN
            RAISE EXCEPTION 'MARGIN_BIRTH: liquidation case % must be born open with no steps', NEW.id USING ERRCODE = 'MG001';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.id, NEW.margin_account_id, NEW.margin_call_id, NEW.policy_version, NEW.target, NEW.max_steps,
        NEW.opened_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.margin_account_id, OLD.margin_call_id, OLD.policy_version, OLD.target, OLD.max_steps,
        OLD.opened_at, OLD.created_at) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: liquidation case % may not change identity', OLD.id USING ERRCODE = 'MG002';
    END IF;
    IF (OLD.deficit_units IS NOT NULL AND NEW.deficit_units IS DISTINCT FROM OLD.deficit_units)
       OR (OLD.deficit_coverage_journal_transaction_id IS NOT NULL
           AND (NEW.deficit_coverage_journal_transaction_id IS DISTINCT FROM OLD.deficit_coverage_journal_transaction_id
                OR NEW.deficit_covered_at IS DISTINCT FROM OLD.deficit_covered_at))
       OR (OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: liquidation case % already recorded its outcome', OLD.id USING ERRCODE = 'MG002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_margin_case_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'MARGIN_TRANSITION_ILLEGAL: liquidation case % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'MG003';
    END IF;
    IF NEW.steps_executed <> OLD.steps_executed THEN
        IF NEW.steps_executed <> OLD.steps_executed + 1
           OR NOT EXISTS (SELECT 1 FROM margin_liquidation_steps WHERE case_id = OLD.id AND step_no = NEW.steps_executed) THEN
            RAISE EXCEPTION 'MARGIN_STEP_COUNT: liquidation case % can count step % only one at a time and only over a recorded step',
                OLD.id, NEW.steps_executed USING ERRCODE = 'MG004';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_case_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_liquidation_cases_guard BEFORE INSERT OR UPDATE ON margin_liquidation_cases
    FOR EACH ROW EXECUTE FUNCTION cp_margin_case_guard();

CREATE TABLE margin_liquidation_case_transitions (
    id           uuid PRIMARY KEY,
    case_id      uuid NOT NULL REFERENCES margin_liquidation_cases(id),
    from_status  text NOT NULL,
    to_status    text NOT NULL,
    reason       text NOT NULL CHECK (length(reason) > 0),
    actor_type   text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id     text NOT NULL,
    occurred_at  timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_liquidation_case_transitions_idx ON margin_liquidation_case_transitions (case_id, occurred_at);
CREATE TRIGGER margin_liquidation_case_transitions_immutable BEFORE UPDATE OR DELETE ON margin_liquidation_case_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER margin_liquidation_case_transitions_flag_edge AFTER INSERT ON margin_liquidation_case_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('case_id', 'from_status', 'to_status', 'margin_case_status');
CREATE CONSTRAINT TRIGGER margin_liquidation_cases_require_transition AFTER UPDATE OF status ON margin_liquidation_cases
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'margin_case_status');

CREATE TABLE margin_liquidation_steps (
    id                              uuid PRIMARY KEY,
    case_id                         uuid NOT NULL REFERENCES margin_liquidation_cases(id),
    step_no                         integer NOT NULL CHECK (step_no >= 1),
    action                          text NOT NULL CHECK (action IN ('SELL','APPLY_COLLATERAL')),
    asset_id                        uuid NOT NULL REFERENCES assets(id),
    quantity                        numeric(38,0) NOT NULL CHECK (quantity > 0),
    mark_mantissa                   numeric(38,0) CHECK (mark_mantissa IS NULL OR mark_mantissa >= 0),
    mark_scale                      integer CHECK (mark_scale IS NULL OR mark_scale BETWEEN 0 AND 38),
    mark_source                     text,
    mark_observed_at                timestamptz,
    execution_mantissa              numeric(38,0) CHECK (execution_mantissa IS NULL OR execution_mantissa >= 0),
    execution_scale                 integer CHECK (execution_scale IS NULL OR execution_scale BETWEEN 0 AND 38),
    slippage_bps                    bigint NOT NULL CHECK (slippage_bps BETWEEN 0 AND 10000),
    slippage_cap_bps                bigint NOT NULL CHECK (slippage_cap_bps BETWEEN 0 AND 10000),
    proceeds                        numeric(38,0) NOT NULL CHECK (proceeds >= 0),
    fee                             numeric(38,0) NOT NULL CHECK (fee >= 0),
    repaid                          numeric(38,0) NOT NULL CHECK (repaid >= 0),
    excess                          numeric(38,0) NOT NULL CHECK (excess >= 0),
    trade_journal_transaction_id    uuid REFERENCES journal_transactions(id),
    release_journal_transaction_id  uuid REFERENCES journal_transactions(id),
    repay_journal_transaction_id    uuid REFERENCES journal_transactions(id),
    excess_journal_transaction_id   uuid REFERENCES journal_transactions(id),
    equity_before                   numeric(38,0) NOT NULL,
    equity_after                    numeric(38,0) NOT NULL,
    plan_hash                       text NOT NULL,
    executed_at                     timestamptz NOT NULL,
    created_at                      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT margin_liquidation_steps_once UNIQUE (case_id, step_no),
    CONSTRAINT margin_liquidation_steps_proceeds_split CHECK (proceeds = fee + repaid + excess),
    -- The slippage cap applies to the execution price, and the database can check it: a sale
    -- executes exactly the recorded slippage below its recorded mark, and never more than the cap.
    CONSTRAINT margin_liquidation_steps_slippage_capped CHECK (slippage_bps <= slippage_cap_bps),
    CONSTRAINT margin_liquidation_steps_sale_priced CHECK (
        action <> 'SELL' OR (
            mark_mantissa IS NOT NULL AND mark_scale IS NOT NULL AND mark_observed_at IS NOT NULL
            AND execution_mantissa IS NOT NULL AND execution_scale IS NOT NULL
            AND trade_journal_transaction_id IS NOT NULL
            AND ((execution_scale = mark_scale + 4 AND execution_mantissa = mark_mantissa * (10000 - slippage_bps))
                 OR (execution_scale = mark_scale AND execution_mantissa = floor(mark_mantissa * (10000 - slippage_bps) / 10000))))),
    CONSTRAINT margin_liquidation_steps_apply_shape CHECK (
        action <> 'APPLY_COLLATERAL' OR (
            fee = 0 AND excess = 0 AND slippage_bps = 0 AND proceeds = quantity AND repaid = quantity
            AND trade_journal_transaction_id IS NULL AND release_journal_transaction_id IS NOT NULL)),
    CONSTRAINT margin_liquidation_steps_repay_posted CHECK ((repaid > 0) = (repay_journal_transaction_id IS NOT NULL)),
    CONSTRAINT margin_liquidation_steps_excess_posted CHECK ((excess > 0) = (excess_journal_transaction_id IS NOT NULL))
);
CREATE TRIGGER margin_liquidation_steps_immutable BEFORE UPDATE OR DELETE ON margin_liquidation_steps
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_step_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    c margin_liquidation_cases%ROWTYPE;
BEGIN
    SELECT * INTO c FROM margin_liquidation_cases WHERE id = NEW.case_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'MARGIN_STEP: liquidation case % does not exist', NEW.case_id USING ERRCODE = 'MG004';
    END IF;
    IF c.status IN ('COMPLETED','BANKRUPT','MANUAL_REVIEW') THEN
        RAISE EXCEPTION 'MARGIN_STEP: liquidation case % is % and takes no more steps', c.id, c.status USING ERRCODE = 'MG004';
    END IF;
    IF NEW.step_no <> c.steps_executed + 1 THEN
        RAISE EXCEPTION 'MARGIN_STEP: liquidation case % is at step % and cannot record step %',
            c.id, c.steps_executed, NEW.step_no USING ERRCODE = 'MG004';
    END IF;
    IF NEW.step_no > c.max_steps THEN
        RAISE EXCEPTION 'MARGIN_STEP_BUDGET: liquidation case % has a budget of % steps', c.id, c.max_steps USING ERRCODE = 'MG004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_step_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_liquidation_steps_guard BEFORE INSERT ON margin_liquidation_steps
    FOR EACH ROW EXECUTE FUNCTION cp_margin_step_guard();

-- A recorded step the case did not count would let the next one reuse its number; refused at COMMIT.
-- +goose StatementBegin
CREATE FUNCTION cp_margin_step_counted() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    counted integer;
BEGIN
    SELECT steps_executed INTO counted FROM margin_liquidation_cases WHERE id = NEW.case_id;
    IF counted IS NULL OR counted < NEW.step_no THEN
        RAISE EXCEPTION 'MARGIN_STEP_UNCOUNTED: liquidation case % recorded step % but counts %',
            NEW.case_id, NEW.step_no, coalesce(counted, 0) USING ERRCODE = 'MG004';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_step_counted() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER margin_liquidation_steps_counted AFTER INSERT ON margin_liquidation_steps
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_margin_step_counted();

-- ---------------------------------------------------------------------------
-- 7. Stress runs
-- ---------------------------------------------------------------------------

CREATE TABLE margin_stress_runs (
    id                 uuid PRIMARY KEY,
    margin_account_id  uuid NOT NULL REFERENCES margin_accounts(id),
    policy_version     text NOT NULL REFERENCES margin_policies(version),
    policy_hash        text NOT NULL CHECK (policy_hash ~ '^[0-9a-f]{64}$'),
    input_hash         text NOT NULL CHECK (input_hash ~ '^[0-9a-f]{64}$'),
    report             jsonb NOT NULL,
    report_hash        text NOT NULL CHECK (report_hash ~ '^[0-9a-f]{64}$'),
    run_at             timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_stress_runs_idx ON margin_stress_runs (margin_account_id, run_at DESC);
CREATE TRIGGER margin_stress_runs_immutable BEFORE UPDATE OR DELETE ON margin_stress_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 8. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON margin_policies, margin_accounts, margin_account_transitions, margin_borrow_lots,
    margin_interest_accruals, margin_collateral_moves, margin_calls, margin_call_transitions,
    margin_liquidation_cases, margin_liquidation_case_transitions, margin_liquidation_steps, margin_stress_runs TO cp_app;
GRANT UPDATE (status) ON margin_accounts TO cp_app;
GRANT UPDATE (status, resolved_at) ON margin_calls TO cp_app;
GRANT UPDATE (status, steps_executed, closed_at, last_reason, deficit_units, deficit_coverage_journal_transaction_id,
              deficit_covered_at) ON margin_liquidation_cases TO cp_app;
GRANT UPDATE (outstanding_principal, outstanding_interest, accrued_through, closed_at, close_reason) ON margin_borrow_lots TO cp_app;
GRANT SELECT ON margin_policies, margin_accounts, margin_account_transitions, margin_borrow_lots, margin_interest_accruals,
    margin_collateral_moves, margin_calls, margin_call_transitions, margin_liquidation_cases,
    margin_liquidation_case_transitions, margin_liquidation_steps, margin_stress_runs TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: margin history backs journal transactions and is never dropped
