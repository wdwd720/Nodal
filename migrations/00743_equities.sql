-- +goose Up
-- Equities and ETFs in PAPER and SANDBOX mode (ADR-0020; docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5;
-- internal/equities).
--
-- An equity or ETF is an instruments row (kind EQUITY or ETF, 00732) whose shares are one SIMULATED asset
-- held in customer WALLET accounts. The ledger already guarantees what moves value: SIM_TRADE,
-- EQUITY_SETTLEMENT and CORPORATE_ACTION are simulated-only kinds that balance per party (SM001/SM002) and
-- no WALLET goes negative (LG001). This migration adds the equities registers and makes their own
-- invariants true in the database, independent of any Go code:
--
--   1. An equity order's status moves only along its legal edges (EQ001) and only with a transition row
--      naming both endpoints (AU001, 00726). Everything but its status, its rejection and its broker order
--      id is frozen (EQ002); the rejection is written once, with the move to REJECTED; the broker order id
--      is written once.
--   2. A fill is immutable, one per order, and it agrees with its order: same account, instrument, side and
--      quantity (no overfill), a price on the order's tick grid that never goes through the order's limit
--      or protection price, executed inside the order's life, and only for an ACCEPTED or OPEN order (EQ003).
--      Its booked charges add up to its total (CHECK).
--   3. A settlement is one per fill. It settles once, together with its journal transaction, and never
--      changes afterwards (EQ004); its identity is frozen (EQ002).
--   4. A corporate action's status moves only along its legal edges (EQ005) with a bound transition row;
--      its terms are frozen (EQ002). One action per (instrument, type, effective date). Each type carries
--      exactly the fields it needs.
--   5. A corporate action is applied to an account at most once (UNIQUE), and an application is immutable
--      and carries its journal transaction exactly when it moved value.
--   6. Calendar holidays are append-only weekdays, one per (calendar, date).
--
-- Custom SQLSTATEs: EQ001 illegal equity order transition, EQ002 equity record identity changed, EQ003 fill
-- contradicts its order, EQ004 settlement changed after settling, EQ005 illegal corporate action transition.

-- ---------------------------------------------------------------------------
-- 1. Orders
-- ---------------------------------------------------------------------------

