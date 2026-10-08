-- +goose Up
-- Margin hardening (internal/margin; amends 00740 without editing it).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A liquidation case's step budget bounds its TRADING steps (SELL), one budget window at a time.
--      Applying numeraire collateral and sweeping the customer's free wallet cash are terminal accounting:
--      they are not bounded by the budget, and each repays a positive amount of a finite loan. A window
--      restarts only when two operators resume the case (budget_from_step moves to the steps already taken).
--   2. MANUAL_REVIEW has an exit, and only one: a resolution proposed by one OPERATOR and approved by a
--      second, distinct OPERATOR, for the review the case is in now. It either returns the case to
--      IN_PROGRESS with a fresh budget window or declares it BANKRUPT. The transition row out of
--      MANUAL_REVIEW names that resolution, a resolution licenses one exit, and an exit without one is refused.
--   3. A case is BANKRUPT only when the account holds no margin collateral: nothing is left to sell or apply.
--   4. No interest is charged while a case is in MANUAL_REVIEW or the account is BANKRUPT (a zero accrual,
--      which records the waived period, is allowed).
--   5. A liquidation step may be a SWEEP_WALLET: free numeraire cash in the customer's wallet repays the
--      loans before bankruptcy, with its own repayment journal and no trade.
--   6. A covered deficit is repaid to the insurance fund at most once in total: the repayments of a case never
--      exceed its deficit, each matches its MARGIN_REPAY journal (customer Dr DEFICIT / Cr WALLET, fund Dr
--      INSURANCE_FUND_CASH / Cr DEFICIT_RECEIVABLE) and its APPLIED economic effect.
--   7. An APPLIED margin economic effect names exactly the journal its own effect key posted, of the journal
--      kind and reference type that effect kind produces, touching only the effect's own customer account
--      (none for facility funding). Checked at COMMIT.
--
-- Custom SQLSTATEs (prefix MG): MG007 interest charged while the case is in MANUAL_REVIEW or the account is
-- BANKRUPT, MG008 an APPLIED margin effect whose journal is not the one its key posted for its kind,
-- MG009 a MANUAL_REVIEW exit or resolution without a second distinct operator's approval for the current
-- review, MG010 a case declared BANKRUPT while the account still holds margin collateral, MG011 a deficit
-- repayment before coverage, beyond the deficit, or not matching its journal and effect.

-- ---------------------------------------------------------------------------
-- 1. The step budget bounds trading steps, one window at a time
-- ---------------------------------------------------------------------------

ALTER TABLE margin_liquidation_cases ADD COLUMN budget_from_step integer NOT NULL DEFAULT 0;
ALTER TABLE margin_liquidation_cases DROP CONSTRAINT margin_liquidation_cases_step_budget;
ALTER TABLE margin_liquidation_cases ADD CONSTRAINT margin_liquidation_cases_budget_window
    CHECK (budget_from_step >= 0 AND budget_from_step <= steps_executed);

ALTER TABLE margin_liquidation_steps DROP CONSTRAINT margin_liquidation_steps_action_check;
ALTER TABLE margin_liquidation_steps ADD CONSTRAINT margin_liquidation_steps_action_check
    CHECK (action IN ('SELL','APPLY_COLLATERAL','SWEEP_WALLET'));
ALTER TABLE margin_liquidation_steps ADD CONSTRAINT margin_liquidation_steps_sweep_shape CHECK (
    action <> 'SWEEP_WALLET' OR (
        fee = 0 AND excess = 0 AND slippage_bps = 0 AND proceeds = quantity AND repaid = quantity
        AND mark_mantissa IS NULL AND execution_mantissa IS NULL
        AND trade_journal_transaction_id IS NULL AND release_journal_transaction_id IS NULL
        AND repay_journal_transaction_id IS NOT NULL));

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_margin_step_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    c    margin_liquidation_cases%ROWTYPE;
    used integer;
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
    -- The budget bounds trading: SELL steps since the window began. Applying collateral and sweeping the
    -- wallet are terminal accounting and each repays a positive amount (their shape CHECKs).
    IF NEW.action = 'SELL' THEN
        SELECT count(*) INTO used FROM margin_liquidation_steps
         WHERE case_id = c.id AND action = 'SELL' AND step_no > c.budget_from_step;
        IF used >= c.max_steps THEN
            RAISE EXCEPTION 'MARGIN_STEP_BUDGET: liquidation case % has used its budget of % trading steps', c.id, c.max_steps
                USING ERRCODE = 'MG004';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
