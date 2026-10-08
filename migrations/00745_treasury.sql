-- +goose Up
-- The treasury: the principal / house book (internal/treasury; ADVANCED_FINANCIAL_MODEL.md §2 TREASURY and
-- §5, ADR-0020). The house book itself (house_accounts) and its ledger owner type HOUSE come from 00731;
-- house orders on the internal book come from 00742. This migration adds what makes a house book a
-- controlled treasury: its registration and class, versioned risk limits, a separate signing policy for
-- house actions, the register of every house action, the paper-venue fills and fundings it posts, the
-- position register its realized and unrealized PnL is computed from, its risk decisions, mark-to-market
-- snapshots, and the disclosure of every house-versus-customer crossing rule.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A treasury book is a house book whose class matches its strategy (PRINCIPAL: liquidity provision,
--      market making, hedging; TREASURY: inventory rebalancing, treasury conversion), valued in a
--      SIMULATED asset, registered by an operator, with a frozen identity (TY001).
--   2. Every house action is taken by the house service actor its current signing policy names, or by an
--      operator -- never by a customer, an agent or an anonymous system -- and administrative actions
--      (registration, policies, funding) only by an operator. A paper fill or a funding exists only with
--      the APPLIED action that authorized it (TY002). Signing policies hold no keys: key custody is
--      NONE_SANDBOX by CHECK.
--   3. Risk policies and signing policies are append-only, consecutively versioned, hashed, and say in
--      their columns exactly what their canonical documents say (TY003).
--   4. Everything the treasury posts stays in the house segment: a paper fill (SIM_TRADE) or a funding
--      (HOUSE_FUNDING) names a journal that touches only HOUSE accounts of its own house book, moves
--      exactly the recorded quantities, and was applied by an APPLIED TREASURY effect (TY004). Customer
--      value can therefore never reach the house strategy through the treasury's own postings.
--   5. The position register is a chain: per house book a gap-free sequence, per asset each event starts
--      from the previous event's position, moves it by exactly its quantity, releases cost at average cost
--      rounded up exactly once, and matches the book trade, paper fill or funding it records (TY005).
--      A position can never go negative.
--   6. A risk-increasing house order or fill can never be ALLOWED while a treasury limit is breached.
--   7. A crossing rule that lets a house book trade with customers is disclosed: every disclosure names a
--      crossing rule version on record with the same setting, says "simulated", and is recorded by an
--      operator (TY006).
--   8. Every history table is append-only.
--
-- Custom SQLSTATEs: TY001 treasury book registration or identity invalid, TY002 house action not
-- authorized by the book's signing policy (or a house effect without its authorizing action), TY003
-- versioned policy not the next version or disagreeing with its document, TY004 treasury posting outside
-- the house segment or not matching its record, TY005 position event breaking the position register,
-- TY006 crossing disclosure without a matching crossing rule.