CREATE TABLE equity_orders (
    id                  uuid PRIMARY KEY,
    account_id          uuid NOT NULL REFERENCES accounts(id),
    instrument_id       uuid NOT NULL REFERENCES instruments(id),
    intent_id           uuid NOT NULL UNIQUE REFERENCES economic_intents(id),
    share_asset_id      uuid NOT NULL REFERENCES assets(id),
    cash_asset_id       uuid NOT NULL REFERENCES assets(id),
    symbol              text NOT NULL CHECK (length(symbol) BETWEEN 1 AND 64),
    exchange_mic        text NOT NULL CHECK (exchange_mic ~ '^[A-Z0-9]{4}$'),
    terms_version       integer NOT NULL CHECK (terms_version >= 1),
    side                text NOT NULL CHECK (side IN ('BUY','SELL')),
    order_type          text NOT NULL CHECK (order_type IN ('MARKET','LIMIT')),
    -- LIVE is absent on purpose, as for every advanced capability.
    mode                text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    quantity            numeric(38,0) NOT NULL CHECK (quantity > 0),
    quantity_decimals   smallint NOT NULL CHECK (quantity_decimals BETWEEN 0 AND 18),
    price_scale         integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    tick_size           numeric(38,0) NOT NULL CHECK (tick_size > 0),
    limit_price         numeric(38,0) CHECK (limit_price > 0),
    protection_price    numeric(38,0) CHECK (protection_price > 0),
    extended_hours      boolean NOT NULL,
    session             text NOT NULL CHECK (session IN ('CLOSED','PRE_MARKET','REGULAR','AFTER_HOURS')),
    trade_date          date NOT NULL,
    expires_at          timestamptz NOT NULL,
    status              text NOT NULL CHECK (status IN ('ACCEPTED','REJECTED','OPEN','FILLED','CANCELLED','EXPIRED')),
    rejection_code      text CHECK (rejection_code IN (
                            'CAPABILITY_NOT_APPROVED','KILL_SWITCH_ACTIVE','RISK_REJECTED','INSUFFICIENT_BUYING_POWER',
                            'EQUITIES_QUANTITY_BELOW_MINIMUM','EQUITIES_QUANTITY_NOT_LOT_MULTIPLE','EQUITIES_LIMIT_PRICE_INVALID',
                            'EQUITIES_NO_MARK','EQUITIES_NOTIONAL_TOO_SMALL','EQUITIES_CORPORATE_ACTION_PENDING','VENUE_REJECTED')),
    rejection_reasons   text[] NOT NULL DEFAULT '{}',
    reservation_id      uuid,
    reserved_quantity   numeric(38,0) CHECK (reserved_quantity > 0),
    risk_decision_id    uuid REFERENCES advanced_risk_decisions(id),
    broker              text NOT NULL CHECK (length(broker) BETWEEN 1 AND 100),
    broker_order_id     text,
    actor_type          text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','AGENT','SYSTEM')),
    actor_id            text NOT NULL CHECK (length(actor_id) > 0),
    correlation_id      text,
    created_at          timestamptz NOT NULL,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    -- A limit order carries its limit. One refused because its limit is not a price on the
    -- instrument's grid carries none: the requested price stays on its economic intent.
    CONSTRAINT equity_orders_limit_shape CHECK (
        (order_type = 'MARKET' AND limit_price IS NULL)
        OR (order_type = 'LIMIT' AND (limit_price IS NOT NULL OR status = 'REJECTED'))),
    CONSTRAINT equity_orders_protection_shape CHECK (
        (order_type = 'LIMIT' AND protection_price IS NULL)
        OR (order_type = 'MARKET' AND (protection_price IS NOT NULL OR reservation_id IS NULL))),
    -- A market order never trades outside the regular session.
    CONSTRAINT equity_orders_market_is_regular_only CHECK (order_type = 'LIMIT' OR NOT extended_hours),
    CONSTRAINT equity_orders_rejected_has_code CHECK ((status = 'REJECTED') = (rejection_code IS NOT NULL)),
    -- Every order that ever went to the venue is backed by a capital reservation.
    CONSTRAINT equity_orders_accepted_is_reserved CHECK (
        status = 'REJECTED' OR (reservation_id IS NOT NULL AND reserved_quantity IS NOT NULL)),
    -- A refused order may have been refused because its session had already closed.
    CONSTRAINT equity_orders_expiry_after_creation CHECK (status = 'REJECTED' OR expires_at > created_at)
);
CREATE INDEX equity_orders_account_idx ON equity_orders (account_id, created_at DESC, id);
CREATE INDEX equity_orders_live_idx ON equity_orders (instrument_id, status) WHERE status IN ('ACCEPTED','OPEN');
CREATE INDEX equity_orders_expiry_idx ON equity_orders (expires_at) WHERE status IN ('ACCEPTED','OPEN');
CREATE TRIGGER equity_orders_updated_at BEFORE UPDATE ON equity_orders FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_equity_order_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('ACCEPTED','OPEN'), ('ACCEPTED','FILLED'), ('ACCEPTED','REJECTED'), ('ACCEPTED','CANCELLED'), ('ACCEPTED','EXPIRED'),
        ('OPEN','FILLED'), ('OPEN','CANCELLED'), ('OPEN','EXPIRED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_equity_order_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.account_id, NEW.instrument_id, NEW.intent_id, NEW.share_asset_id, NEW.cash_asset_id, NEW.symbol,
        NEW.exchange_mic, NEW.terms_version, NEW.side, NEW.order_type, NEW.mode, NEW.quantity, NEW.quantity_decimals,
        NEW.price_scale, NEW.tick_size, NEW.limit_price, NEW.protection_price, NEW.extended_hours, NEW.session,
        NEW.trade_date, NEW.expires_at, NEW.reservation_id, NEW.reserved_quantity, NEW.risk_decision_id, NEW.broker,
        NEW.actor_type, NEW.actor_id, NEW.correlation_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.instrument_id, OLD.intent_id, OLD.share_asset_id, OLD.cash_asset_id, OLD.symbol,
        OLD.exchange_mic, OLD.terms_version, OLD.side, OLD.order_type, OLD.mode, OLD.quantity, OLD.quantity_decimals,
        OLD.price_scale, OLD.tick_size, OLD.limit_price, OLD.protection_price, OLD.extended_hours, OLD.session,
        OLD.trade_date, OLD.expires_at, OLD.reservation_id, OLD.reserved_quantity, OLD.risk_decision_id, OLD.broker,
        OLD.actor_type, OLD.actor_id, OLD.correlation_id, OLD.created_at) THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: equity order % may change only its status, rejection and broker order id', OLD.id
            USING ERRCODE = 'EQ002';
    END IF;
    IF OLD.broker_order_id IS NOT NULL AND NEW.broker_order_id IS DISTINCT FROM OLD.broker_order_id THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: equity order % already names broker order %', OLD.id, OLD.broker_order_id
            USING ERRCODE = 'EQ002';
    END IF;
    IF (NEW.rejection_code IS DISTINCT FROM OLD.rejection_code OR NEW.rejection_reasons IS DISTINCT FROM OLD.rejection_reasons)
       AND NOT (OLD.status <> 'REJECTED' AND NEW.status = 'REJECTED' AND OLD.rejection_code IS NULL) THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: equity order % records its rejection once, when it is rejected', OLD.id
            USING ERRCODE = 'EQ002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_equity_order_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'EQUITY_ORDER_TRANSITION_ILLEGAL: equity order % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'EQ001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_equity_order_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER equity_orders_guard BEFORE UPDATE ON equity_orders
    FOR EACH ROW EXECUTE FUNCTION cp_equity_order_guard();