-- CREATE OR REPLACE resets a function's settings; pin the search path again, as 00740 did.
ALTER FUNCTION cp_margin_step_guard() SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 2. MANUAL_REVIEW resolutions: proposed by one operator, approved by another
-- ---------------------------------------------------------------------------

CREATE TABLE margin_case_resolutions (
    id                      uuid PRIMARY KEY,
    case_id                 uuid NOT NULL REFERENCES margin_liquidation_cases(id),
    -- The transition that put the case in review: a resolution answers that review and no later one.
    review_transition_id    uuid NOT NULL REFERENCES margin_liquidation_case_transitions(id),
    outcome                 text NOT NULL CHECK (outcome IN ('RESUME','DECLARE_BANKRUPT')),
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    proposed_by_actor_type  text NOT NULL CHECK (proposed_by_actor_type = 'OPERATOR'),
    proposed_by_actor_id    text NOT NULL CHECK (length(proposed_by_actor_id) BETWEEN 1 AND 200),
    proposed_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_case_resolutions_case_idx ON margin_case_resolutions (case_id, proposed_at);
CREATE TRIGGER margin_case_resolutions_immutable BEFORE UPDATE OR DELETE ON margin_case_resolutions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE margin_case_resolution_decisions (
    id                     uuid PRIMARY KEY,
    resolution_id          uuid NOT NULL UNIQUE REFERENCES margin_case_resolutions(id),
    decision               text NOT NULL CHECK (decision IN ('APPROVED','REJECTED')),
    reason                 text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    decided_by_actor_type  text NOT NULL CHECK (decided_by_actor_type = 'OPERATOR'),
    decided_by_actor_id    text NOT NULL CHECK (length(decided_by_actor_id) BETWEEN 1 AND 200),
    decided_at             timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER margin_case_resolution_decisions_immutable BEFORE UPDATE OR DELETE ON margin_case_resolution_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE margin_liquidation_case_transitions ADD COLUMN resolution_id uuid REFERENCES margin_case_resolutions(id);
-- Leaving MANUAL_REVIEW is always a resolution, and only leaving it is.
ALTER TABLE margin_liquidation_case_transitions ADD CONSTRAINT margin_liquidation_case_transitions_review_exit_resolved
    CHECK ((from_status = 'MANUAL_REVIEW') = (resolution_id IS NOT NULL));
CREATE UNIQUE INDEX margin_liquidation_case_transitions_resolution_once
    ON margin_liquidation_case_transitions (resolution_id) WHERE resolution_id IS NOT NULL;

-- A review is left at most once: no transition has used any resolution of this review yet.
-- +goose StatementBegin
CREATE FUNCTION cp_margin_review_left(p_review_transition uuid) RETURNS boolean
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (SELECT 1 FROM margin_liquidation_case_transitions t
                     JOIN margin_case_resolutions r ON r.id = t.resolution_id
                    WHERE r.review_transition_id = p_review_transition);
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_review_left(uuid) SET search_path = pg_catalog, public, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION cp_margin_resolution_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    rev         record;
    case_status text;
BEGIN
    SELECT case_id, to_status INTO rev FROM margin_liquidation_case_transitions WHERE id = NEW.review_transition_id;
    IF rev.case_id IS DISTINCT FROM NEW.case_id OR rev.to_status IS DISTINCT FROM 'MANUAL_REVIEW' THEN
        RAISE EXCEPTION 'MARGIN_RESOLUTION: % does not name the transition that put case % in MANUAL_REVIEW',
            NEW.review_transition_id, NEW.case_id USING ERRCODE = 'MG009';
    END IF;
    SELECT lc.status INTO case_status FROM margin_liquidation_cases lc WHERE lc.id = NEW.case_id;
    IF case_status IS DISTINCT FROM 'MANUAL_REVIEW' OR cp_margin_review_left(NEW.review_transition_id) THEN
        RAISE EXCEPTION 'MARGIN_RESOLUTION: case % is not in the review a resolution of % would answer',
            NEW.case_id, NEW.review_transition_id USING ERRCODE = 'MG009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_resolution_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_case_resolutions_guard BEFORE INSERT ON margin_case_resolutions
    FOR EACH ROW EXECUTE FUNCTION cp_margin_resolution_guard();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_resolution_decision_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r margin_case_resolutions%ROWTYPE;
BEGIN
    SELECT * INTO r FROM margin_case_resolutions WHERE id = NEW.resolution_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'MARGIN_RESOLUTION: resolution % does not exist', NEW.resolution_id USING ERRCODE = 'MG009';
    END IF;
    IF NEW.decision = 'APPROVED' AND NEW.decided_by_actor_id = r.proposed_by_actor_id THEN
        RAISE EXCEPTION 'MARGIN_RESOLUTION_DUAL_CONTROL: resolution % was proposed by % and cannot be approved by them',
            r.id, r.proposed_by_actor_id USING ERRCODE = 'MG009';
    END IF;
    IF NEW.decision = 'APPROVED' AND cp_margin_review_left(r.review_transition_id) THEN
        RAISE EXCEPTION 'MARGIN_RESOLUTION: the review resolution % answers is already over', r.id USING ERRCODE = 'MG009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_resolution_decision_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_case_resolution_decisions_guard BEFORE INSERT ON margin_case_resolution_decisions
    FOR EACH ROW EXECUTE FUNCTION cp_margin_resolution_decision_guard();

-- The transition out of MANUAL_REVIEW names an APPROVED resolution of this case, for the review it is in,
-- whose outcome is that transition, approved by a second operator who also writes the transition.
-- +goose StatementBegin
CREATE FUNCTION cp_margin_review_exit_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    r margin_case_resolutions%ROWTYPE;
    d margin_case_resolution_decisions%ROWTYPE;
BEGIN
    SELECT * INTO r FROM margin_case_resolutions WHERE id = NEW.resolution_id;
    SELECT * INTO d FROM margin_case_resolution_decisions WHERE resolution_id = NEW.resolution_id;
    IF r.id IS NULL OR r.case_id IS DISTINCT FROM NEW.case_id
       OR d.id IS NULL OR d.decision IS DISTINCT FROM 'APPROVED'
       OR d.decided_by_actor_id = r.proposed_by_actor_id
       OR NEW.actor_type IS DISTINCT FROM 'OPERATOR' OR NEW.actor_id IS DISTINCT FROM d.decided_by_actor_id
       OR NOT ((r.outcome = 'RESUME' AND NEW.to_status = 'IN_PROGRESS')
               OR (r.outcome = 'DECLARE_BANKRUPT' AND NEW.to_status = 'BANKRUPT'))
       OR cp_margin_review_left(r.review_transition_id) THEN
        RAISE EXCEPTION 'MARGIN_REVIEW_EXIT: case % cannot leave MANUAL_REVIEW for % on resolution %',
            NEW.case_id, NEW.to_status, NEW.resolution_id USING ERRCODE = 'MG009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_review_exit_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_liquidation_case_transitions_review_exit BEFORE INSERT ON margin_liquidation_case_transitions
    FOR EACH ROW WHEN (NEW.resolution_id IS NOT NULL) EXECUTE FUNCTION cp_margin_review_exit_guard();

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_margin_case_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('AWAITING_PRICE','IN_PROGRESS'), ('AWAITING_PRICE','AWAITING_VENUE'), ('AWAITING_PRICE','COMPLETED'),
        ('AWAITING_PRICE','BANKRUPT'), ('AWAITING_PRICE','MANUAL_REVIEW'),
        ('AWAITING_VENUE','IN_PROGRESS'), ('AWAITING_VENUE','AWAITING_PRICE'), ('AWAITING_VENUE','COMPLETED'),
        ('AWAITING_VENUE','BANKRUPT'), ('AWAITING_VENUE','MANUAL_REVIEW'),
        ('IN_PROGRESS','AWAITING_PRICE'), ('IN_PROGRESS','AWAITING_VENUE'), ('IN_PROGRESS','COMPLETED'),
        ('IN_PROGRESS','BANKRUPT'), ('IN_PROGRESS','MANUAL_REVIEW'),
        -- Only through an approved resolution (cp_margin_review_exit_guard).
        ('MANUAL_REVIEW','IN_PROGRESS'), ('MANUAL_REVIEW','BANKRUPT'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_margin_case_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    resolving boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status NOT IN ('IN_PROGRESS','AWAITING_PRICE','AWAITING_VENUE') OR NEW.steps_executed <> 0
           OR NEW.budget_from_step <> 0
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
    -- A resolution reopens a case for automation (closed_at cleared) or closes it BANKRUPT; nothing else
    -- rewrites an outcome already recorded.
    resolving := OLD.status = 'MANUAL_REVIEW' AND NEW.status IN ('IN_PROGRESS','BANKRUPT');
    IF (OLD.deficit_units IS NOT NULL AND NEW.deficit_units IS DISTINCT FROM OLD.deficit_units)
       OR (OLD.deficit_coverage_journal_transaction_id IS NOT NULL
           AND (NEW.deficit_coverage_journal_transaction_id IS DISTINCT FROM OLD.deficit_coverage_journal_transaction_id
                OR NEW.deficit_covered_at IS DISTINCT FROM OLD.deficit_covered_at))
       OR (OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at AND NOT resolving) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: liquidation case % already recorded its outcome', OLD.id USING ERRCODE = 'MG002';
    END IF;
    -- The budget window restarts only when a case is resumed, and then at the steps already taken.
    IF NEW.budget_from_step IS DISTINCT FROM OLD.budget_from_step
       AND NOT (OLD.status = 'MANUAL_REVIEW' AND NEW.status = 'IN_PROGRESS' AND NEW.budget_from_step = OLD.steps_executed) THEN
        RAISE EXCEPTION 'MARGIN_IDENTITY_FROZEN: liquidation case % budget window moves only when the case resumes', OLD.id
            USING ERRCODE = 'MG002';
    END IF;
    IF OLD.status = 'MANUAL_REVIEW' AND NEW.status = 'IN_PROGRESS' AND NEW.budget_from_step <> OLD.steps_executed THEN
        RAISE EXCEPTION 'MARGIN_REVIEW_EXIT: a resumed case % starts a fresh budget window', OLD.id USING ERRCODE = 'MG009';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_margin_case_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'MARGIN_TRANSITION_ILLEGAL: liquidation case % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'MG003';
    END IF;
    -- Bankruptcy means nothing is left to sell or apply.
    IF NEW.status = 'BANKRUPT' AND OLD.status IS DISTINCT FROM 'BANKRUPT' AND EXISTS (
        SELECT 1 FROM margin_accounts m
          JOIN ledger_accounts la ON la.owner_type = 'CUSTOMER' AND la.owner_id = m.account_id AND la.code = 'MARGIN_COLLATERAL'
          JOIN ledger_balances lb ON lb.ledger_account_id = la.id
         WHERE m.id = NEW.margin_account_id AND lb.balance <> 0) THEN
        RAISE EXCEPTION 'MARGIN_BANKRUPT_WITH_COLLATERAL: case % cannot be BANKRUPT while its account still holds margin collateral',
            OLD.id USING ERRCODE = 'MG010';
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

-- ---------------------------------------------------------------------------
-- 3. No interest while a person holds the case, or after bankruptcy
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION cp_margin_accrual_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lot          margin_borrow_lots%ROWTYPE;
    acct_status  text;
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
    IF NEW.amount > 0 THEN
        SELECT status INTO acct_status FROM margin_accounts WHERE id = NEW.margin_account_id;
        IF acct_status = 'BANKRUPT' OR EXISTS (SELECT 1 FROM margin_liquidation_cases
                                               WHERE margin_account_id = NEW.margin_account_id AND status = 'MANUAL_REVIEW') THEN
            RAISE EXCEPTION 'MARGIN_ACCRUAL_SUSPENDED: lot % accrues no interest while its account is % or its case is in MANUAL_REVIEW',
                NEW.lot_id, acct_status USING ERRCODE = 'MG007';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_accrual_guard() SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 4. Repaying a covered deficit to the insurance fund
-- ---------------------------------------------------------------------------

CREATE TABLE margin_deficit_repayments (
    id                      uuid PRIMARY KEY,
    case_id                 uuid NOT NULL REFERENCES margin_liquidation_cases(id),
    margin_account_id       uuid NOT NULL REFERENCES margin_accounts(id),
    amount                  numeric(38,0) NOT NULL CHECK (amount > 0),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    intent_id               uuid NOT NULL REFERENCES economic_intents(id),
    repaid_at               timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX margin_deficit_repayments_case_idx ON margin_deficit_repayments (case_id, repaid_at);
CREATE TRIGGER margin_deficit_repayments_immutable BEFORE UPDATE OR DELETE ON margin_deficit_repayments
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_margin_deficit_repayment_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    lc      margin_liquidation_cases%ROWTYPE;
    m       margin_accounts%ROWTYPE;
    repaid  numeric(38,0);
    j       record;
    legs    record;
    eff     record;
BEGIN
    -- Serialize repayments of one case on its row: the running total below is then exact.
    SELECT * INTO lc FROM margin_liquidation_cases WHERE id = NEW.case_id FOR UPDATE;
    IF NOT FOUND OR lc.margin_account_id <> NEW.margin_account_id THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: case % is not a case of margin account %', NEW.case_id, NEW.margin_account_id
            USING ERRCODE = 'MG011';
    END IF;
    IF lc.status <> 'BANKRUPT' OR lc.deficit_coverage_journal_transaction_id IS NULL THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: case % has no covered deficit to repay', lc.id USING ERRCODE = 'MG011';
    END IF;
    SELECT coalesce(sum(r.amount), 0) INTO repaid FROM margin_deficit_repayments r WHERE r.case_id = lc.id;
    IF repaid + NEW.amount > lc.deficit_units THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: case % owes % and has repaid %; % more would exceed the deficit',
            lc.id, lc.deficit_units, repaid, NEW.amount USING ERRCODE = 'MG011';
    END IF;
    SELECT * INTO m FROM margin_accounts WHERE id = NEW.margin_account_id;
    SELECT kind, reference_type, reference_id INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM 'MARGIN_REPAY' OR j.reference_type IS DISTINCT FROM 'margin_deficit_repayment'
       OR j.reference_id IS DISTINCT FROM NEW.id::text THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: repayment % names journal % which is not its MARGIN_REPAY',
            NEW.id, NEW.journal_transaction_id USING ERRCODE = 'MG011';
    END IF;
    SELECT count(*) AS n,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = m.account_id
                              AND la.code = 'DEFICIT' AND e.side = 'DEBIT') AS deficit_dr,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = m.account_id
                              AND la.code = 'WALLET' AND e.side = 'CREDIT') AS wallet_cr,
           count(*) FILTER (WHERE la.owner_type = 'FACILITY' AND la.owner_id = m.insurance_fund_facility_id
                              AND la.code = 'INSURANCE_FUND_CASH' AND e.side = 'DEBIT') AS cash_dr,
           count(*) FILTER (WHERE la.owner_type = 'FACILITY' AND la.owner_id = m.insurance_fund_facility_id
                              AND la.code = 'DEFICIT_RECEIVABLE' AND e.side = 'CREDIT') AS receivable_cr,
           coalesce(bool_and(e.asset_id = m.numeraire_asset_id AND e.quantity = NEW.amount), false) AS exact
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF legs.n <> 4 OR legs.deficit_dr <> 1 OR legs.wallet_cr <> 1 OR legs.cash_dr <> 1 OR legs.receivable_cr <> 1
       OR NOT legs.exact THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: journal % does not repay exactly % of the deficit to fund %',
            NEW.journal_transaction_id, NEW.amount, m.insurance_fund_facility_id USING ERRCODE = 'MG011';
    END IF;
    SELECT status, subsystem, journal_transaction_id, intent_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'MARGIN'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id OR eff.intent_id IS DISTINCT FROM NEW.intent_id THEN
        RAISE EXCEPTION 'MARGIN_DEFICIT_REPAYMENT: repayment % names effect % which did not apply journal % for intent %',
            NEW.id, NEW.effect_id, NEW.journal_transaction_id, NEW.intent_id USING ERRCODE = 'MG011';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_deficit_repayment_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER margin_deficit_repayments_guard BEFORE INSERT ON margin_deficit_repayments
    FOR EACH ROW EXECUTE FUNCTION cp_margin_deficit_repayment_guard();

-- ---------------------------------------------------------------------------
-- 5. An APPLIED margin effect names the journal its own key posted
-- ---------------------------------------------------------------------------

-- The journal kind and reference type each margin effect kind posts. internal/margin's
-- EffectJournalShapes is the Go twin; TestIntegration_SQLAndGoAgreeOnEffectJournalShapes compares them.
-- +goose StatementBegin
CREATE FUNCTION cp_margin_effect_journal_shape(p_effect_kind text, OUT journal_kind text, OUT reference_type text)
LANGUAGE sql IMMUTABLE AS $$
    SELECT k.journal_kind, k.reference_type FROM (VALUES
        ('COLLATERAL_POST',     'MARGIN_COLLATERAL_MOVE', 'margin_collateral_move'),
        ('COLLATERAL_RELEASE',  'MARGIN_COLLATERAL_MOVE', 'margin_collateral_move'),
        ('BORROW',              'MARGIN_BORROW',          'margin_borrow_lot'),
        ('REPAY',               'MARGIN_REPAY',           'margin_repayment'),
        ('INTEREST',            'MARGIN_INTEREST',        'margin_interest_accrual'),
        ('SPOT_BORROW',         'MARGIN_BORROW',          'margin_borrow_lot'),
        ('SPOT_TRADE',          'SIM_TRADE',              'margin_spot_trade'),
        ('SPOT_COLLATERAL',     'MARGIN_COLLATERAL_MOVE', 'margin_collateral_move'),
        ('LIQUIDATION',         'LIQUIDATION',            'margin_liquidation_step'),
        ('LIQUIDATION_RELEASE', 'MARGIN_COLLATERAL_MOVE', 'margin_liquidation_step'),
        ('LIQUIDATION_REPAY',   'MARGIN_REPAY',           'margin_liquidation_step'),
        ('LIQUIDATION_EXCESS',  'MARGIN_COLLATERAL_MOVE', 'margin_liquidation_step'),
        ('LIQUIDATION_SWEEP',   'MARGIN_REPAY',           'margin_liquidation_step'),
        ('DEFICIT_COVERAGE',    'DEFICIT_COVERAGE',       'margin_liquidation_case'),
        ('DEFICIT_REPAY',       'MARGIN_REPAY',           'margin_deficit_repayment'),
        ('FACILITY_FUNDING',    'FACILITY_FUNDING',       'margin_facility_funding')
    ) AS k(effect_kind, journal_kind, reference_type)
    WHERE k.effect_kind = p_effect_kind;
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_margin_effect_journal_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    shape  record;
    j      record;
    owners record;
BEGIN
    SELECT * INTO shape FROM cp_margin_effect_journal_shape(NEW.effect_kind);
    IF shape.journal_kind IS NULL THEN
        RAISE EXCEPTION 'MARGIN_EFFECT_JOURNAL: margin effect % is of kind %, which no margin posting produces',
            NEW.id, NEW.effect_kind USING ERRCODE = 'MG008';
    END IF;
    SELECT kind, reference_type, idempotency_key, reversal_of INTO j
      FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    -- Every margin posting is made under its effect's key, so the effect names that journal and no other.
    IF j.kind IS DISTINCT FROM shape.journal_kind OR j.reference_type IS DISTINCT FROM shape.reference_type
       OR j.idempotency_key IS DISTINCT FROM NEW.effect_key OR j.reversal_of IS NOT NULL THEN
        RAISE EXCEPTION 'MARGIN_EFFECT_JOURNAL: margin effect % (%) names journal % which is not the % / % its key posted',
            NEW.id, NEW.effect_kind, NEW.journal_transaction_id, shape.journal_kind, shape.reference_type
            USING ERRCODE = 'MG008';
    END IF;
    SELECT count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id) AS own,
           count(*) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id IS DISTINCT FROM NEW.account_id) AS other
      INTO owners
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF owners.other > 0 OR (NEW.account_id IS NOT NULL AND owners.own = 0) THEN
        RAISE EXCEPTION 'MARGIN_EFFECT_JOURNAL: journal % of margin effect % moves another customer''s books',
            NEW.journal_transaction_id, NEW.id USING ERRCODE = 'MG008';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_margin_effect_journal_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER economic_effects_margin_journal_matches
    AFTER INSERT OR UPDATE OF status, journal_transaction_id ON economic_effects
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    WHEN (NEW.subsystem = 'MARGIN' AND NEW.status = 'APPLIED')
    EXECUTE FUNCTION cp_margin_effect_journal_matches();

-- ---------------------------------------------------------------------------
-- 6. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON margin_case_resolutions, margin_case_resolution_decisions, margin_deficit_repayments TO cp_app;
GRANT UPDATE (budget_from_step) ON margin_liquidation_cases TO cp_app;
GRANT SELECT ON margin_case_resolutions, margin_case_resolution_decisions, margin_deficit_repayments TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: margin history backs journal transactions and is never dropped