-- ---------------------------------------------------------------------------
-- 1. Treasury books
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_books (
    house_account_id          uuid PRIMARY KEY REFERENCES house_accounts(id),
    class                     text NOT NULL CHECK (class IN ('PRINCIPAL','TREASURY')),
    valuation_asset_id        uuid NOT NULL REFERENCES assets(id),
    -- Not choices: the columns exist so the facts are visible on the row.
    ledger_segment            text NOT NULL DEFAULT 'HOUSE' CHECK (ledger_segment = 'HOUSE'),
    value_domain              text NOT NULL DEFAULT 'SIMULATED' CHECK (value_domain = 'SIMULATED'),
    -- Bumped by every mutating treasury command under the row lock (FOR NO KEY UPDATE needs an
    -- updatable column); nothing else about a book ever changes.
    version                   bigint NOT NULL DEFAULT 0 CHECK (version >= 0),
    registered_by_actor_type  text NOT NULL CHECK (registered_by_actor_type = 'OPERATOR'),
    registered_by_actor_id    text NOT NULL CHECK (length(registered_by_actor_id) BETWEEN 1 AND 200),
    registered_at             timestamptz NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER treasury_books_updated_at BEFORE UPDATE ON treasury_books FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_treasury_book_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    strat  text;
    akind  text;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT strategy INTO strat FROM house_accounts WHERE id = NEW.house_account_id;
        IF strat IS NULL THEN
            RAISE EXCEPTION 'TREASURY_BOOK_INVALID: house book % does not exist', NEW.house_account_id USING ERRCODE = 'TY001';
        END IF;
        IF NOT ((NEW.class = 'PRINCIPAL' AND strat IN ('LIQUIDITY_PROVISION','MARKET_MAKING','HEDGING'))
             OR (NEW.class = 'TREASURY' AND strat IN ('INVENTORY_REBALANCING','TREASURY_CONVERSION'))) THEN
            RAISE EXCEPTION 'TREASURY_BOOK_INVALID: a % book cannot run the % strategy', NEW.class, strat USING ERRCODE = 'TY001';
        END IF;
        SELECT kind INTO akind FROM assets WHERE id = NEW.valuation_asset_id;
        IF akind IS DISTINCT FROM 'SIMULATED' THEN
            RAISE EXCEPTION 'TREASURY_BOOK_INVALID: a house book is valued in a SIMULATED asset, not %', akind USING ERRCODE = 'TY001';
        END IF;
        IF NEW.version <> 0 THEN
            RAISE EXCEPTION 'TREASURY_BOOK_INVALID: a treasury book is registered at version 0' USING ERRCODE = 'TY001';
        END IF;
        RETURN NEW;
    END IF;
    IF (NEW.house_account_id, NEW.class, NEW.valuation_asset_id, NEW.ledger_segment, NEW.value_domain,
        NEW.registered_by_actor_type, NEW.registered_by_actor_id, NEW.registered_at, NEW.created_at)
       IS DISTINCT FROM
       (OLD.house_account_id, OLD.class, OLD.valuation_asset_id, OLD.ledger_segment, OLD.value_domain,
        OLD.registered_by_actor_type, OLD.registered_by_actor_id, OLD.registered_at, OLD.created_at)
       OR NEW.version <> OLD.version + 1 THEN
        RAISE EXCEPTION 'TREASURY_BOOK_FROZEN: treasury book % may only advance its version by one', OLD.house_account_id
            USING ERRCODE = 'TY001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_book_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_books_guard BEFORE INSERT OR UPDATE ON treasury_books
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_book_guard();

-- ---------------------------------------------------------------------------
-- 2. Signing policies and risk policies (versioned, hashed, append-only)
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_signing_policies (
    id                  uuid PRIMARY KEY,
    house_account_id    uuid NOT NULL REFERENCES treasury_books(house_account_id),
    version             integer NOT NULL CHECK (version >= 1),
    -- A sandbox house book holds no keys at all; its policy is a recorded rule about who may act.
    key_custody         text NOT NULL CHECK (key_custody = 'NONE_SANDBOX'),
    service_actor_id    text NOT NULL CHECK (length(service_actor_id) BETWEEN 1 AND 200),
    operators_allowed   boolean NOT NULL,
    policy              bytea NOT NULL,
    policy_hash         text NOT NULL,
    actor_type          text NOT NULL CHECK (actor_type = 'OPERATOR'),
    actor_id            text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    reason              text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    effective_at        timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, version),
    CONSTRAINT treasury_signing_policies_hash CHECK (encode(sha256(policy), 'hex') = policy_hash)
);
CREATE TRIGGER treasury_signing_policies_immutable BEFORE UPDATE OR DELETE ON treasury_signing_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE treasury_policies (
    id                  uuid PRIMARY KEY,
    house_account_id    uuid NOT NULL REFERENCES treasury_books(house_account_id),
    version             integer NOT NULL CHECK (version >= 1),
    label               text NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
    valuation_asset_id  uuid NOT NULL REFERENCES assets(id),
    daily_loss_limit    numeric(38,0) NOT NULL CHECK (daily_loss_limit > 0),
    max_mark_age_ms     bigint NOT NULL CHECK (max_mark_age_ms BETWEEN 1 AND 86400000),
    policy              bytea NOT NULL,
    policy_hash         text NOT NULL,
    actor_type          text NOT NULL CHECK (actor_type = 'OPERATOR'),
    actor_id            text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    reason              text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    effective_at        timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, version),
    CONSTRAINT treasury_policies_hash CHECK (encode(sha256(policy), 'hex') = policy_hash)
);
CREATE TRIGGER treasury_policies_immutable BEFORE UPDATE OR DELETE ON treasury_policies
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_treasury_policy_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest  integer;
    doc     jsonb;
    book    record;
BEGIN
    EXECUTE format('SELECT max(version) FROM %I WHERE house_account_id = $1', TG_TABLE_NAME) INTO latest USING NEW.house_account_id;
    IF NEW.version <> coalesce(latest, 0) + 1 THEN
        RAISE EXCEPTION 'TREASURY_POLICY_VERSION_GAP: % version % of house book % is not the next after %',
            TG_TABLE_NAME, NEW.version, NEW.house_account_id, coalesce(latest, 0) USING ERRCODE = 'TY003';
    END IF;
    doc := convert_from(NEW.policy, 'UTF8')::jsonb;
    IF (doc ->> 'house_account_id') IS DISTINCT FROM NEW.house_account_id::text THEN
        RAISE EXCEPTION 'TREASURY_POLICY_MISMATCH: % document names another house book', TG_TABLE_NAME USING ERRCODE = 'TY003';
    END IF;
    IF TG_TABLE_NAME = 'treasury_signing_policies' THEN
        IF (doc ->> 'kind') IS DISTINCT FROM 'treasury.signing_policy.v1'
           OR (doc ->> 'key_custody') IS DISTINCT FROM NEW.key_custody
           OR (doc ->> 'service_actor_id') IS DISTINCT FROM NEW.service_actor_id
           OR (doc ->> 'operators_allowed')::boolean IS DISTINCT FROM NEW.operators_allowed THEN
            RAISE EXCEPTION 'TREASURY_POLICY_MISMATCH: signing policy % does not say what its columns say', NEW.id
                USING ERRCODE = 'TY003';
        END IF;
    ELSE
        SELECT valuation_asset_id INTO book FROM treasury_books WHERE house_account_id = NEW.house_account_id;
        IF (doc ->> 'kind') IS DISTINCT FROM 'treasury.policy.v1'
           OR (doc ->> 'label') IS DISTINCT FROM NEW.label
           OR (doc ->> 'valuation_asset') IS DISTINCT FROM NEW.valuation_asset_id::text
           OR (doc ->> 'daily_loss_limit')::numeric IS DISTINCT FROM NEW.daily_loss_limit
           OR (doc ->> 'max_mark_age_ms')::bigint IS DISTINCT FROM NEW.max_mark_age_ms
           OR NEW.valuation_asset_id IS DISTINCT FROM book.valuation_asset_id THEN
            RAISE EXCEPTION 'TREASURY_POLICY_MISMATCH: risk policy % does not say what its columns and its book say', NEW.id
                USING ERRCODE = 'TY003';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_policy_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_signing_policies_is_next BEFORE INSERT ON treasury_signing_policies
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_policy_is_next();
CREATE TRIGGER treasury_policies_is_next BEFORE INSERT ON treasury_policies
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_policy_is_next();

