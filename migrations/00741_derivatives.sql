-- +goose Up
-- Futures, perpetuals and options in PAPER / SANDBOX mode (internal/derivatives; ADVANCED_FINANCIAL_MODEL §5,
-- goal §7; ADR-0020).
--
-- The counterparty of every customer derivative position is the CLEARING_HOUSE facility (novation): every
-- cash flow is customer <-> clearing house and is posted with one of the simulated-only journal kinds of
-- 00731. Positions are contracts tracked in these tables, never ledger assets; only cash flows reach the
-- ledger.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A position's quantity is always the sum of its fills. A fill is every quantity change: a paper or
--      book trade, a holder exercise, an assignment, an expiry. Neither side can be written without the
--      other (DV001, checked at COMMIT from both tables).
--   2. A position's identity never changes (DV002); its status moves only OPEN -> CLOSED or OPEN -> SETTLED
--      (DV003), and every status change carries a transition row naming both endpoints (AU001, 00726).
--      An OPEN position holds contracts; a flat one holds none, carries no margin and no cost.
--   3. One OPEN position per (account, instrument).
--   4. Every customer <-> clearing-house cash flow is in derivative_cash_flows, the register the clearing
--      house's books must agree with, and the journal transaction a flow names moves the clearing house's
--      CLEARING_RESULT by exactly the flow's amount, under the journal kind the flow's kind requires (DV004).
--      A posting that moves CLEARING_RESULT without a register row is what reconciliation kind CLEARING
--      finds.
--   5. A trade (PAPER or BOOK fill) cannot exist without the Risk V2 decision that allowed it.
--   6. Exactly once, by construction: one settlement per instrument, one variation-margin run and one
--      funding rate per (instrument, interval start), one funding accrual per (position, interval start),
--      one fill per fill key, one cash flow per flow key, one cash flow per journal transaction.
--   7. Every history table is append-only.
--
-- Custom SQLSTATEs: DV001 position quantity disagrees with its fills, DV002 position identity changed,
-- DV003 illegal position status transition, DV004 cash flow disagrees with the journal transaction it names.

-- ---------------------------------------------------------------------------
-- 1. Marks: mark and index per instrument, append-only
-- ---------------------------------------------------------------------------

CREATE TABLE derivative_marks (
    id              uuid PRIMARY KEY,
    instrument_id   uuid NOT NULL REFERENCES instruments(id),
    -- The mark at the instrument's price scale, on its tick grid (Go refuses anything else).
    mark_mantissa   numeric(38,0) NOT NULL CHECK (mark_mantissa >= 0),
    mark_scale      integer NOT NULL CHECK (mark_scale BETWEEN 0 AND 18),
    -- The index: the underlying's spot price (valuation asset_prices when available), at its own scale.
    index_mantissa  numeric(38,0) CHECK (index_mantissa IS NULL OR index_mantissa > 0),
    index_scale     integer CHECK (index_scale IS NULL OR index_scale BETWEEN 0 AND 38),
    index_source    text CHECK (index_source IS NULL OR length(index_source) BETWEEN 1 AND 200),
    source          text NOT NULL CHECK (length(source) BETWEEN 1 AND 100),
    observed_at     timestamptz NOT NULL,
    recorded_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, source, observed_at),
    CONSTRAINT derivative_marks_index_complete CHECK (
        (index_mantissa IS NULL) = (index_scale IS NULL) AND (index_mantissa IS NULL) = (index_source IS NULL))
);
CREATE INDEX derivative_marks_latest_idx ON derivative_marks (instrument_id, observed_at DESC, id DESC);
CREATE TRIGGER derivative_marks_immutable BEFORE UPDATE OR DELETE ON derivative_marks
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 2. Positions
-- ---------------------------------------------------------------------------