CREATE TRIGGER equity_orders_no_delete BEFORE DELETE ON equity_orders
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE equity_order_transitions (
    id              uuid PRIMARY KEY,
    order_id        uuid NOT NULL REFERENCES equity_orders(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    -- An agent proposes; it never moves an order.
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    correlation_id  text,
    occurred_at     timestamptz NOT NULL
);
CREATE INDEX equity_order_transitions_idx ON equity_order_transitions (order_id, occurred_at);
CREATE TRIGGER equity_order_transitions_immutable BEFORE UPDATE OR DELETE ON equity_order_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER equity_order_transitions_flag_edge AFTER INSERT ON equity_order_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('order_id', 'from_status', 'to_status', 'equity_order_status');
CREATE CONSTRAINT TRIGGER equity_orders_require_transition AFTER UPDATE OF status ON equity_orders
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'equity_order_status');

-- ---------------------------------------------------------------------------
-- 2. Fills
-- ---------------------------------------------------------------------------

CREATE TABLE equity_fills (
    id                      uuid PRIMARY KEY,
    -- The paper venue fills an order in full, once.
    order_id                uuid NOT NULL UNIQUE REFERENCES equity_orders(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    instrument_id           uuid NOT NULL REFERENCES instruments(id),
    side                    text NOT NULL CHECK (side IN ('BUY','SELL')),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    price                   numeric(38,0) NOT NULL CHECK (price > 0),
    price_scale             integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    mark_mantissa           numeric(38,0) NOT NULL CHECK (mark_mantissa > 0),
    mark_scale              integer NOT NULL CHECK (mark_scale BETWEEN 0 AND 38),
    mark_source             text NOT NULL CHECK (length(mark_source) > 0),
    mark_observed_at        timestamptz NOT NULL,
    slippage_bps            bigint NOT NULL CHECK (slippage_bps BETWEEN 0 AND 10000),
    venue                   text NOT NULL,
    venue_fill_id           text NOT NULL,
    executed_at             timestamptz NOT NULL,
    trade_date              date NOT NULL,
    settlement_date         date NOT NULL,
    notional                numeric(38,0) NOT NULL CHECK (notional > 0),
    spread_amount           numeric(38,0) NOT NULL CHECK (spread_amount >= 0),
    platform_fees           numeric(38,0) NOT NULL CHECK (platform_fees >= 0),
    venue_fees              numeric(38,0) NOT NULL CHECK (venue_fees >= 0),
    total                   numeric(38,0) NOT NULL CHECK (total > 0),
    fee_schedule_version    text NOT NULL CHECK (length(fee_schedule_version) > 0),
    fee_schedule_hash       text NOT NULL CHECK (fee_schedule_hash ~ '^[0-9a-f]{64}$'),
    -- Exact bytes: an auditor re-verifies the disclosure from these two columns. text, not jsonb, because
    -- jsonb rewrites key order and the disclosure is verified byte for byte.
    fee_schedule            text NOT NULL CHECK (fee_schedule IS JSON OBJECT),
    fee_disclosure          text NOT NULL CHECK (fee_disclosure IS JSON OBJECT),
    fee_disclosure_hash     text NOT NULL CHECK (fee_disclosure_hash ~ '^[0-9a-f]{64}$'),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (venue, venue_fill_id),
    CONSTRAINT equity_fills_settles_after_trade CHECK (settlement_date > trade_date),
    CONSTRAINT equity_fills_charges_add_up CHECK (
        (side = 'BUY' AND total = notional + spread_amount + platform_fees + venue_fees)
        OR (side = 'SELL' AND total = notional - spread_amount - platform_fees - venue_fees))
);
CREATE INDEX equity_fills_account_idx ON equity_fills (account_id, executed_at);
CREATE INDEX equity_fills_instrument_idx ON equity_fills (instrument_id, executed_at);
CREATE TRIGGER equity_fills_immutable BEFORE UPDATE OR DELETE ON equity_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_equity_fill_matches_order() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    o equity_orders%ROWTYPE;
BEGIN
    SELECT * INTO o FROM equity_orders WHERE id = NEW.order_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % names no order', NEW.id USING ERRCODE = 'EQ003';
    END IF;
    IF o.status NOT IN ('ACCEPTED','OPEN') THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: order % is % and cannot fill', o.id, o.status USING ERRCODE = 'EQ003';
    END IF;
    IF NEW.account_id <> o.account_id OR NEW.instrument_id <> o.instrument_id OR NEW.side <> o.side
       OR NEW.price_scale <> o.price_scale THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % is for a different account, instrument, side or price scale than order %',
            NEW.id, o.id USING ERRCODE = 'EQ003';
    END IF;
    IF NEW.quantity <> o.quantity THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % quantity % is not order % quantity %', NEW.id, NEW.quantity, o.id, o.quantity
            USING ERRCODE = 'EQ003';
    END IF;
    IF mod(NEW.price, o.tick_size) <> 0 THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % price % is off order % tick %', NEW.id, NEW.price, o.id, o.tick_size
            USING ERRCODE = 'EQ003';
    END IF;
    IF (o.side = 'BUY' AND NEW.price > coalesce(o.limit_price, o.protection_price))
       OR (o.side = 'SELL' AND NEW.price < coalesce(o.limit_price, o.protection_price)) THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % price % goes through order % limit', NEW.id, NEW.price, o.id
            USING ERRCODE = 'EQ003';
    END IF;
    IF NEW.executed_at < o.created_at OR NEW.executed_at >= o.expires_at THEN
        RAISE EXCEPTION 'EQUITY_FILL_CONTRADICTS_ORDER: fill % executed outside the life of order %', NEW.id, o.id
            USING ERRCODE = 'EQ003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_equity_fill_matches_order() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER equity_fills_match_order BEFORE INSERT ON equity_fills
    FOR EACH ROW EXECUTE FUNCTION cp_equity_fill_matches_order();

-- ---------------------------------------------------------------------------
-- 3. Settlements
-- ---------------------------------------------------------------------------

CREATE TABLE equity_settlements (
    id                      uuid PRIMARY KEY,
    fill_id                 uuid NOT NULL UNIQUE REFERENCES equity_fills(id),
    order_id                uuid NOT NULL REFERENCES equity_orders(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    instrument_id           uuid NOT NULL REFERENCES instruments(id),
    -- The asset the trade delivers into WALLET at settlement: shares for a buy, net proceeds for a sell.
    asset_id                uuid NOT NULL REFERENCES assets(id),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    trade_date              date NOT NULL,
    due_date                date NOT NULL,
    settled_at              timestamptz,
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT equity_settlements_due_after_trade CHECK (due_date > trade_date),
    CONSTRAINT equity_settlements_settled_with_journal CHECK ((settled_at IS NULL) = (journal_transaction_id IS NULL))
);
CREATE INDEX equity_settlements_due_idx ON equity_settlements (due_date, id) WHERE settled_at IS NULL;
CREATE INDEX equity_settlements_instrument_idx ON equity_settlements (instrument_id) WHERE settled_at IS NULL;
CREATE TRIGGER equity_settlements_updated_at BEFORE UPDATE ON equity_settlements FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_equity_settlement_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.fill_id, NEW.order_id, NEW.account_id, NEW.instrument_id, NEW.asset_id, NEW.quantity, NEW.trade_date,
        NEW.due_date, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.fill_id, OLD.order_id, OLD.account_id, OLD.instrument_id, OLD.asset_id, OLD.quantity, OLD.trade_date,
        OLD.due_date, OLD.created_at) THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: equity settlement % may only settle', OLD.id USING ERRCODE = 'EQ002';
    END IF;
    IF OLD.settled_at IS NOT NULL AND (NEW.settled_at IS DISTINCT FROM OLD.settled_at
                                       OR NEW.journal_transaction_id IS DISTINCT FROM OLD.journal_transaction_id) THEN
        RAISE EXCEPTION 'EQUITY_SETTLEMENT_FINAL: equity settlement % settled at % with journal % and never changes',
            OLD.id, OLD.settled_at, OLD.journal_transaction_id USING ERRCODE = 'EQ004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_equity_settlement_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER equity_settlements_guard BEFORE UPDATE ON equity_settlements
    FOR EACH ROW EXECUTE FUNCTION cp_equity_settlement_guard();
CREATE TRIGGER equity_settlements_no_delete BEFORE DELETE ON equity_settlements
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 4. Corporate actions
-- ---------------------------------------------------------------------------

CREATE TABLE equity_corporate_actions (
    id                        uuid PRIMARY KEY,
    instrument_id             uuid NOT NULL REFERENCES instruments(id),
    action_type               text NOT NULL CHECK (action_type IN ('SPLIT','REVERSE_SPLIT','CASH_DIVIDEND','SYMBOL_CHANGE','DELISTING')),
    status                    text NOT NULL CHECK (status IN ('ANNOUNCED','APPLYING','APPLIED','CANCELLED')),
    -- Splits, symbol changes and delistings take effect on it; for a dividend it is the ex-date.
    effective_date            date NOT NULL,
    share_asset_id            uuid NOT NULL REFERENCES assets(id),
    cash_asset_id             uuid NOT NULL REFERENCES assets(id),
    -- New shares per old share, exactly: numerator / denominator.
    ratio_numerator           bigint CHECK (ratio_numerator BETWEEN 1 AND 1000000),
    ratio_denominator         bigint CHECK (ratio_denominator BETWEEN 1 AND 1000000),
    -- Cash per whole share (dividend) and per whole new share (cash-in-lieu): mantissa × 10^-scale.
    cash_per_share            numeric(38,0) CHECK (cash_per_share > 0),
    cash_per_share_scale      integer CHECK (cash_per_share_scale BETWEEN 0 AND 18),
    cash_in_lieu_price        numeric(38,0) CHECK (cash_in_lieu_price > 0),
    cash_in_lieu_price_scale  integer CHECK (cash_in_lieu_price_scale BETWEEN 0 AND 18),
    ex_date                   date,
    record_date               date,
    pay_date                  date,
    final_date                date,
    new_symbol                text CHECK (new_symbol ~ '^[A-Z0-9][A-Z0-9._/-]{0,63}$'),
    rounding_policy           text NOT NULL CHECK (rounding_policy IN ('SHARES_DOWN_CASH_DOWN','SHARES_DOWN_CASH_HALF_EVEN')),
    reason                    text NOT NULL CHECK (length(reason) > 0),
    content_hash              bytea NOT NULL CHECK (length(content_hash) = 32),
    announced_by_actor_type   text NOT NULL CHECK (announced_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    announced_by_actor_id     text NOT NULL,
    applied_at                timestamptz,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, action_type, effective_date),
    CONSTRAINT equity_corporate_actions_ratio_shape CHECK (
        (action_type IN ('SPLIT','REVERSE_SPLIT')) = (ratio_numerator IS NOT NULL AND ratio_denominator IS NOT NULL)),
    CONSTRAINT equity_corporate_actions_split_direction CHECK (
        (action_type <> 'SPLIT' OR ratio_numerator > ratio_denominator)
        AND (action_type <> 'REVERSE_SPLIT' OR ratio_numerator < ratio_denominator)),
    CONSTRAINT equity_corporate_actions_dividend_shape CHECK (
        (action_type = 'CASH_DIVIDEND') = (cash_per_share IS NOT NULL AND ex_date IS NOT NULL
                                           AND record_date IS NOT NULL AND pay_date IS NOT NULL)),
    CONSTRAINT equity_corporate_actions_dividend_dates CHECK (
        action_type <> 'CASH_DIVIDEND' OR (effective_date = ex_date AND ex_date <= record_date AND record_date <= pay_date)),
    CONSTRAINT equity_corporate_actions_cash_scales CHECK (
        (cash_per_share IS NULL) = (cash_per_share_scale IS NULL)
        AND (cash_in_lieu_price IS NULL) = (cash_in_lieu_price_scale IS NULL)),
    CONSTRAINT equity_corporate_actions_cash_in_lieu_for_splits CHECK (
        cash_in_lieu_price IS NULL OR action_type IN ('SPLIT','REVERSE_SPLIT')),
    CONSTRAINT equity_corporate_actions_symbol_shape CHECK ((action_type = 'SYMBOL_CHANGE') = (new_symbol IS NOT NULL)),
    CONSTRAINT equity_corporate_actions_delisting_shape CHECK (
        (action_type = 'DELISTING') = (final_date IS NOT NULL) AND (final_date IS NULL OR final_date >= effective_date)),
    CONSTRAINT equity_corporate_actions_applying_is_delisting CHECK (status <> 'APPLYING' OR action_type = 'DELISTING'),
    CONSTRAINT equity_corporate_actions_applied_at CHECK ((status = 'APPLIED') = (applied_at IS NOT NULL))
);
CREATE INDEX equity_corporate_actions_open_idx ON equity_corporate_actions (instrument_id, effective_date)
    WHERE status IN ('ANNOUNCED','APPLYING');
CREATE TRIGGER equity_corporate_actions_updated_at BEFORE UPDATE ON equity_corporate_actions
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_equity_action_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('ANNOUNCED','APPLYING'), ('ANNOUNCED','APPLIED'), ('ANNOUNCED','CANCELLED'), ('APPLYING','APPLIED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_equity_action_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.instrument_id, NEW.action_type, NEW.effective_date, NEW.share_asset_id, NEW.cash_asset_id,
        NEW.ratio_numerator, NEW.ratio_denominator, NEW.cash_per_share, NEW.cash_per_share_scale, NEW.cash_in_lieu_price,
        NEW.cash_in_lieu_price_scale, NEW.ex_date, NEW.record_date, NEW.pay_date, NEW.final_date, NEW.new_symbol,
        NEW.rounding_policy, NEW.reason, NEW.content_hash, NEW.announced_by_actor_type, NEW.announced_by_actor_id,
        NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.instrument_id, OLD.action_type, OLD.effective_date, OLD.share_asset_id, OLD.cash_asset_id,
        OLD.ratio_numerator, OLD.ratio_denominator, OLD.cash_per_share, OLD.cash_per_share_scale, OLD.cash_in_lieu_price,
        OLD.cash_in_lieu_price_scale, OLD.ex_date, OLD.record_date, OLD.pay_date, OLD.final_date, OLD.new_symbol,
        OLD.rounding_policy, OLD.reason, OLD.content_hash, OLD.announced_by_actor_type, OLD.announced_by_actor_id,
        OLD.created_at) THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: corporate action % may change only its status', OLD.id USING ERRCODE = 'EQ002';
    END IF;
    IF OLD.applied_at IS NOT NULL AND NEW.applied_at IS DISTINCT FROM OLD.applied_at THEN
        RAISE EXCEPTION 'EQUITY_IDENTITY_FROZEN: corporate action % was applied at % and that never changes', OLD.id, OLD.applied_at
            USING ERRCODE = 'EQ002';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT cp_equity_action_transition_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION 'EQUITY_ACTION_TRANSITION_ILLEGAL: corporate action % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'EQ005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_equity_action_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER equity_corporate_actions_guard BEFORE UPDATE ON equity_corporate_actions
    FOR EACH ROW EXECUTE FUNCTION cp_equity_action_guard();
CREATE TRIGGER equity_corporate_actions_no_delete BEFORE DELETE ON equity_corporate_actions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE equity_corporate_action_transitions (
    id              uuid PRIMARY KEY,
    action_id       uuid NOT NULL REFERENCES equity_corporate_actions(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    occurred_at     timestamptz NOT NULL
);
CREATE INDEX equity_corporate_action_transitions_idx ON equity_corporate_action_transitions (action_id, occurred_at);
CREATE TRIGGER equity_corporate_action_transitions_immutable BEFORE UPDATE OR DELETE ON equity_corporate_action_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER equity_corporate_action_transitions_flag_edge AFTER INSERT ON equity_corporate_action_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('action_id', 'from_status', 'to_status', 'equity_action_status');
CREATE CONSTRAINT TRIGGER equity_corporate_actions_require_transition AFTER UPDATE OF status ON equity_corporate_actions
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'equity_action_status');

CREATE TABLE corporate_action_applications (
    id                      uuid PRIMARY KEY,
    action_id               uuid NOT NULL REFERENCES equity_corporate_actions(id),
    account_id              uuid NOT NULL REFERENCES accounts(id),
    -- Shares held (splits) or entitled (dividends), in share base units.
    entitled_quantity       numeric(38,0) NOT NULL CHECK (entitled_quantity > 0),
    shares_delta            numeric(38,0) NOT NULL,
    cash_amount             numeric(38,0) NOT NULL CHECK (cash_amount >= 0),
    -- The exact cash the rounding policy rounded, as a fraction of cash base units.
    exact_cash_numerator    numeric NOT NULL CHECK (exact_cash_numerator >= 0),
    exact_cash_denominator  numeric NOT NULL CHECK (exact_cash_denominator > 0),
    rounding_policy         text NOT NULL CHECK (rounding_policy IN ('SHARES_DOWN_CASH_DOWN','SHARES_DOWN_CASH_HALF_EVEN')),
    journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    effect_id               uuid UNIQUE REFERENCES economic_effects(id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- The idempotency of every replay: one application per account per action.
    UNIQUE (action_id, account_id),
    CONSTRAINT corporate_action_applications_journal_iff_value CHECK (
        (journal_transaction_id IS NULL) = (shares_delta = 0 AND cash_amount = 0)),
    CONSTRAINT corporate_action_applications_effect_iff_journal CHECK ((journal_transaction_id IS NULL) = (effect_id IS NULL))
);
CREATE TRIGGER corporate_action_applications_immutable BEFORE UPDATE OR DELETE ON corporate_action_applications
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 5. Calendar holidays
-- ---------------------------------------------------------------------------

CREATE TABLE equity_calendar_holidays (
    id                      uuid PRIMARY KEY,
    calendar_code           text NOT NULL CHECK (calendar_code IN ('XNYS')),
    holiday_date            date NOT NULL,
    name                    text NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    created_by_actor_type   text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id     text NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (calendar_code, holiday_date),
    -- A weekend is closed already; a holiday on one would be a data error.
    CONSTRAINT equity_calendar_holidays_weekday CHECK (extract(isodow FROM holiday_date) < 6)
);
CREATE TRIGGER equity_calendar_holidays_immutable BEFORE UPDATE OR DELETE ON equity_calendar_holidays
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 6. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON equity_orders, equity_order_transitions, equity_fills, equity_settlements,
                        equity_corporate_actions, equity_corporate_action_transitions, corporate_action_applications,
                        equity_calendar_holidays TO cp_app;
GRANT UPDATE (status, rejection_code, rejection_reasons, broker_order_id) ON equity_orders TO cp_app;
GRANT UPDATE (settled_at, journal_transaction_id) ON equity_settlements TO cp_app;
GRANT UPDATE (status, applied_at) ON equity_corporate_actions TO cp_app;
GRANT SELECT ON equity_orders, equity_order_transitions, equity_fills, equity_settlements, equity_corporate_actions,
               equity_corporate_action_transitions, corporate_action_applications, equity_calendar_holidays
    TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: equity orders, fills, settlements and corporate actions are journal history