-- ---------------------------------------------------------------------------
-- 3. Risk decisions (Risk Kernel V2 decisions about house books, with the treasury limit evaluation)
-- ---------------------------------------------------------------------------

-- advanced_risk_decisions names a customer account; a house book is not one, so its decisions live here,
-- with the same canonical input, decision and hashes plus the treasury limits that fed the kernel.
CREATE TABLE treasury_risk_decisions (
    id                       uuid PRIMARY KEY,
    house_account_id         uuid NOT NULL REFERENCES treasury_books(house_account_id),
    intent_kind              text NOT NULL CHECK (intent_kind IN ('PROVIDE_LIQUIDITY','TAKE_LIQUIDITY')),
    instrument_id            uuid REFERENCES instruments(id),
    asset_id                 uuid NOT NULL REFERENCES assets(id),
    risk_increasing          boolean NOT NULL,
    stage                    text NOT NULL CHECK (stage IN ('PRE_TRADE','FINAL','CONTINUOUS')),
    verdict                  text NOT NULL CHECK (verdict IN ('ALLOW','REJECT')),
    reason_codes             text[] NOT NULL,
    treasury_codes           text[] NOT NULL,
    input                    jsonb NOT NULL,
    decision                 jsonb NOT NULL,
    limits                   jsonb NOT NULL,
    input_hash               text NOT NULL,
    decision_hash            text NOT NULL,
    risk_policy_version      text NOT NULL,
    risk_policy_hash         text NOT NULL,
    evaluator_version        text NOT NULL,
    treasury_policy_version  integer NOT NULL CHECK (treasury_policy_version >= 1),
    treasury_policy_hash     text NOT NULL CHECK (length(treasury_policy_hash) = 64),
    evaluated_at             timestamptz NOT NULL,
    created_at               timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT treasury_risk_decisions_verdict_reasons CHECK ((verdict = 'ALLOW') = (cardinality(reason_codes) = 0)),
    -- A breached treasury limit always stops new risk.
    CONSTRAINT treasury_risk_decisions_limits_bind CHECK (
        NOT (risk_increasing AND verdict = 'ALLOW' AND cardinality(treasury_codes) > 0))
);
CREATE INDEX treasury_risk_decisions_house_idx ON treasury_risk_decisions (house_account_id, evaluated_at DESC);
CREATE TRIGGER treasury_risk_decisions_immutable BEFORE UPDATE OR DELETE ON treasury_risk_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 4. Fundings and paper-venue fills: the only postings the treasury makes itself
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_fundings (
    id                      uuid PRIMARY KEY,
    house_account_id        uuid NOT NULL REFERENCES treasury_books(house_account_id),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    -- Valuation-asset base units the funded quantity enters the position register at: the quantity
    -- itself for the valuation asset, the quantity at the mark (rounded up) for anything else.
    cost_basis              numeric(38,0) NOT NULL CHECK (cost_basis >= 0),
    mark_mantissa           numeric(38,0) CHECK (mark_mantissa IS NULL OR mark_mantissa > 0),
    mark_scale              integer CHECK (mark_scale IS NULL OR mark_scale BETWEEN 0 AND 38),
    mark_observed_at        timestamptz,
    idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    evidence_ref            text NOT NULL,
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    funded_at               timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, idempotency_key),
    CONSTRAINT treasury_fundings_mark_shape CHECK (
        (mark_mantissa IS NULL) = (mark_scale IS NULL) AND (mark_scale IS NULL) = (mark_observed_at IS NULL))
);
CREATE TRIGGER treasury_fundings_immutable BEFORE UPDATE OR DELETE ON treasury_fundings
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE treasury_paper_fills (
    id                      uuid PRIMARY KEY,
    house_account_id        uuid NOT NULL REFERENCES treasury_books(house_account_id),
    activity                text NOT NULL CHECK (activity IN ('LIQUIDITY_PROVISION','MARKET_MAKING','HEDGING',
                                                              'INVENTORY_REBALANCING','TREASURY_CONVERSION')),
    venue_label             text NOT NULL DEFAULT 'SIMULATED_PAPER_VENUE' CHECK (venue_label = 'SIMULATED_PAPER_VENUE'),
    side                    text NOT NULL CHECK (side IN ('BUY','SELL')),
    base_asset_id           uuid NOT NULL REFERENCES assets(id),
    quote_asset_id          uuid NOT NULL REFERENCES assets(id),
    base_quantity           numeric(38,0) NOT NULL CHECK (base_quantity > 0),
    reference_mantissa      numeric(38,0) NOT NULL CHECK (reference_mantissa > 0),
    reference_scale         integer NOT NULL CHECK (reference_scale BETWEEN 0 AND 38),
    mark_observed_at        timestamptz NOT NULL,
    notional_at_reference   numeric(38,0) NOT NULL CHECK (notional_at_reference >= 0),
    spread_bps              integer NOT NULL CHECK (spread_bps BETWEEN 0 AND 9999),
    spread_amount           numeric(38,0) NOT NULL CHECK (spread_amount >= 0),
    fee_amount              numeric(38,0) NOT NULL CHECK (fee_amount >= 0),
    total_quote             numeric(38,0) NOT NULL CHECK (total_quote >= 0),
    -- The fees.Schedule canonical form the fill was priced under and the complete fees.Disclosure: every
    -- unit of spread and fee the house paid is recomputable from these two documents alone.
    schedule                bytea NOT NULL,
    schedule_hash           text NOT NULL,
    disclosure              bytea NOT NULL,
    disclosure_hash         text NOT NULL,
    idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    effect_id               uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    journal_transaction_id  uuid NOT NULL UNIQUE REFERENCES journal_transactions(id),
    evidence_ref            text NOT NULL,
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    filled_at               timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, idempotency_key),
    CHECK (base_asset_id <> quote_asset_id),
    CONSTRAINT treasury_paper_fills_schedule_hash CHECK (encode(sha256(schedule), 'hex') = schedule_hash),
    CONSTRAINT treasury_paper_fills_disclosure_hash CHECK (encode(sha256(disclosure), 'hex') = disclosure_hash),
    CONSTRAINT treasury_paper_fills_conserved CHECK (
        (side <> 'BUY' OR total_quote = notional_at_reference + spread_amount + fee_amount)
        AND (side <> 'SELL' OR total_quote = notional_at_reference - spread_amount - fee_amount))
);
CREATE INDEX treasury_paper_fills_house_idx ON treasury_paper_fills (house_account_id, filled_at);
CREATE TRIGGER treasury_paper_fills_immutable BEFORE UPDATE OR DELETE ON treasury_paper_fills
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The house segment: a treasury posting touches only its own house book's accounts and moves exactly
-- what its record says. Checked when the record is written, after its journal (same transaction).
-- +goose StatementBegin
CREATE FUNCTION cp_treasury_posting_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    j        record;
    legs     record;
    eff      record;
    valuation uuid;
    ref_type text;
    want_kind text;