CREATE TABLE derivative_positions (
    id                   uuid PRIMARY KEY,
    account_id           uuid NOT NULL REFERENCES accounts(id),
    instrument_id        uuid NOT NULL REFERENCES instruments(id),
    settlement_asset_id  uuid NOT NULL REFERENCES assets(id),
    status               text NOT NULL CHECK (status IN ('OPEN','CLOSED','SETTLED')),
    -- Signed contracts: long > 0, short < 0.
    quantity             numeric(38,0) NOT NULL,
    -- Signed entry value of the open contracts in settlement-asset base units (Σ q × contract value at
    -- the fill price). For an option it is the premium paid (> 0) or received (< 0).
    cost_value           numeric(38,0) NOT NULL,
    -- Signed variation-margin baseline: the value at which the open contracts were last settled.
    settled_value        numeric(38,0) NOT NULL,
    realized_pnl         numeric(38,0) NOT NULL DEFAULT 0,
    funding_pnl          numeric(38,0) NOT NULL DEFAULT 0,
    -- Initial margin held in the customer's DERIVATIVES_MARGIN account for this position.
    margin_posted        numeric(38,0) NOT NULL DEFAULT 0 CHECK (margin_posted >= 0),
    -- Obligations the customer could not pay and the clearing house absorbed.
    shortfall            numeric(38,0) NOT NULL DEFAULT 0 CHECK (shortfall >= 0),
    version              bigint NOT NULL DEFAULT 1 CHECK (version >= 1),
    opened_at            timestamptz NOT NULL,
    closed_at            timestamptz,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT derivative_positions_open_holds_contracts CHECK ((status = 'OPEN') = (quantity <> 0)),
    CONSTRAINT derivative_positions_closed_at_iff_flat CHECK ((status = 'OPEN') = (closed_at IS NULL)),
    CONSTRAINT derivative_positions_flat_is_empty CHECK (
        status = 'OPEN' OR (margin_posted = 0 AND cost_value = 0 AND settled_value = 0))
);
CREATE UNIQUE INDEX derivative_positions_one_open ON derivative_positions (account_id, instrument_id) WHERE status = 'OPEN';
CREATE INDEX derivative_positions_instrument_idx ON derivative_positions (instrument_id, status, opened_at, id);
CREATE INDEX derivative_positions_account_idx ON derivative_positions (account_id, status);
CREATE TRIGGER derivative_positions_updated_at BEFORE UPDATE ON derivative_positions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_position_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (('OPEN','CLOSED'), ('OPEN','SETTLED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_position_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.account_id, NEW.instrument_id, NEW.settlement_asset_id, NEW.opened_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.instrument_id, OLD.settlement_asset_id, OLD.opened_at, OLD.created_at) THEN
        RAISE EXCEPTION 'DERIVATIVE_POSITION_IDENTITY_FROZEN: position % may not change identity', OLD.id
            USING ERRCODE = 'DV002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_derivative_position_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'DERIVATIVE_POSITION_TRANSITION_ILLEGAL: position % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'DV003';
    END IF;
    -- A flat position is history. Only funding owed for an interval it was open in (and what of that
    -- could not be collected) can still be booked against it.
    IF OLD.status <> 'OPEN'
       AND (NEW.status, NEW.quantity, NEW.cost_value, NEW.settled_value, NEW.realized_pnl, NEW.margin_posted, NEW.closed_at)
           IS DISTINCT FROM
           (OLD.status, OLD.quantity, OLD.cost_value, OLD.settled_value, OLD.realized_pnl, OLD.margin_posted, OLD.closed_at) THEN
        RAISE EXCEPTION 'DERIVATIVE_POSITION_TRANSITION_ILLEGAL: position % is % and can no longer change', OLD.id, OLD.status
            USING ERRCODE = 'DV003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_position_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_positions_guard BEFORE UPDATE ON derivative_positions
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_position_guard();
CREATE TRIGGER derivative_positions_no_delete BEFORE DELETE ON derivative_positions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE derivative_position_transitions (
    id            uuid PRIMARY KEY,
    position_id   uuid NOT NULL REFERENCES derivative_positions(id),
    from_status   text NOT NULL CHECK (from_status IN ('OPEN','CLOSED','SETTLED')),
    to_status     text NOT NULL CHECK (to_status IN ('OPEN','CLOSED','SETTLED')),
    actor_type    text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id      text NOT NULL,
    reason        text NOT NULL CHECK (length(reason) > 0),
    occurred_at   timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX derivative_position_transitions_idx ON derivative_position_transitions (position_id, occurred_at);
CREATE TRIGGER derivative_position_transitions_immutable BEFORE UPDATE OR DELETE ON derivative_position_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER derivative_position_transitions_flag_edge AFTER INSERT ON derivative_position_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('position_id', 'from_status', 'to_status', 'deriv_position_status');
CREATE CONSTRAINT TRIGGER derivative_positions_require_transition AFTER UPDATE OF status ON derivative_positions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'deriv_position_status');

-- ---------------------------------------------------------------------------
-- 3. Fills: every quantity change of a position
-- ---------------------------------------------------------------------------

CREATE TABLE derivative_fills (
    id                 uuid PRIMARY KEY,
    -- Deterministic per fill: 'intent:<intent id>' for a paper fill, the match id for a book fill,
    -- '<exercise>:<position>' for an exercise or assignment, 'settlement:<id>:<position>' at expiry.
    fill_key           text NOT NULL UNIQUE CHECK (length(fill_key) BETWEEN 1 AND 200),
    position_id        uuid NOT NULL REFERENCES derivative_positions(id),
    account_id         uuid NOT NULL REFERENCES accounts(id),
    instrument_id      uuid NOT NULL REFERENCES instruments(id),
    source             text NOT NULL CHECK (source IN ('PAPER','BOOK','EXERCISE','ASSIGNMENT','EXPIRY')),
    side               text NOT NULL CHECK (side IN ('BUY','SELL')),
    -- Signed contracts: a BUY is positive, a SELL negative.
    quantity           numeric(38,0) NOT NULL,
    -- The price the contracts changed hands at, at the instrument's price scale. For an option exercise,
    -- assignment or expiry it is the intrinsic value per unit (the underlying settlement price is on the
    -- exercise or settlement row); for a future's expiry, the final settlement price.
    price_mantissa     numeric(38,0) NOT NULL CHECK (price_mantissa >= 0),
    price_scale        integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    -- Signed quantity × contract value at the price, in settlement-asset base units.
    value              numeric(38,0) NOT NULL,
    -- Economic profit or loss against the entry cost of the contracts this fill closed.
    realized_pnl       numeric(38,0) NOT NULL DEFAULT 0,
    -- Cash this fill moved between the customer and the clearing house (> 0 received).
    cash_settled       numeric(38,0) NOT NULL DEFAULT 0,
    -- Disclosure of a paper fill: the mark it was priced from and the slippage applied.
    mark_id            uuid REFERENCES derivative_marks(id),
    slippage_bps       bigint NOT NULL DEFAULT 0 CHECK (slippage_bps >= 0),
    intent_id          uuid REFERENCES economic_intents(id),
    risk_decision_id   uuid REFERENCES advanced_risk_decisions(id),
    occurred_at        timestamptz NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT derivative_fills_side_matches_sign CHECK ((side = 'BUY') = (quantity > 0) AND quantity <> 0),
    -- A trade cannot exist without the risk decision that allowed it.
    CONSTRAINT derivative_fills_trades_carry_a_decision CHECK (source NOT IN ('PAPER','BOOK') OR risk_decision_id IS NOT NULL)
);
CREATE INDEX derivative_fills_position_idx ON derivative_fills (position_id, occurred_at, id);
CREATE INDEX derivative_fills_intent_idx ON derivative_fills (intent_id) WHERE intent_id IS NOT NULL;
CREATE TRIGGER derivative_fills_immutable BEFORE UPDATE OR DELETE ON derivative_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A position's quantity is the sum of its fills. Checked at COMMIT from both sides, so the order of the
-- two writes inside a transaction does not matter and neither can be made without the other.
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_position_matches_fills() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    pid     uuid;
    held    numeric;
    filled  numeric;
BEGIN
    IF TG_TABLE_NAME = 'derivative_fills' THEN
        pid := NEW.position_id;
    ELSE
        pid := NEW.id;
    END IF;
    SELECT quantity INTO held FROM derivative_positions WHERE id = pid;
    SELECT coalesce(sum(quantity), 0) INTO filled FROM derivative_fills WHERE position_id = pid;
    IF held IS DISTINCT FROM filled THEN
        RAISE EXCEPTION 'DERIVATIVE_POSITION_UNEXPLAINED: position % holds % contracts and its fills sum to %',
            pid, held, filled USING ERRCODE = 'DV001';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_position_matches_fills() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER derivative_positions_match_fills AFTER INSERT OR UPDATE OF quantity ON derivative_positions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_derivative_position_matches_fills();
CREATE CONSTRAINT TRIGGER derivative_fills_match_position AFTER INSERT ON derivative_fills
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_derivative_position_matches_fills();

-- ---------------------------------------------------------------------------
-- 4. The cash-flow register: every customer <-> clearing house flow
-- ---------------------------------------------------------------------------

CREATE TABLE derivative_cash_flows (
    id                      uuid PRIMARY KEY,
    flow_key                text NOT NULL UNIQUE CHECK (length(flow_key) BETWEEN 1 AND 300),
    position_id             uuid NOT NULL REFERENCES derivative_positions(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    instrument_id           uuid NOT NULL REFERENCES instruments(id),
    settlement_asset_id     uuid NOT NULL REFERENCES assets(id),
    clearing_house_id       uuid NOT NULL REFERENCES facilities(id),
    kind                    text NOT NULL CHECK (kind IN ('REALIZED','VARIATION_MARGIN','FUNDING','FUTURE_SETTLEMENT',
                                                          'OPTION_PREMIUM','OPTION_EXERCISE','OPTION_ASSIGNMENT')),
    -- Signed, from the customer's side: > 0 received from the clearing house, < 0 paid to it. It is what
    -- actually moved; an obligation the customer could not pay is the shortfall.
    amount                  numeric(38,0) NOT NULL,
    shortfall               numeric(38,0) NOT NULL DEFAULT 0 CHECK (shortfall >= 0),
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    effect_id               uuid UNIQUE REFERENCES economic_effects(id),
    reference_type          text NOT NULL CHECK (length(reference_type) BETWEEN 1 AND 50),
    reference_id            text NOT NULL CHECK (length(reference_id) BETWEEN 1 AND 100),
    occurred_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT derivative_cash_flows_posted_iff_nonzero CHECK ((amount = 0) = (journal_transaction_id IS NULL)),
    CONSTRAINT derivative_cash_flows_effect_iff_journal CHECK ((journal_transaction_id IS NULL) = (effect_id IS NULL)),
    CONSTRAINT derivative_cash_flows_only_payments_fall_short CHECK (shortfall = 0 OR amount <= 0),
    CONSTRAINT derivative_cash_flows_something_happened CHECK (amount <> 0 OR shortfall > 0)
);
CREATE INDEX derivative_cash_flows_asset_idx ON derivative_cash_flows (clearing_house_id, settlement_asset_id);
CREATE INDEX derivative_cash_flows_position_idx ON derivative_cash_flows (position_id, occurred_at);
CREATE TRIGGER derivative_cash_flows_immutable BEFORE UPDATE OR DELETE ON derivative_cash_flows
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The journal kind each cash-flow kind is posted with (internal/derivatives JournalKindFor; a parity test
-- compares the two).
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_cash_flow_journal_kind(p_kind text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
    SELECT CASE p_kind
        WHEN 'REALIZED' THEN 'VARIATION_MARGIN'
        WHEN 'VARIATION_MARGIN' THEN 'VARIATION_MARGIN'
        WHEN 'FUNDING' THEN 'FUNDING_PAYMENT'
        WHEN 'FUTURE_SETTLEMENT' THEN 'FUTURE_SETTLEMENT'
        WHEN 'OPTION_PREMIUM' THEN 'OPTION_PREMIUM'
        WHEN 'OPTION_EXERCISE' THEN 'OPTION_SETTLEMENT'
        WHEN 'OPTION_ASSIGNMENT' THEN 'OPTION_SETTLEMENT'
        ELSE NULL END;
$$;
-- +goose StatementEnd

-- The journal a flow names moves the clearing house's CLEARING_RESULT by exactly -amount (the clearing
-- house's side of the flow), in the flow's asset, under the journal kind the flow's kind requires.
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_cash_flow_matches_journal() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    jkind  text;
    net    numeric;
BEGIN
    IF NEW.journal_transaction_id IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT kind INTO jkind FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF jkind IS DISTINCT FROM cp_derivative_cash_flow_journal_kind(NEW.kind) THEN
        RAISE EXCEPTION 'DERIVATIVE_CASH_FLOW_JOURNAL_MISMATCH: % flow % names a % journal, expected %',
            NEW.kind, NEW.id, coalesce(jkind, 'missing'), cp_derivative_cash_flow_journal_kind(NEW.kind)
            USING ERRCODE = 'DV004';
    END IF;
    SELECT coalesce(sum(CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE -e.quantity END), 0) INTO net
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'FACILITY' AND la.owner_id = NEW.clearing_house_id
       AND la.code = 'CLEARING_RESULT' AND e.asset_id = NEW.settlement_asset_id;
    IF net <> -NEW.amount THEN
        RAISE EXCEPTION 'DERIVATIVE_CASH_FLOW_JOURNAL_MISMATCH: flow % of % moves CLEARING_RESULT by % in journal %',
            NEW.id, NEW.amount, net, NEW.journal_transaction_id USING ERRCODE = 'DV004';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_cash_flow_matches_journal() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER derivative_cash_flows_match_journal AFTER INSERT ON derivative_cash_flows
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_derivative_cash_flow_matches_journal();

-- ---------------------------------------------------------------------------
-- 5. Variation margin runs, final settlements
-- ---------------------------------------------------------------------------

CREATE TABLE variation_margin_runs (
    id               uuid PRIMARY KEY,
    instrument_id    uuid NOT NULL REFERENCES instruments(id),
    interval_start   timestamptz NOT NULL,
    interval_end     timestamptz NOT NULL,
    mark_id          uuid NOT NULL REFERENCES derivative_marks(id),
    mark_mantissa    numeric(38,0) NOT NULL CHECK (mark_mantissa >= 0),
    positions        integer NOT NULL CHECK (positions >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, interval_start),
    CHECK (interval_end > interval_start)
);
CREATE TRIGGER variation_margin_runs_immutable BEFORE UPDATE OR DELETE ON variation_margin_runs
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE derivative_settlements (
    id                    uuid PRIMARY KEY,
    -- Once per instrument.
    instrument_id         uuid NOT NULL UNIQUE REFERENCES instruments(id),
    kind                  text NOT NULL CHECK (kind IN ('FUTURE','OPTION')),
    settlement_mantissa   numeric(38,0) NOT NULL CHECK (settlement_mantissa >= 0),
    price_scale           integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    price_source          text NOT NULL CHECK (length(price_source) BETWEEN 1 AND 200),
    positions             integer NOT NULL CHECK (positions >= 0),
    settled_at            timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER derivative_settlements_immutable BEFORE UPDATE OR DELETE ON derivative_settlements
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 6. Perpetual funding
-- ---------------------------------------------------------------------------

CREATE TABLE funding_rates (
    id                    uuid PRIMARY KEY,
    instrument_id         uuid NOT NULL REFERENCES instruments(id),
    interval_start        timestamptz NOT NULL,
    interval_end          timestamptz NOT NULL,
    samples               integer NOT NULL CHECK (samples > 0),
    -- Rates in units of 10^-8 (1 bp = 10 000), rounded half-even from the exact rational premium index.
    premium_index_e8      bigint NOT NULL,
    rate_e8               bigint NOT NULL,
    clamped               boolean NOT NULL,
    mark_id               uuid NOT NULL REFERENCES derivative_marks(id),
    mark_mantissa         numeric(38,0) NOT NULL CHECK (mark_mantissa >= 0),
    -- What one contract pays or receives for the interval, in settlement-asset base units.
    per_contract_amount   numeric(38,0) NOT NULL CHECK (per_contract_amount >= 0),
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, interval_start),
    CHECK (interval_end > interval_start),
    CHECK (clamped OR rate_e8 = premium_index_e8)
);
CREATE TRIGGER funding_rates_immutable BEFORE UPDATE OR DELETE ON funding_rates
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE funding_accruals (
    id                uuid PRIMARY KEY,
    funding_rate_id   uuid NOT NULL REFERENCES funding_rates(id),
    position_id       uuid NOT NULL REFERENCES derivative_positions(id),
    interval_start    timestamptz NOT NULL,
    -- The position's signed quantity at the funding time (the interval end).
    quantity          numeric(38,0) NOT NULL CHECK (quantity <> 0),
    direction         text NOT NULL CHECK (direction IN ('PAY','RECEIVE')),
    amount            numeric(38,0) NOT NULL CHECK (amount >= 0),
    cash_flow_id      uuid REFERENCES derivative_cash_flows(id),
    created_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (position_id, interval_start),
    CHECK ((amount = 0) = (cash_flow_id IS NULL))
);
CREATE INDEX funding_accruals_rate_idx ON funding_accruals (funding_rate_id);
CREATE TRIGGER funding_accruals_immutable BEFORE UPDATE OR DELETE ON funding_accruals
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 7. Option exercise and assignment
-- ---------------------------------------------------------------------------

CREATE TABLE option_exercises (
    id                       uuid PRIMARY KEY,
    instrument_id            uuid NOT NULL REFERENCES instruments(id),
    -- NULL: the clearing house exercising the net long it carries for a one-sided book.
    position_id              uuid REFERENCES derivative_positions(id),
    account_id               uuid REFERENCES accounts(id),
    kind                     text NOT NULL CHECK (kind IN ('HOLDER','AUTOMATIC')),
    quantity                 numeric(38,0) NOT NULL CHECK (quantity > 0),
    settlement_mantissa      numeric(38,0) NOT NULL CHECK (settlement_mantissa >= 0),
    intrinsic_per_contract   numeric(38,0) NOT NULL CHECK (intrinsic_per_contract > 0),
    intent_id                uuid REFERENCES economic_intents(id),
    settlement_id            uuid REFERENCES derivative_settlements(id),
    occurred_at              timestamptz NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    CHECK ((position_id IS NULL) = (account_id IS NULL)),
    CHECK (kind <> 'HOLDER' OR (position_id IS NOT NULL AND intent_id IS NOT NULL)),
    CHECK ((kind = 'AUTOMATIC') = (settlement_id IS NOT NULL))
);
CREATE INDEX option_exercises_instrument_idx ON option_exercises (instrument_id, occurred_at);
CREATE UNIQUE INDEX option_exercises_one_per_intent ON option_exercises (intent_id) WHERE intent_id IS NOT NULL;
CREATE UNIQUE INDEX option_exercises_once_per_settlement ON option_exercises (settlement_id, position_id)
    WHERE settlement_id IS NOT NULL AND position_id IS NOT NULL;
CREATE TRIGGER option_exercises_immutable BEFORE UPDATE OR DELETE ON option_exercises
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE option_assignments (
    id                  uuid PRIMARY KEY,
    exercise_id         uuid NOT NULL REFERENCES option_exercises(id),
    seq                 integer NOT NULL CHECK (seq >= 0),
    -- NULL: assigned to the clearing house, the writer of record for a one-sided book.
    short_position_id   uuid REFERENCES derivative_positions(id),
    account_id          uuid REFERENCES accounts(id),
    quantity            numeric(38,0) NOT NULL CHECK (quantity > 0),
    amount              numeric(38,0) NOT NULL CHECK (amount >= 0),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (exercise_id, seq),
    CHECK ((short_position_id IS NULL) = (account_id IS NULL))
);
CREATE TRIGGER option_assignments_immutable BEFORE UPDATE OR DELETE ON option_assignments
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 8. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON derivative_marks, derivative_positions, derivative_position_transitions, derivative_fills,
    derivative_cash_flows, variation_margin_runs, derivative_settlements, funding_rates, funding_accruals,
    option_exercises, option_assignments TO cp_app;
GRANT UPDATE (status, quantity, cost_value, settled_value, realized_pnl, funding_pnl, margin_posted, shortfall,
              version, closed_at) ON derivative_positions TO cp_app;
GRANT SELECT ON derivative_marks, derivative_positions, derivative_position_transitions, derivative_fills,
    derivative_cash_flows, variation_margin_runs, derivative_settlements, funding_rates, funding_accruals,
    option_exercises, option_assignments TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: positions, fills and the cash-flow register are financial history