BEGIN
    SELECT valuation_asset_id INTO valuation FROM treasury_books WHERE house_account_id = NEW.house_account_id;
    IF TG_TABLE_NAME = 'treasury_paper_fills' THEN
        ref_type := 'treasury_paper_fill';
        want_kind := 'SIM_TRADE';
    ELSE
        ref_type := 'treasury_funding';
        want_kind := 'HOUSE_FUNDING';
    END IF;
    SELECT kind, reference_type, reference_id INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM want_kind OR j.reference_type IS DISTINCT FROM ref_type OR j.reference_id IS DISTINCT FROM NEW.id::text THEN
        RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: % % names journal % which is not its own %',
            TG_TABLE_NAME, NEW.id, NEW.journal_transaction_id, want_kind USING ERRCODE = 'TY004';
    END IF;
    IF TG_TABLE_NAME = 'treasury_paper_fills' THEN
        IF NEW.quote_asset_id IS DISTINCT FROM valuation THEN
            RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: paper fill % is not quoted in the book''s valuation asset', NEW.id
                USING ERRCODE = 'TY004';
        END IF;
        SELECT count(*) FILTER (WHERE la.owner_type <> 'HOUSE' OR la.owner_id <> NEW.house_account_id) AS foreign_legs,
               count(*) FILTER (WHERE e.asset_id <> NEW.base_asset_id AND e.asset_id <> NEW.quote_asset_id) AS foreign_assets,
               coalesce(sum(e.quantity) FILTER (WHERE la.code = 'HOUSE_INVENTORY' AND e.asset_id = NEW.base_asset_id AND e.side = 'DEBIT'), 0) AS base_in,
               coalesce(sum(e.quantity) FILTER (WHERE la.code = 'HOUSE_INVENTORY' AND e.asset_id = NEW.base_asset_id AND e.side = 'CREDIT'), 0) AS base_out,
               coalesce(sum(e.quantity) FILTER (WHERE la.code = 'HOUSE_INVENTORY' AND e.asset_id = NEW.quote_asset_id AND e.side = 'DEBIT'), 0) AS quote_in,
               coalesce(sum(e.quantity) FILTER (WHERE la.code = 'HOUSE_INVENTORY' AND e.asset_id = NEW.quote_asset_id AND e.side = 'CREDIT'), 0) AS quote_out,
               coalesce(sum(e.quantity) FILTER (WHERE la.code = 'HOUSE_FEES' AND e.asset_id = NEW.quote_asset_id AND e.side = 'DEBIT'), 0) AS fees
          INTO legs
          FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.journal_transaction_id;
        IF legs.foreign_legs > 0 OR legs.foreign_assets > 0 THEN
            RAISE EXCEPTION 'TREASURY_SEGMENT_VIOLATION: journal % of paper fill % touches an account outside house book %',
                NEW.journal_transaction_id, NEW.id, NEW.house_account_id USING ERRCODE = 'TY004';
        END IF;
        IF legs.fees <> NEW.spread_amount + NEW.fee_amount
           OR (NEW.side = 'BUY' AND (legs.base_in <> NEW.base_quantity OR legs.base_out <> 0
                                     OR legs.quote_out <> NEW.total_quote OR legs.quote_in <> 0))
           OR (NEW.side = 'SELL' AND (legs.base_out <> NEW.base_quantity OR legs.base_in <> 0
                                      OR legs.quote_in <> NEW.total_quote OR legs.quote_out <> 0)) THEN
            RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: journal % does not move what paper fill % records',
                NEW.journal_transaction_id, NEW.id USING ERRCODE = 'TY004';
        END IF;
    ELSE
        IF NEW.asset_id = valuation AND (NEW.cost_basis <> NEW.quantity OR NEW.mark_mantissa IS NOT NULL) THEN
            RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: funding % in the valuation asset enters at its own quantity', NEW.id
                USING ERRCODE = 'TY004';
        END IF;
        IF NEW.asset_id <> valuation AND NEW.mark_mantissa IS NULL THEN
            RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: funding % of a non-valuation asset needs the mark it was valued at', NEW.id
                USING ERRCODE = 'TY004';
        END IF;
        SELECT count(*) AS n,
               count(*) FILTER (WHERE la.code = 'HOUSE_INVENTORY' AND e.side = 'DEBIT') AS a,
               count(*) FILTER (WHERE la.code = 'HOUSE_CAPITAL' AND e.side = 'CREDIT') AS b,
               coalesce(bool_and(la.owner_type = 'HOUSE' AND la.owner_id = NEW.house_account_id
                                 AND e.asset_id = NEW.asset_id AND e.quantity = NEW.quantity), false) AS exact
          INTO legs
          FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.journal_transaction_id;
        IF legs.n <> 2 OR legs.a <> 1 OR legs.b <> 1 OR NOT legs.exact THEN
            RAISE EXCEPTION 'TREASURY_SEGMENT_VIOLATION: journal % does not fund exactly % of asset % into house book %',
                NEW.journal_transaction_id, NEW.quantity, NEW.asset_id, NEW.house_account_id USING ERRCODE = 'TY004';
        END IF;
    END IF;
    SELECT status, subsystem, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'TREASURY'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'TREASURY_POSTING_MISMATCH: % % names effect % which did not apply journal %',
            TG_TABLE_NAME, NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'TY004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_posting_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_paper_fills_match BEFORE INSERT ON treasury_paper_fills
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_posting_matches();
CREATE TRIGGER treasury_fundings_match BEFORE INSERT ON treasury_fundings
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_posting_matches();

-- ---------------------------------------------------------------------------
-- 5. The position register (realized PnL at average cost)
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_position_events (
    id                      uuid PRIMARY KEY,
    house_account_id        uuid NOT NULL REFERENCES treasury_books(house_account_id),
    seq                     bigint NOT NULL CHECK (seq >= 1),
    kind                    text NOT NULL CHECK (kind IN ('FUNDING','BOOK_FILL','PAPER_FILL')),
    direction               text NOT NULL CHECK (direction IN ('INCREASE','DECREASE')),
    asset_id                uuid NOT NULL REFERENCES assets(id),
    quantity                numeric(38,0) NOT NULL CHECK (quantity > 0),
    -- Valuation-asset base units: the cost added by an increase, the proceeds of a decrease.
    value                   numeric(38,0) NOT NULL CHECK (value >= 0),
    -- Explicit costs (a paper fill's spread and fee), expensed into realized PnL, never capitalized.
    fees                    numeric(38,0) NOT NULL CHECK (fees >= 0),
    qty_before              numeric(38,0) NOT NULL CHECK (qty_before >= 0),
    qty_after               numeric(38,0) NOT NULL CHECK (qty_after >= 0),
    cost_before             numeric(38,0) NOT NULL CHECK (cost_before >= 0),
    cost_after              numeric(38,0) NOT NULL CHECK (cost_after >= 0),
    cost_removed            numeric(38,0) NOT NULL CHECK (cost_removed >= 0),
    realized_pnl            numeric(38,0) NOT NULL,
    book_trade_id           uuid REFERENCES book_trades(id),
    paper_fill_id           uuid UNIQUE REFERENCES treasury_paper_fills(id),
    funding_id              uuid UNIQUE REFERENCES treasury_fundings(id),
    instrument_id           uuid REFERENCES instruments(id),
    journal_transaction_id  uuid NOT NULL REFERENCES journal_transactions(id),
    occurred_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, seq),
    UNIQUE (house_account_id, book_trade_id),
    CONSTRAINT treasury_position_events_source CHECK (
        (kind = 'FUNDING') = (funding_id IS NOT NULL) AND (kind = 'BOOK_FILL') = (book_trade_id IS NOT NULL)
        AND (kind = 'PAPER_FILL') = (paper_fill_id IS NOT NULL) AND (kind = 'BOOK_FILL') = (instrument_id IS NOT NULL)),
    CONSTRAINT treasury_position_events_funding_increases CHECK (kind <> 'FUNDING' OR direction = 'INCREASE'),
    CONSTRAINT treasury_position_events_increase CHECK (direction <> 'INCREASE' OR (
        qty_after = qty_before + quantity AND cost_after = cost_before + value AND cost_removed = 0 AND realized_pnl = -fees)),
    CONSTRAINT treasury_position_events_decrease CHECK (direction <> 'DECREASE' OR (
        qty_after = qty_before - quantity AND cost_removed <= cost_before AND cost_after = cost_before - cost_removed
        AND realized_pnl = value - cost_removed - fees)),
    -- Average cost released exactly once, rounded up: cost_removed = ceil(cost_before * quantity / qty_before).
    CONSTRAINT treasury_position_events_average_cost CHECK (direction <> 'DECREASE' OR (
        cost_removed * qty_before >= cost_before * quantity AND (cost_removed - 1) * qty_before < cost_before * quantity))
);
CREATE INDEX treasury_position_events_asset_idx ON treasury_position_events (house_account_id, asset_id, seq DESC);
CREATE INDEX treasury_position_events_day_idx ON treasury_position_events (house_account_id, occurred_at);
CREATE TRIGGER treasury_position_events_immutable BEFORE UPDATE OR DELETE ON treasury_position_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_treasury_position_event_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest_seq  bigint;
    prev        record;
    valuation   uuid;
    tr          record;
    pf          record;
    fd          record;
    house_side  text;
    want_dir    text;
BEGIN
    SELECT valuation_asset_id INTO valuation FROM treasury_books WHERE house_account_id = NEW.house_account_id;
    IF valuation IS NULL OR NEW.asset_id = valuation THEN
        RAISE EXCEPTION 'TREASURY_POSITION_INVALID: the valuation asset is cash, not a position' USING ERRCODE = 'TY005';
    END IF;
    SELECT max(seq) INTO latest_seq FROM treasury_position_events WHERE house_account_id = NEW.house_account_id;
    IF NEW.seq <> coalesce(latest_seq, 0) + 1 THEN
        RAISE EXCEPTION 'TREASURY_POSITION_GAP: event % of house book % is not the next after %',
            NEW.seq, NEW.house_account_id, coalesce(latest_seq, 0) USING ERRCODE = 'TY005';
    END IF;
    SELECT qty_after, cost_after INTO prev FROM treasury_position_events
     WHERE house_account_id = NEW.house_account_id AND asset_id = NEW.asset_id ORDER BY seq DESC LIMIT 1;
    IF NEW.qty_before IS DISTINCT FROM coalesce(prev.qty_after, 0) OR NEW.cost_before IS DISTINCT FROM coalesce(prev.cost_after, 0) THEN
        RAISE EXCEPTION 'TREASURY_POSITION_CHAIN: event % does not start from the previous position of asset %',
            NEW.seq, NEW.asset_id USING ERRCODE = 'TY005';
    END IF;
    IF NEW.kind = 'BOOK_FILL' THEN
        SELECT instrument_id, base_asset_id, quote_asset_id, base_quantity, quote_notional, aggressor_side,
               maker_owner_type, maker_owner_id, taker_owner_type, taker_owner_id, journal_transaction_id
          INTO tr FROM book_trades WHERE id = NEW.book_trade_id;
        IF tr.maker_owner_type = 'HOUSE' AND tr.maker_owner_id = NEW.house_account_id THEN
            house_side := CASE WHEN tr.aggressor_side = 'BUY' THEN 'SELL' ELSE 'BUY' END;
        ELSIF tr.taker_owner_type = 'HOUSE' AND tr.taker_owner_id = NEW.house_account_id THEN
            house_side := tr.aggressor_side;
        ELSE
            RAISE EXCEPTION 'TREASURY_POSITION_SOURCE: trade % is not a trade of house book %', NEW.book_trade_id, NEW.house_account_id
                USING ERRCODE = 'TY005';
        END IF;
        want_dir := CASE WHEN house_side = 'BUY' THEN 'INCREASE' ELSE 'DECREASE' END;
        IF NEW.asset_id <> tr.base_asset_id OR tr.quote_asset_id <> valuation OR NEW.quantity <> tr.base_quantity
           OR NEW.value <> tr.quote_notional OR NEW.fees <> 0 OR NEW.direction <> want_dir
           OR NEW.instrument_id IS DISTINCT FROM tr.instrument_id OR NEW.journal_transaction_id <> tr.journal_transaction_id THEN
            RAISE EXCEPTION 'TREASURY_POSITION_SOURCE: event % does not record trade %', NEW.seq, NEW.book_trade_id
                USING ERRCODE = 'TY005';
        END IF;
    ELSIF NEW.kind = 'PAPER_FILL' THEN
        SELECT house_account_id, side, base_asset_id, base_quantity, notional_at_reference, spread_amount, fee_amount,
               journal_transaction_id
          INTO pf FROM treasury_paper_fills WHERE id = NEW.paper_fill_id;
        want_dir := CASE WHEN pf.side = 'BUY' THEN 'INCREASE' ELSE 'DECREASE' END;
        IF pf.house_account_id IS DISTINCT FROM NEW.house_account_id OR NEW.asset_id <> pf.base_asset_id
           OR NEW.quantity <> pf.base_quantity OR NEW.value <> pf.notional_at_reference
           OR NEW.fees <> pf.spread_amount + pf.fee_amount OR NEW.direction <> want_dir
           OR NEW.journal_transaction_id <> pf.journal_transaction_id THEN
            RAISE EXCEPTION 'TREASURY_POSITION_SOURCE: event % does not record paper fill %', NEW.seq, NEW.paper_fill_id
                USING ERRCODE = 'TY005';
        END IF;
    ELSE
        SELECT house_account_id, asset_id, quantity, cost_basis, journal_transaction_id
          INTO fd FROM treasury_fundings WHERE id = NEW.funding_id;
        IF fd.house_account_id IS DISTINCT FROM NEW.house_account_id OR NEW.asset_id <> fd.asset_id
           OR NEW.quantity <> fd.quantity OR NEW.value <> fd.cost_basis OR NEW.fees <> 0
           OR NEW.journal_transaction_id <> fd.journal_transaction_id THEN
            RAISE EXCEPTION 'TREASURY_POSITION_SOURCE: event % does not record funding %', NEW.seq, NEW.funding_id
                USING ERRCODE = 'TY005';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_position_event_chain() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_position_events_chain BEFORE INSERT ON treasury_position_events
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_position_event_chain();

-- ---------------------------------------------------------------------------
-- 6. Mark-to-market snapshots
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_pnl_snapshots (
    id                  uuid PRIMARY KEY,
    house_account_id    uuid NOT NULL REFERENCES treasury_books(house_account_id),
    through_event_seq   bigint NOT NULL CHECK (through_event_seq >= 0),
    realized_total      numeric(38,0) NOT NULL,
    realized_today      numeric(38,0) NOT NULL,
    unrealized_total    numeric(38,0) NOT NULL,
    open_losses         numeric(38,0) NOT NULL CHECK (open_losses <= 0),
    document            bytea NOT NULL,
    document_hash       text NOT NULL,
    taken_at            timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT treasury_pnl_snapshots_hash CHECK (encode(sha256(document), 'hex') = document_hash)
);
CREATE INDEX treasury_pnl_snapshots_house_idx ON treasury_pnl_snapshots (house_account_id, taken_at DESC);
CREATE TRIGGER treasury_pnl_snapshots_immutable BEFORE UPDATE OR DELETE ON treasury_pnl_snapshots
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- ---------------------------------------------------------------------------
-- 7. The register of house actions
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_actions (
    id                      uuid PRIMARY KEY,
    house_account_id        uuid NOT NULL REFERENCES treasury_books(house_account_id),
    kind                    text NOT NULL CHECK (kind IN ('REGISTER','SET_POLICY','SET_SIGNING_POLICY','FUND','PLACE_ORDER',
                                                          'CANCEL_ORDER','PAPER_FILL','MARK_TO_MARKET')),
    activity                text CHECK (activity IN ('LIQUIDITY_PROVISION','MARKET_MAKING','HEDGING',
                                                     'INVENTORY_REBALANCING','TREASURY_CONVERSION')),
    outcome                 text NOT NULL CHECK (outcome IN ('APPLIED','REJECTED')),
    rejection_code          text CHECK (rejection_code IS NULL OR rejection_code ~ '^[A-Z][A-Z0-9_]{1,63}$'),
    reason_codes            text[] NOT NULL DEFAULT '{}',
    actor_type              text NOT NULL CHECK (actor_type IN ('SERVICE','OPERATOR')),
    actor_id                text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    signing_policy_version  integer NOT NULL CHECK (signing_policy_version >= 1),
    risk_policy_version     integer CHECK (risk_policy_version IS NULL OR risk_policy_version >= 1),
    idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash            bytea NOT NULL CHECK (length(request_hash) = 32),
    risk_decision_id        uuid REFERENCES treasury_risk_decisions(id),
    order_id                uuid REFERENCES book_orders(id),
    paper_fill_id           uuid UNIQUE REFERENCES treasury_paper_fills(id),
    funding_id              uuid UNIQUE REFERENCES treasury_fundings(id),
    policy_id               uuid UNIQUE REFERENCES treasury_policies(id),
    signing_policy_id       uuid UNIQUE REFERENCES treasury_signing_policies(id),
    snapshot_id             uuid UNIQUE REFERENCES treasury_pnl_snapshots(id),
    effect_id               uuid REFERENCES economic_effects(id),
    evidence_ref            text,
    evidence_sha256         bytea CHECK (evidence_sha256 IS NULL OR length(evidence_sha256) = 32),
    -- The audit tag of every house action: it belongs to the house ledger segment, never a customer's.
    ledger_segment          text NOT NULL DEFAULT 'HOUSE' CHECK (ledger_segment = 'HOUSE'),
    correlation_id          text,
    occurred_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (house_account_id, idempotency_key),
    CONSTRAINT treasury_actions_rejection_has_code CHECK ((outcome = 'REJECTED') = (rejection_code IS NOT NULL)),
    CONSTRAINT treasury_actions_activity_shape CHECK (
        (kind = 'PLACE_ORDER' OR kind = 'PAPER_FILL') = (activity IS NOT NULL)),
    CONSTRAINT treasury_actions_applied_shape CHECK (outcome = 'REJECTED' OR (
        (kind <> 'PLACE_ORDER' OR (order_id IS NOT NULL AND risk_decision_id IS NOT NULL))
        AND (kind <> 'CANCEL_ORDER' OR order_id IS NOT NULL)
        AND (kind <> 'PAPER_FILL' OR (paper_fill_id IS NOT NULL AND risk_decision_id IS NOT NULL AND effect_id IS NOT NULL))
        AND (kind <> 'FUND' OR (funding_id IS NOT NULL AND effect_id IS NOT NULL))
        AND (kind <> 'SET_POLICY' OR policy_id IS NOT NULL)
        AND (kind <> 'SET_SIGNING_POLICY' OR signing_policy_id IS NOT NULL)
        AND (kind <> 'MARK_TO_MARKET' OR snapshot_id IS NOT NULL)))
);
CREATE INDEX treasury_actions_house_idx ON treasury_actions (house_account_id, occurred_at DESC);
CREATE INDEX treasury_actions_order_idx ON treasury_actions (order_id) WHERE order_id IS NOT NULL;
CREATE TRIGGER treasury_actions_immutable BEFORE UPDATE OR DELETE ON treasury_actions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The authority check: the signing policy in force names who may act for the house book.
-- +goose StatementBegin
CREATE FUNCTION cp_treasury_action_authorized() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    sp  record;
BEGIN
    SELECT version, service_actor_id, operators_allowed INTO sp FROM treasury_signing_policies
     WHERE house_account_id = NEW.house_account_id ORDER BY version DESC LIMIT 1;
    IF sp.version IS NULL OR sp.version <> NEW.signing_policy_version THEN
        RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: action % names signing policy version %, the policy in force is %',
            NEW.id, NEW.signing_policy_version, sp.version USING ERRCODE = 'TY002';
    END IF;
    IF NEW.kind = 'REGISTER' OR NEW.kind = 'SET_POLICY' OR NEW.kind = 'SET_SIGNING_POLICY' OR NEW.kind = 'FUND' THEN
        IF NEW.actor_type <> 'OPERATOR' THEN
            RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: % is an operator action; % % may not take it', NEW.kind, NEW.actor_type, NEW.actor_id
                USING ERRCODE = 'TY002';
        END IF;
    ELSIF NEW.actor_type = 'SERVICE' THEN
        IF NEW.actor_id <> sp.service_actor_id THEN
            RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: service % is not the house service % of house book %',
                NEW.actor_id, sp.service_actor_id, NEW.house_account_id USING ERRCODE = 'TY002';
        END IF;
    ELSIF NEW.actor_type = 'OPERATOR' THEN
        IF NOT sp.operators_allowed AND NEW.kind <> 'CANCEL_ORDER' THEN
            RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: the signing policy of house book % does not let operators trade', NEW.house_account_id
                USING ERRCODE = 'TY002';
        END IF;
    ELSE
        RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: % may never act for a house book', NEW.actor_type USING ERRCODE = 'TY002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_action_authorized() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_actions_authorized BEFORE INSERT ON treasury_actions
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_action_authorized();

-- A posting the treasury made exists only with the APPLIED action that authorized it (at COMMIT: the
-- action is written after its fill or funding, in the same transaction).
-- +goose StatementBegin
CREATE FUNCTION cp_treasury_effect_has_action() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    found boolean;
BEGIN
    IF TG_TABLE_NAME = 'treasury_paper_fills' THEN
        SELECT EXISTS (SELECT 1 FROM treasury_actions WHERE paper_fill_id = NEW.id AND outcome = 'APPLIED'
                        AND kind = 'PAPER_FILL' AND house_account_id = NEW.house_account_id) INTO found;
    ELSE
        SELECT EXISTS (SELECT 1 FROM treasury_actions WHERE funding_id = NEW.id AND outcome = 'APPLIED'
                        AND kind = 'FUND' AND house_account_id = NEW.house_account_id) INTO found;
    END IF;
    IF NOT found THEN
        RAISE EXCEPTION 'TREASURY_NOT_AUTHORIZED: % % has no APPLIED house action authorizing it', TG_TABLE_NAME, NEW.id
            USING ERRCODE = 'TY002';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_effect_has_action() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER treasury_paper_fills_authorized AFTER INSERT ON treasury_paper_fills
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_treasury_effect_has_action();
CREATE CONSTRAINT TRIGGER treasury_fundings_authorized AFTER INSERT ON treasury_fundings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_treasury_effect_has_action();

-- ---------------------------------------------------------------------------
-- 8. Disclosed house-versus-customer crossing rules
-- ---------------------------------------------------------------------------

CREATE TABLE treasury_crossing_disclosures (
    id               uuid PRIMARY KEY,
    instrument_id    uuid NOT NULL REFERENCES instruments(id),
    book_id          uuid NOT NULL REFERENCES book_states(id),
    rule_version     integer NOT NULL CHECK (rule_version >= 1),
    allow            boolean NOT NULL,
    -- What a customer is told. Every surface says simulated, and so must this.
    disclosure       text NOT NULL CHECK (length(disclosure) BETWEEN 20 AND 2000 AND disclosure ILIKE '%simulated%'),
    reason           text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    actor_type       text NOT NULL CHECK (actor_type = 'OPERATOR'),
    actor_id         text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    evidence_ref     text NOT NULL,
    evidence_sha256  bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    disclosed_at     timestamptz NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (book_id, rule_version)
);
CREATE TRIGGER treasury_crossing_disclosures_immutable BEFORE UPDATE OR DELETE ON treasury_crossing_disclosures
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_treasury_crossing_disclosed() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    rule   record;
    book   record;
BEGIN
    SELECT instrument_id INTO book FROM book_states WHERE id = NEW.book_id;
    SELECT allow_house_customer_crossing INTO rule FROM book_crossing_rules WHERE book_id = NEW.book_id AND version = NEW.rule_version;
    IF book.instrument_id IS DISTINCT FROM NEW.instrument_id OR NOT FOUND
       OR rule.allow_house_customer_crossing IS DISTINCT FROM NEW.allow THEN
        RAISE EXCEPTION 'TREASURY_CROSSING_UNDISCLOSED: disclosure % names no crossing rule version % of book % allowing %',
            NEW.id, NEW.rule_version, NEW.book_id, NEW.allow USING ERRCODE = 'TY006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_treasury_crossing_disclosed() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER treasury_crossing_disclosures_match BEFORE INSERT ON treasury_crossing_disclosures
    FOR EACH ROW EXECUTE FUNCTION cp_treasury_crossing_disclosed();

-- ---------------------------------------------------------------------------
-- 9. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON treasury_books, treasury_signing_policies, treasury_policies, treasury_risk_decisions,
                        treasury_fundings, treasury_paper_fills, treasury_position_events, treasury_pnl_snapshots,
                        treasury_actions, treasury_crossing_disclosures TO cp_app;
GRANT UPDATE (version) ON treasury_books TO cp_app;
GRANT SELECT ON treasury_books, treasury_signing_policies, treasury_policies, treasury_risk_decisions,
               treasury_fundings, treasury_paper_fills, treasury_position_events, treasury_pnl_snapshots,
               treasury_actions, treasury_crossing_disclosures TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the house book's actions, fills and positions are referenced by ledger history
