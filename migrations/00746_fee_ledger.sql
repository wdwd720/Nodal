-- +goose Up
-- The fee ledger: persistence and accounting of the transparent spread and fee engine and of the sandbox
-- performance fee (internal/feeledger; ADVANCED_FINANCIAL_MODEL.md §2 FEES and §5, ADR-0020).
-- internal/fees is the pure engine and nothing here recomputes it. What the database guarantees is that
-- what was stored is what the engine's documents say, that what was booked is exactly what was disclosed,
-- and that a performance fee's high-water-mark chain and accrual chain cannot be broken.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. A fee schedule is versioned per key (consecutive), immutable, hashed, quoted in a SIMULATED asset,
--      never backdated, effective from a strictly later instant than its predecessor, published by a person
--      or a service (never an agent), and its summary columns are its canonical document's (FE001, FE002).
--   2. A quote disclosure is immutable and hashed, names a published schedule by its hash, its columns are
--      its document's, no amount in it is negative, its totals are exactly the reference notional plus or
--      minus the itemized spread, fees and network cost, and its explicit fees never exceed the schedule's
--      configured caps (CHECK, FE003).
--   3. A disclosure is booked at most once. A booking's amounts are its disclosure's; its journal carries
--      exactly those amounts on the payer's FEES_SPREAD / FEES_PLATFORM / FEES_VENUE / FEES_NETWORK
--      accounts -- and, for a booking the fee ledger posted itself, the spread on SPREAD_REVENUE and the
--      explicit fees on PLATFORM_FEE_REVENUE and nothing else; the effect that applied the journal is
--      APPLIED (FE004). A booking is reversed only by a COMPENSATION of exactly its amounts, once (FE005).
--      Every charged spread is therefore discoverable from fee_bookings and fee_quote_disclosures alone.
--   4. Performance fee periods chain per (account, strategy): one OPEN period at a time; a later period
--      opens where its predecessor's crystallized statement says (net NAV, high-water mark, period end);
--      flows and accrual lines are append-only, in time order, and never precede a recorded valuation; the
--      running accrual is always the last line's; every non-zero line is posted (PERFORMANCE_FEE_ACCRUAL,
--      PERFORMANCE_FEE_CRYSTALLIZATION) by an APPLIED FEES effect with exactly its amount (FE006, FE007).
--      A crystallized period carries its hashed statement, whose figures are its columns', and a reversal
--      is a COMPENSATION of exactly the charged fee (FE008). Identities are frozen and the status machine
--      OPEN -> CRYSTALLIZED | CANCELLED, CRYSTALLIZED -> REVERSED is enforced, each change bound to a
--      transition row (FE009, AU001).
--
-- Custom SQLSTATEs: FE001 fee schedule not the next version or effective too early, FE002 fee schedule
-- columns disagree with its document or asset, FE003 quote disclosure disagrees with its schedule or
-- document, FE004 fee booking disagrees with its disclosure, journal or effect, FE005 fee booking changed
-- outside its one legal reversal or reversed by the wrong journal, FE006 performance period or flow out of
-- its chain, FE007 performance line out of its chain or its posting, FE008 performance outcome disagreeing
-- with its statement or journals, FE009 performance period identity changed or illegal status change.

-- ---------------------------------------------------------------------------
-- 1. Fee schedules
-- ---------------------------------------------------------------------------

CREATE TABLE fee_schedules (
    id                     uuid PRIMARY KEY,
    schedule_key           text NOT NULL CHECK (schedule_key ~ '^[a-z0-9][a-z0-9_.:/-]{0,127}$'),
    version                integer NOT NULL CHECK (version >= 1),
    -- fees.Schedule.Version: the schedule's own name for this price list.
    label                  text NOT NULL CHECK (length(label) BETWEEN 1 AND 100),
    quote_asset_id         uuid NOT NULL REFERENCES assets(id),
    quote_decimals         smallint NOT NULL CHECK (quote_decimals BETWEEN 0 AND 18),
    spread_bps             integer NOT NULL CHECK (spread_bps BETWEEN 0 AND 9999),
    percentage_fee_bps     integer NOT NULL CHECK (percentage_fee_bps BETWEEN 0 AND 10000),
    max_total_fee_bps      integer NOT NULL CHECK (max_total_fee_bps BETWEEN 0 AND 10000),
    max_total_fee          numeric(38,0) NOT NULL CHECK (max_total_fee >= 0),
    -- The fees.Schedule canonical form; schedule_hash is what every quote priced under it records.
    schedule               bytea NOT NULL,
    schedule_hash          text NOT NULL,
    effective_from         timestamptz NOT NULL,
    published_at           timestamptz NOT NULL,
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    created_by_actor_id    text NOT NULL CHECK (length(created_by_actor_id) BETWEEN 1 AND 200),
    reason                 text NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (schedule_key, version),
    UNIQUE (schedule_key, label),
    CONSTRAINT fee_schedules_hash CHECK (encode(sha256(schedule), 'hex') = schedule_hash),
    CONSTRAINT fee_schedules_not_backdated CHECK (effective_from >= published_at)
);
CREATE INDEX fee_schedules_effective_idx ON fee_schedules (schedule_key, effective_from DESC);
CREATE TRIGGER fee_schedules_immutable BEFORE UPDATE OR DELETE ON fee_schedules
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_fee_schedule_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    prev   record;
    doc    jsonb;
    akind  text;
BEGIN
    SELECT version, effective_from INTO prev FROM fee_schedules WHERE schedule_key = NEW.schedule_key
     ORDER BY version DESC LIMIT 1;
    IF NEW.version <> coalesce(prev.version, 0) + 1 THEN
        RAISE EXCEPTION 'FEE_SCHEDULE_VERSION_GAP: schedule % version % is not the next after %',
            NEW.schedule_key, NEW.version, coalesce(prev.version, 0) USING ERRCODE = 'FE001';
    END IF;
    IF prev.effective_from IS NOT NULL AND NEW.effective_from <= prev.effective_from THEN
        RAISE EXCEPTION 'FEE_SCHEDULE_EFFECTIVE_TOO_EARLY: schedule % version % must take effect after %',
            NEW.schedule_key, NEW.version, prev.effective_from USING ERRCODE = 'FE001';
    END IF;
    SELECT kind INTO akind FROM assets WHERE id = NEW.quote_asset_id;
    IF akind IS DISTINCT FROM 'SIMULATED' THEN
        RAISE EXCEPTION 'FEE_SCHEDULE_MISMATCH: a fee schedule is quoted in a SIMULATED asset, not %', akind USING ERRCODE = 'FE002';
    END IF;
    doc := convert_from(NEW.schedule, 'UTF8')::jsonb;
    IF (doc ->> 'kind') IS DISTINCT FROM 'fees.schedule.v1'
       OR (doc ->> 'version') IS DISTINCT FROM NEW.label
       OR (doc ->> 'quote_asset') IS DISTINCT FROM NEW.quote_asset_id::text
       OR (doc ->> 'quote_decimals')::smallint IS DISTINCT FROM NEW.quote_decimals
       OR (doc ->> 'spread_bps')::integer IS DISTINCT FROM NEW.spread_bps
       OR (doc ->> 'percentage_fee_bps')::integer IS DISTINCT FROM NEW.percentage_fee_bps
       OR (doc ->> 'max_total_fee_bps')::integer IS DISTINCT FROM NEW.max_total_fee_bps
       OR (doc ->> 'max_total_fee')::numeric IS DISTINCT FROM NEW.max_total_fee THEN
        RAISE EXCEPTION 'FEE_SCHEDULE_MISMATCH: schedule % version % does not say what its columns say',
            NEW.schedule_key, NEW.version USING ERRCODE = 'FE002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_schedule_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER fee_schedules_is_next BEFORE INSERT ON fee_schedules
    FOR EACH ROW EXECUTE FUNCTION cp_fee_schedule_is_next();

-- ---------------------------------------------------------------------------
-- 2. Quote disclosures
-- ---------------------------------------------------------------------------

CREATE TABLE fee_quote_disclosures (
    id                     uuid PRIMARY KEY,
    schedule_id            uuid NOT NULL REFERENCES fee_schedules(id),
    schedule_hash          text NOT NULL,
    subsystem              text NOT NULL CHECK (subsystem IN (
                               'SANDBOX_FUNDING','STABLE_ASSET','MARGIN','DERIVATIVES','EQUITIES','ORDERS',
                               'TRANSFERS','TREASURY','FEES','MARKETPLACE','SANDBOX_RUNTIME','AGENT_AUTHORITY',
                               'WITHDRAWALS','BRIDGE','PREDICTION_MARKETS')),
    idempotency_key        text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash           bytea NOT NULL CHECK (length(request_hash) = 32),
    account_id             uuid REFERENCES accounts(id),
    base_asset_id          uuid REFERENCES assets(id),
    quote_asset_id         uuid NOT NULL REFERENCES assets(id),
    side                   text NOT NULL CHECK (side IN ('BUY','SELL')),
    base_quantity          numeric(38,0) NOT NULL CHECK (base_quantity > 0),
    reference_mantissa     numeric(38,0) NOT NULL CHECK (reference_mantissa > 0),
    reference_scale        integer NOT NULL CHECK (reference_scale BETWEEN 0 AND 38),
    execution_mantissa     numeric NOT NULL CHECK (execution_mantissa >= 0 AND execution_mantissa = trunc(execution_mantissa)),
    execution_scale        integer NOT NULL CHECK (execution_scale BETWEEN 0 AND 38),
    effective_mantissa     numeric NOT NULL CHECK (effective_mantissa >= 0 AND effective_mantissa = trunc(effective_mantissa)),
    effective_scale        integer NOT NULL CHECK (effective_scale BETWEEN 0 AND 38),
    notional_at_reference  numeric(38,0) NOT NULL CHECK (notional_at_reference >= 0),
    notional_at_execution  numeric(38,0) NOT NULL CHECK (notional_at_execution >= 0),
    spread_bps             integer NOT NULL CHECK (spread_bps BETWEEN 0 AND 9999),
    spread_amount          numeric(38,0) NOT NULL CHECK (spread_amount >= 0),
    -- The fixed fee, the percentage fee and any minimum-fee top-up.
    platform_fee           numeric(38,0) NOT NULL CHECK (platform_fee >= 0),
    strategy_fee           numeric(38,0) NOT NULL CHECK (strategy_fee >= 0),
    venue_fee              numeric(38,0) NOT NULL CHECK (venue_fee >= 0),
    explicit_fees_total    numeric(38,0) NOT NULL CHECK (explicit_fees_total >= 0),
    network_fee_asset_id   uuid REFERENCES assets(id),
    network_fee            numeric(38,0) NOT NULL CHECK (network_fee >= 0),
    network_in_quote       boolean NOT NULL,
    slippage_reserve       numeric(38,0) NOT NULL CHECK (slippage_reserve >= 0),
    total_debit            numeric(38,0) NOT NULL CHECK (total_debit >= 0),
    total_credit           numeric(38,0) NOT NULL CHECK (total_credit >= 0),
    max_total_fee_bps      integer NOT NULL CHECK (max_total_fee_bps BETWEEN 0 AND 10000),
    max_bps_amount         numeric(38,0) NOT NULL CHECK (max_bps_amount >= 0),
    max_total_fee          numeric(38,0) NOT NULL CHECK (max_total_fee >= 0),
    -- The complete fees.Disclosure, canonical form: everything above is recomputable from it.
    disclosure             bytea NOT NULL,
    disclosure_hash        text NOT NULL,
    quoted_at              timestamptz NOT NULL,
    valid_until            timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (subsystem, idempotency_key),
    CONSTRAINT fee_quote_disclosures_hash CHECK (encode(sha256(disclosure), 'hex') = disclosure_hash),
    CONSTRAINT fee_quote_disclosures_validity CHECK (valid_until > quoted_at),
    CONSTRAINT fee_quote_disclosures_fee_lines CHECK (explicit_fees_total = platform_fee + strategy_fee + venue_fee),
    CONSTRAINT fee_quote_disclosures_network_asset CHECK (network_fee = 0 OR network_fee_asset_id IS NOT NULL),
    -- Conservation: a total is the reference notional plus or minus exactly the itemized charges.
    CONSTRAINT fee_quote_disclosures_buy_conserved CHECK (side <> 'BUY' OR (
        total_credit = 0 AND notional_at_execution = notional_at_reference + spread_amount
        AND total_debit = notional_at_reference + spread_amount + explicit_fees_total
                          + CASE WHEN network_in_quote THEN network_fee ELSE 0 END)),
    CONSTRAINT fee_quote_disclosures_sell_conserved CHECK (side <> 'SELL' OR (
        total_debit = 0 AND notional_at_execution = notional_at_reference - spread_amount
        AND total_credit = notional_at_reference - spread_amount - explicit_fees_total
                           - CASE WHEN network_in_quote THEN network_fee ELSE 0 END)),
    -- No fee above the configured maximum.
    CONSTRAINT fee_quote_disclosures_absolute_cap CHECK (max_total_fee = 0 OR explicit_fees_total <= max_total_fee),
    CONSTRAINT fee_quote_disclosures_bps_cap CHECK (max_total_fee_bps = 0 OR explicit_fees_total <= max_bps_amount),
    CONSTRAINT fee_quote_disclosures_bps_cap_bound CHECK (
        max_total_fee_bps = 0 OR max_bps_amount * 10000 <= (notional_at_reference + 1) * max_total_fee_bps + 9999)
);
CREATE INDEX fee_quote_disclosures_schedule_idx ON fee_quote_disclosures (schedule_id);
CREATE INDEX fee_quote_disclosures_account_idx ON fee_quote_disclosures (account_id, quoted_at DESC) WHERE account_id IS NOT NULL;
CREATE TRIGGER fee_quote_disclosures_immutable BEFORE UPDATE OR DELETE ON fee_quote_disclosures
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_fee_disclosure_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    s       record;
    d       jsonb;
    plat    numeric;
    strat   numeric;
    venue   numeric;
BEGIN
    SELECT schedule_hash, label, quote_asset_id, quote_decimals, max_total_fee_bps, max_total_fee
      INTO s FROM fee_schedules WHERE id = NEW.schedule_id;
    IF s.schedule_hash IS DISTINCT FROM NEW.schedule_hash OR s.quote_asset_id IS DISTINCT FROM NEW.quote_asset_id
       OR s.max_total_fee_bps IS DISTINCT FROM NEW.max_total_fee_bps OR s.max_total_fee IS DISTINCT FROM NEW.max_total_fee THEN
        RAISE EXCEPTION 'FEE_DISCLOSURE_MISMATCH: disclosure % does not name schedule % as published', NEW.id, NEW.schedule_id
            USING ERRCODE = 'FE003';
    END IF;
    d := convert_from(NEW.disclosure, 'UTF8')::jsonb;
    SELECT coalesce(sum((l ->> 'charged')::numeric) FILTER (WHERE l ->> 'component' IN ('FIXED_FEE','PERCENTAGE_FEE')), 0),
           coalesce(sum((l ->> 'charged')::numeric) FILTER (WHERE l ->> 'component' = 'STRATEGY_FEE'), 0),
           coalesce(sum((l ->> 'charged')::numeric) FILTER (WHERE l ->> 'component' = 'VENUE_PASS_THROUGH'), 0)
      INTO plat, strat, venue
      FROM jsonb_array_elements(d -> 'fees' -> 'lines') AS l;
    plat := plat + (d -> 'fees' ->> 'minimum_fee_top_up')::numeric;
    IF (d ->> 'kind') IS DISTINCT FROM 'fees.quote_disclosure.v1'
       OR (d ->> 'schedule_hash') IS DISTINCT FROM NEW.schedule_hash
       OR (d ->> 'schedule_version') IS DISTINCT FROM s.label
       OR (d ->> 'quote_asset') IS DISTINCT FROM NEW.quote_asset_id::text
       OR (d -> 'input' ->> 'quote_decimals')::integer IS DISTINCT FROM s.quote_decimals::integer
       OR (d -> 'input' ->> 'side') IS DISTINCT FROM NEW.side
       OR (d -> 'input' ->> 'base_qty')::numeric IS DISTINCT FROM NEW.base_quantity
       OR (d -> 'reference_price' ->> 'mantissa')::numeric IS DISTINCT FROM NEW.reference_mantissa
       OR (d -> 'reference_price' ->> 'scale')::integer IS DISTINCT FROM NEW.reference_scale
       OR (d -> 'execution_price' ->> 'mantissa')::numeric IS DISTINCT FROM NEW.execution_mantissa
       OR (d -> 'execution_price' ->> 'scale')::integer IS DISTINCT FROM NEW.execution_scale
       OR (d -> 'effective_price' ->> 'mantissa')::numeric IS DISTINCT FROM NEW.effective_mantissa
       OR (d -> 'effective_price' ->> 'scale')::integer IS DISTINCT FROM NEW.effective_scale
       OR (d ->> 'notional_at_reference')::numeric IS DISTINCT FROM NEW.notional_at_reference
       OR (d ->> 'notional_at_execution')::numeric IS DISTINCT FROM NEW.notional_at_execution
       OR (d -> 'spread' ->> 'bps')::integer IS DISTINCT FROM NEW.spread_bps
       OR (d -> 'spread' ->> 'amount')::numeric IS DISTINCT FROM NEW.spread_amount
       OR (d -> 'fees' ->> 'total')::numeric IS DISTINCT FROM NEW.explicit_fees_total
       OR plat IS DISTINCT FROM NEW.platform_fee OR strat IS DISTINCT FROM NEW.strategy_fee OR venue IS DISTINCT FROM NEW.venue_fee
       OR (d -> 'fees' ->> 'max_total_fee_bps')::integer IS DISTINCT FROM NEW.max_total_fee_bps
       OR (d -> 'fees' ->> 'max_total_fee_bps_amount')::numeric IS DISTINCT FROM NEW.max_bps_amount
       OR (d -> 'fees' ->> 'max_total_fee')::numeric IS DISTINCT FROM NEW.max_total_fee
       OR (d -> 'network' ->> 'amount')::numeric IS DISTINCT FROM NEW.network_fee
       OR (d -> 'network' ->> 'in_quote_asset')::boolean IS DISTINCT FROM NEW.network_in_quote
       OR (NEW.network_fee > 0 AND (d -> 'network' ->> 'asset') IS DISTINCT FROM NEW.network_fee_asset_id::text)
       OR (d -> 'slippage_reserve' ->> 'amount')::numeric IS DISTINCT FROM NEW.slippage_reserve
       OR (d ->> 'total_debit')::numeric IS DISTINCT FROM NEW.total_debit
       OR (d ->> 'total_credit')::numeric IS DISTINCT FROM NEW.total_credit THEN
        RAISE EXCEPTION 'FEE_DISCLOSURE_MISMATCH: disclosure % does not say what its columns say', NEW.id
            USING ERRCODE = 'FE003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_disclosure_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER fee_quote_disclosures_match BEFORE INSERT ON fee_quote_disclosures
    FOR EACH ROW EXECUTE FUNCTION cp_fee_disclosure_matches();

-- ---------------------------------------------------------------------------
-- 3. Bookings: every charged component on its own account, exactly once
-- ---------------------------------------------------------------------------

CREATE TABLE fee_bookings (
    id                               uuid PRIMARY KEY,
    disclosure_id                    uuid NOT NULL UNIQUE REFERENCES fee_quote_disclosures(id),
    account_id                       uuid NOT NULL REFERENCES accounts(id),
    mode                             text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    -- POSTED: the fee ledger posted its own journal under its own FEES effect. EMBEDDED: the fee entries
    -- are inside the caller's journal (a trade's SIM_TRADE), applied by the caller's effect.
    booking_mode                     text NOT NULL CHECK (booking_mode IN ('POSTED','EMBEDDED')),
    journal_kind                     text NOT NULL CHECK (journal_kind IN (
                                         'SIM_TRADE','P2P_TRANSFER','LIQUIDATION','OPTION_PREMIUM','DERIVATIVE_MARGIN_MOVE',
                                         'MARGIN_BORROW','EQUITY_SETTLEMENT','STABLE_ISSUANCE','STABLE_REDEMPTION',
                                         'BRIDGE_SOURCE_FINALIZED','PREDICTION_SET_ISSUANCE','SANDBOX_WITHDRAWAL')),
    journal_transaction_id           uuid NOT NULL REFERENCES journal_transactions(id),
    effect_id                        uuid NOT NULL REFERENCES economic_effects(id),
    intent_id                        uuid REFERENCES economic_intents(id),
    quote_asset_id                   uuid NOT NULL REFERENCES assets(id),
    network_fee_asset_id             uuid REFERENCES assets(id),
    spread_amount                    numeric(38,0) NOT NULL CHECK (spread_amount >= 0),
    platform_fee                     numeric(38,0) NOT NULL CHECK (platform_fee >= 0),
    strategy_fee                     numeric(38,0) NOT NULL CHECK (strategy_fee >= 0),
    venue_fee                        numeric(38,0) NOT NULL CHECK (venue_fee >= 0),
    network_fee                      numeric(38,0) NOT NULL CHECK (network_fee >= 0),
    status                           text NOT NULL CHECK (status IN ('BOOKED','REVERSED')),
    evidence_ref                     text NOT NULL,
    evidence_sha256                  bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    booked_at                        timestamptz NOT NULL,
    reversal_journal_transaction_id  uuid UNIQUE REFERENCES journal_transactions(id),
    reversal_effect_id               uuid REFERENCES economic_effects(id),
    reversal_reason_code             text CHECK (reversal_reason_code IS NULL OR reversal_reason_code ~ '^[A-Z][A-Z0-9_]{2,63}$'),
    reversed_by_actor_type           text CHECK (reversed_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    reversed_by_actor_id             text,
    reversed_at                      timestamptz,
    correlation_id                   text,
    created_at                       timestamptz NOT NULL DEFAULT now(),
    updated_at                       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fee_bookings_charges_something CHECK (spread_amount + platform_fee + strategy_fee + venue_fee + network_fee > 0),
    CONSTRAINT fee_bookings_network_asset CHECK (network_fee = 0 OR network_fee_asset_id IS NOT NULL),
    CONSTRAINT fee_bookings_reversal_complete CHECK (
        (status = 'REVERSED') = (reversal_journal_transaction_id IS NOT NULL)
        AND (status <> 'REVERSED' OR (reversal_effect_id IS NOT NULL AND reversal_reason_code IS NOT NULL
             AND reversed_by_actor_type IS NOT NULL AND reversed_by_actor_id IS NOT NULL AND reversed_at IS NOT NULL)))
);
CREATE INDEX fee_bookings_account_idx ON fee_bookings (account_id, booked_at DESC);
CREATE INDEX fee_bookings_journal_idx ON fee_bookings (journal_transaction_id);
CREATE INDEX fee_bookings_booked_idx ON fee_bookings (booked_at);
CREATE TRIGGER fee_bookings_updated_at BEFORE UPDATE ON fee_bookings FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- The payer's FEES_* debits (and, for a POSTED booking, the platform's revenue legs) in a journal.
-- +goose StatementBegin
CREATE FUNCTION cp_fee_journal_legs(p_journal uuid, p_account uuid, p_quote uuid, p_network uuid)
RETURNS TABLE (spread numeric, platform numeric, venue numeric, network numeric, wallet_out numeric,
               spread_revenue numeric, fee_revenue numeric, receivable numeric, others bigint)
LANGUAGE sql STABLE AS $$
    SELECT
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p_account AND la.code = 'FEES_SPREAD'
                                         AND e.side = 'DEBIT' AND e.asset_id = p_quote), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p_account AND la.code = 'FEES_PLATFORM'
                                         AND e.side = 'DEBIT' AND e.asset_id = p_quote), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p_account AND la.code = 'FEES_VENUE'
                                         AND e.side = 'DEBIT' AND e.asset_id = p_quote), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p_account AND la.code = 'FEES_NETWORK'
                                         AND e.side = 'DEBIT' AND e.asset_id = p_network), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p_account AND la.code = 'WALLET'
                                         AND e.side = 'CREDIT'), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'SPREAD_REVENUE'
                                         AND e.side = 'CREDIT' AND e.asset_id = p_quote), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_REVENUE'
                                         AND e.side = 'CREDIT' AND e.asset_id = p_quote), 0),
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_RECEIVABLE'
                                         AND e.side = 'DEBIT' AND e.asset_id = p_quote), 0),
        count(*) FILTER (WHERE NOT (
            (la.owner_type = 'CUSTOMER' AND la.owner_id = p_account
             AND la.code IN ('FEES_SPREAD','FEES_PLATFORM','FEES_VENUE','FEES_NETWORK','WALLET'))
            OR (la.owner_type = 'PLATFORM' AND la.code IN ('SPREAD_REVENUE','PLATFORM_FEE_REVENUE','PLATFORM_FEE_RECEIVABLE'))))
    FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
    WHERE e.transaction_id = p_journal;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_journal_legs(uuid, uuid, uuid, uuid) SET search_path = pg_catalog, public, pg_temp;

-- +goose StatementBegin
CREATE FUNCTION cp_fee_booking_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    d     record;
    j     record;
    eff   record;
    legs  record;
    rev   numeric;
BEGIN
    IF NEW.status IS DISTINCT FROM 'BOOKED' OR NEW.reversal_journal_transaction_id IS NOT NULL OR NEW.reversal_effect_id IS NOT NULL
       OR NEW.reversal_reason_code IS NOT NULL OR NEW.reversed_at IS NOT NULL THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: booking % must be created BOOKED with no reversal', NEW.id USING ERRCODE = 'FE004';
    END IF;
    SELECT account_id, quote_asset_id, network_fee_asset_id, spread_amount, platform_fee, strategy_fee, venue_fee, network_fee, valid_until
      INTO d FROM fee_quote_disclosures WHERE id = NEW.disclosure_id;
    IF NOT FOUND OR (d.account_id IS NOT NULL AND d.account_id <> NEW.account_id)
       OR d.quote_asset_id <> NEW.quote_asset_id OR d.network_fee_asset_id IS DISTINCT FROM NEW.network_fee_asset_id
       OR d.spread_amount <> NEW.spread_amount OR d.platform_fee <> NEW.platform_fee OR d.strategy_fee <> NEW.strategy_fee
       OR d.venue_fee <> NEW.venue_fee OR d.network_fee <> NEW.network_fee OR NEW.booked_at > d.valid_until THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: booking % does not book disclosure % as disclosed', NEW.id, NEW.disclosure_id
            USING ERRCODE = 'FE004';
    END IF;
    SELECT kind, reference_type, reference_id, reversal_of INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    IF j.kind IS DISTINCT FROM NEW.journal_kind OR j.reversal_of IS NOT NULL THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: booking % names journal % of kind %, not %',
            NEW.id, NEW.journal_transaction_id, j.kind, NEW.journal_kind USING ERRCODE = 'FE004';
    END IF;
    SELECT status, subsystem, effect_kind, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: booking % names effect % which did not apply journal %',
            NEW.id, NEW.effect_id, NEW.journal_transaction_id USING ERRCODE = 'FE004';
    END IF;
    SELECT * INTO legs FROM cp_fee_journal_legs(NEW.journal_transaction_id, NEW.account_id, NEW.quote_asset_id, NEW.network_fee_asset_id);
    IF legs.spread <> NEW.spread_amount OR legs.platform <> NEW.platform_fee + NEW.strategy_fee OR legs.venue <> NEW.venue_fee
       OR legs.network <> NEW.network_fee THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: journal % does not charge account % exactly what booking % disclosed',
            NEW.journal_transaction_id, NEW.account_id, NEW.id USING ERRCODE = 'FE004';
    END IF;
    rev := NEW.platform_fee + NEW.strategy_fee;
    IF NEW.booking_mode = 'POSTED' THEN
        IF eff.subsystem IS DISTINCT FROM 'FEES' OR eff.effect_kind IS DISTINCT FROM 'FEE_CHARGE'
           OR j.reference_type IS DISTINCT FROM 'fee_charge' OR j.reference_id IS DISTINCT FROM NEW.disclosure_id::text
           OR legs.others <> 0 OR legs.spread_revenue <> NEW.spread_amount OR legs.fee_revenue <> rev
           OR legs.receivable <> NEW.spread_amount + rev
           OR legs.wallet_out <> NEW.spread_amount + rev + NEW.venue_fee + NEW.network_fee THEN
            RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: posted booking % journal % carries something other than its own charges',
                NEW.id, NEW.journal_transaction_id USING ERRCODE = 'FE004';
        END IF;
    ELSIF legs.spread_revenue < NEW.spread_amount OR legs.fee_revenue < rev THEN
        RAISE EXCEPTION 'FEE_BOOKING_MISMATCH: journal % does not credit the platform the revenue booking % discloses',
            NEW.journal_transaction_id, NEW.id USING ERRCODE = 'FE004';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_booking_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER fee_bookings_match BEFORE INSERT ON fee_bookings
    FOR EACH ROW EXECUTE FUNCTION cp_fee_booking_matches();

-- +goose StatementBegin
CREATE FUNCTION cp_fee_booking_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.disclosure_id, NEW.account_id, NEW.mode, NEW.booking_mode, NEW.journal_kind, NEW.journal_transaction_id,
        NEW.effect_id, NEW.intent_id, NEW.quote_asset_id, NEW.network_fee_asset_id, NEW.spread_amount, NEW.platform_fee,
        NEW.strategy_fee, NEW.venue_fee, NEW.network_fee, NEW.evidence_ref, NEW.evidence_sha256, NEW.booked_at,
        NEW.correlation_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.disclosure_id, OLD.account_id, OLD.mode, OLD.booking_mode, OLD.journal_kind, OLD.journal_transaction_id,
        OLD.effect_id, OLD.intent_id, OLD.quote_asset_id, OLD.network_fee_asset_id, OLD.spread_amount, OLD.platform_fee,
        OLD.strategy_fee, OLD.venue_fee, OLD.network_fee, OLD.evidence_ref, OLD.evidence_sha256, OLD.booked_at,
        OLD.correlation_id, OLD.created_at) THEN
        RAISE EXCEPTION 'FEE_BOOKING_FROZEN: booking % may change only by its reversal', OLD.id USING ERRCODE = 'FE005';
    END IF;
    IF NOT (OLD.status = 'BOOKED' AND NEW.status = 'REVERSED') THEN
        RAISE EXCEPTION 'FEE_BOOKING_FROZEN: booking % cannot move % -> %', OLD.id, OLD.status, NEW.status USING ERRCODE = 'FE005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_booking_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER fee_bookings_guard BEFORE UPDATE ON fee_bookings FOR EACH ROW EXECUTE FUNCTION cp_fee_booking_guard();

-- A reversal is a COMPENSATION of exactly the booked charges. Deferred: the journal, the effect and the
-- booking are written by different statements of one transaction.
-- +goose StatementBegin
CREATE FUNCTION cp_fee_booking_reversal_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    j     record;
    eff   record;
    legs  record;
    rev   numeric;
BEGIN
    SELECT kind, reversal_of, reference_type, reference_id INTO j FROM journal_transactions WHERE id = NEW.reversal_journal_transaction_id;
    IF j.kind IS DISTINCT FROM 'COMPENSATION' OR j.reversal_of IS DISTINCT FROM NEW.journal_transaction_id
       OR j.reference_type IS DISTINCT FROM 'fee_charge_reversal' OR j.reference_id IS DISTINCT FROM NEW.id::text THEN
        RAISE EXCEPTION 'FEE_REVERSAL_MISMATCH: booking % names reversal % which does not compensate journal %',
            NEW.id, NEW.reversal_journal_transaction_id, NEW.journal_transaction_id USING ERRCODE = 'FE005';
    END IF;
    rev := NEW.platform_fee + NEW.strategy_fee;
    SELECT
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'WALLET' AND e.side = 'DEBIT'), 0) AS refund,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'FEES_SPREAD' AND e.side = 'CREDIT'), 0) AS spread,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'FEES_PLATFORM' AND e.side = 'CREDIT'), 0) AS platform,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'FEES_VENUE' AND e.side = 'CREDIT'), 0) AS venue,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'FEES_NETWORK' AND e.side = 'CREDIT'), 0) AS network,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'SPREAD_REVENUE' AND e.side = 'DEBIT'), 0) AS spread_revenue,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_REVENUE' AND e.side = 'DEBIT'), 0) AS fee_revenue,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_RECEIVABLE' AND e.side = 'CREDIT'), 0) AS receivable,
        count(*) AS n
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.reversal_journal_transaction_id;
    IF legs.spread <> NEW.spread_amount OR legs.platform <> rev OR legs.venue <> NEW.venue_fee OR legs.network <> NEW.network_fee
       OR legs.refund <> NEW.spread_amount + rev + NEW.venue_fee + NEW.network_fee
       OR legs.spread_revenue <> NEW.spread_amount OR legs.fee_revenue <> rev OR legs.receivable <> NEW.spread_amount + rev THEN
        RAISE EXCEPTION 'FEE_REVERSAL_MISMATCH: reversal % does not return exactly what booking % charged',
            NEW.reversal_journal_transaction_id, NEW.id USING ERRCODE = 'FE005';
    END IF;
    IF NEW.booking_mode = 'POSTED' THEN
        SELECT status, compensation_journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.effect_id;
        IF NEW.reversal_effect_id <> NEW.effect_id OR eff.status IS DISTINCT FROM 'COMPENSATED'
           OR eff.compensation_journal_transaction_id IS DISTINCT FROM NEW.reversal_journal_transaction_id THEN
            RAISE EXCEPTION 'FEE_REVERSAL_MISMATCH: booking % is REVERSED but its effect was not compensated by journal %',
                NEW.id, NEW.reversal_journal_transaction_id USING ERRCODE = 'FE005';
        END IF;
    ELSE
        SELECT status, subsystem, effect_kind, journal_transaction_id INTO eff FROM economic_effects WHERE id = NEW.reversal_effect_id;
        IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'FEES' OR eff.effect_kind IS DISTINCT FROM 'FEE_REFUND'
           OR eff.journal_transaction_id IS DISTINCT FROM NEW.reversal_journal_transaction_id THEN
            RAISE EXCEPTION 'FEE_REVERSAL_MISMATCH: booking % names refund effect % which did not apply journal %',
                NEW.id, NEW.reversal_effect_id, NEW.reversal_journal_transaction_id USING ERRCODE = 'FE005';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_fee_booking_reversal_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER fee_bookings_reversal_matches AFTER UPDATE OF status ON fee_bookings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'REVERSED')
    EXECUTE FUNCTION cp_fee_booking_reversal_matches();

CREATE TABLE fee_booking_transitions (
    id              uuid PRIMARY KEY,
    booking_id      uuid NOT NULL REFERENCES fee_bookings(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX fee_booking_transitions_idx ON fee_booking_transitions (booking_id, occurred_at);
CREATE TRIGGER fee_booking_transitions_immutable BEFORE UPDATE OR DELETE ON fee_booking_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
-- BOOKED -> REVERSED is acyclic, so the accumulating edge binding of 00733 applies.
CREATE TRIGGER fee_booking_transitions_flag_edge AFTER INSERT ON fee_booking_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('booking_id', 'from_status', 'to_status', 'fee_booking_status');
CREATE CONSTRAINT TRIGGER fee_bookings_require_transition AFTER UPDATE OF status ON fee_bookings
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'fee_booking_status');

-- ---------------------------------------------------------------------------
-- 4. Performance fee periods (sandbox accounting only)
-- ---------------------------------------------------------------------------

CREATE TABLE performance_fee_periods (
    id                                     uuid PRIMARY KEY,
    account_id                             uuid NOT NULL REFERENCES accounts(id),
    strategy_ref                           text NOT NULL CHECK (strategy_ref ~ '^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,127}$'),
    period_seq                             integer NOT NULL CHECK (period_seq >= 1),
    previous_period_id                     uuid REFERENCES performance_fee_periods(id),
    mode                                   text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    fee_asset_id                           uuid NOT NULL REFERENCES assets(id),
    terms_version                          text NOT NULL CHECK (length(terms_version) BETWEEN 1 AND 100),
    period_start                           timestamptz NOT NULL,
    period_end                             timestamptz NOT NULL,
    opening_nav_minor                      bigint NOT NULL CHECK (opening_nav_minor >= 0),
    prior_hwm_minor                        bigint NOT NULL,
    hurdle_annual_bps                      integer NOT NULL CHECK (hurdle_annual_bps BETWEEN 0 AND 10000),
    fee_rate_bps                           integer NOT NULL CHECK (fee_rate_bps BETWEEN 0 AND 10000),
    crystallization                        text NOT NULL CHECK (crystallization = 'PERIOD_END'),
    prior_accrued_fee_minor                bigint NOT NULL CHECK (prior_accrued_fee_minor >= 0),
    terms_hash                             text NOT NULL CHECK (length(terms_hash) = 64),
    status                                 text NOT NULL CHECK (status IN ('OPEN','CRYSTALLIZED','CANCELLED','REVERSED')),
    accrued_fee_minor                      bigint NOT NULL CHECK (accrued_fee_minor >= 0),
    last_valuation_at                      timestamptz,
    statement                              bytea,
    statement_hash                         text,
    closing_nav_gross_minor                bigint,
    net_flows_minor                        bigint,
    gross_pnl_minor                        bigint,
    hwm_adjusted_minor                     bigint,
    fee_base_minor                         bigint CHECK (fee_base_minor IS NULL OR fee_base_minor >= 0),
    fee_amount_minor                       bigint CHECK (fee_amount_minor IS NULL OR fee_amount_minor >= 0),
    closing_nav_net_minor                  bigint,
    hwm_after_minor                        bigint,
    crystallization_journal_transaction_id uuid UNIQUE REFERENCES journal_transactions(id),
    crystallization_effect_id              uuid UNIQUE REFERENCES economic_effects(id),
    crystallized_at                        timestamptz,
    reversal_journal_transaction_id        uuid UNIQUE REFERENCES journal_transactions(id),
    reversal_reason_code                   text CHECK (reversal_reason_code IS NULL OR reversal_reason_code ~ '^[A-Z][A-Z0-9_]{2,63}$'),
    reversed_at                            timestamptz,
    opened_by_actor_type                   text NOT NULL CHECK (opened_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    opened_by_actor_id                     text NOT NULL CHECK (length(opened_by_actor_id) BETWEEN 1 AND 200),
    idempotency_key                        text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash                           bytea NOT NULL CHECK (length(request_hash) = 32),
    created_at                             timestamptz NOT NULL DEFAULT now(),
    updated_at                             timestamptz NOT NULL DEFAULT now(),
    UNIQUE (account_id, strategy_ref, period_seq),
    UNIQUE (account_id, idempotency_key),
    CONSTRAINT performance_fee_periods_window CHECK (period_end > period_start),
    -- A high-water mark below the opening NAV would charge, in this period, performance earned before it.
    CONSTRAINT performance_fee_periods_hwm CHECK (prior_hwm_minor >= opening_nav_minor),
    CONSTRAINT performance_fee_periods_statement_hash CHECK (statement IS NULL OR encode(sha256(statement), 'hex') = statement_hash),
    CONSTRAINT performance_fee_periods_crystallized_shape CHECK (
        (status = 'CRYSTALLIZED' OR status = 'REVERSED') = (statement IS NOT NULL AND statement_hash IS NOT NULL
            AND fee_amount_minor IS NOT NULL AND hwm_after_minor IS NOT NULL AND closing_nav_net_minor IS NOT NULL
            AND closing_nav_gross_minor IS NOT NULL AND net_flows_minor IS NOT NULL AND gross_pnl_minor IS NOT NULL
            AND hwm_adjusted_minor IS NOT NULL AND fee_base_minor IS NOT NULL AND crystallized_at IS NOT NULL
            AND crystallization_effect_id IS NOT NULL)),
    CONSTRAINT performance_fee_periods_charge_posted CHECK (
        statement IS NULL OR (fee_amount_minor > 0) = (crystallization_journal_transaction_id IS NOT NULL)),
    CONSTRAINT performance_fee_periods_settled_accrual CHECK (status = 'OPEN' OR accrued_fee_minor = 0),
    CONSTRAINT performance_fee_periods_reversal_shape CHECK (
        (status = 'REVERSED') = (reversal_reason_code IS NOT NULL AND reversed_at IS NOT NULL)
        AND (status <> 'REVERSED' OR (reversal_journal_transaction_id IS NOT NULL) = (fee_amount_minor > 0)))
);
CREATE UNIQUE INDEX performance_fee_periods_one_open ON performance_fee_periods (account_id, strategy_ref) WHERE status = 'OPEN';
CREATE INDEX performance_fee_periods_chain_idx ON performance_fee_periods (account_id, strategy_ref, period_seq DESC);
CREATE TRIGGER performance_fee_periods_updated_at BEFORE UPDATE ON performance_fee_periods
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- +goose StatementBegin
CREATE FUNCTION cp_performance_period_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest_seq  integer;
    prev        record;
    asset       record;
    rival       boolean;
BEGIN
    IF NEW.status IS DISTINCT FROM 'OPEN' OR NEW.accrued_fee_minor <> NEW.prior_accrued_fee_minor OR NEW.statement IS NOT NULL
       OR NEW.last_valuation_at IS NOT NULL OR NEW.crystallized_at IS NOT NULL OR NEW.reversed_at IS NOT NULL
       OR NEW.crystallization_effect_id IS NOT NULL OR NEW.fee_amount_minor IS NOT NULL THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: period % must open OPEN with no valuation and no outcome', NEW.id
            USING ERRCODE = 'FE006';
    END IF;
    SELECT kind, decimals INTO asset FROM assets WHERE id = NEW.fee_asset_id;
    IF asset.kind IS DISTINCT FROM 'SIMULATED' OR asset.decimals < 2 THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: a performance fee is accounted in a SIMULATED asset with at least two decimals'
            USING ERRCODE = 'FE006';
    END IF;
    SELECT max(period_seq) INTO latest_seq FROM performance_fee_periods
     WHERE account_id = NEW.account_id AND strategy_ref = NEW.strategy_ref;
    IF NEW.period_seq <> coalesce(latest_seq, 0) + 1 THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: period % of (%, %) is not the next after %',
            NEW.period_seq, NEW.account_id, NEW.strategy_ref, coalesce(latest_seq, 0) USING ERRCODE = 'FE006';
    END IF;
    -- No live period may already continue from the same predecessor.
    SELECT EXISTS (SELECT 1 FROM performance_fee_periods
                    WHERE account_id = NEW.account_id AND strategy_ref = NEW.strategy_ref
                      AND previous_period_id IS NOT DISTINCT FROM NEW.previous_period_id
                      AND status <> 'CANCELLED' AND status <> 'REVERSED') INTO rival;
    IF rival THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: another live period of (%, %) already continues from %',
            NEW.account_id, NEW.strategy_ref, coalesce(NEW.previous_period_id::text, 'the start') USING ERRCODE = 'FE006';
    END IF;
    IF NEW.previous_period_id IS NULL THEN
        IF EXISTS (SELECT 1 FROM performance_fee_periods WHERE account_id = NEW.account_id AND strategy_ref = NEW.strategy_ref
                    AND status = 'CRYSTALLIZED') THEN
            RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: (%, %) has a crystallized period; a new one continues from it',
                NEW.account_id, NEW.strategy_ref USING ERRCODE = 'FE006';
        END IF;
        IF NEW.prior_hwm_minor <> NEW.opening_nav_minor THEN
            RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: a first period starts with its high-water mark at its opening NAV'
                USING ERRCODE = 'FE006';
        END IF;
    ELSE
        SELECT account_id, strategy_ref, status, period_end, closing_nav_net_minor, hwm_after_minor INTO prev
          FROM performance_fee_periods WHERE id = NEW.previous_period_id;
        IF prev.account_id IS DISTINCT FROM NEW.account_id OR prev.strategy_ref IS DISTINCT FROM NEW.strategy_ref
           OR prev.status IS DISTINCT FROM 'CRYSTALLIZED' OR NEW.period_start <> prev.period_end
           OR NEW.opening_nav_minor <> prev.closing_nav_net_minor OR NEW.prior_hwm_minor <> prev.hwm_after_minor
           OR NEW.prior_accrued_fee_minor <> 0 THEN
            RAISE EXCEPTION 'PERFORMANCE_PERIOD_CHAIN: period % does not open where its predecessor % crystallized',
                NEW.id, NEW.previous_period_id USING ERRCODE = 'FE006';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_performance_period_chain() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER performance_fee_periods_chain BEFORE INSERT ON performance_fee_periods
    FOR EACH ROW EXECUTE FUNCTION cp_performance_period_chain();

-- +goose StatementBegin
CREATE FUNCTION cp_performance_period_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF (NEW.id, NEW.account_id, NEW.strategy_ref, NEW.period_seq, NEW.previous_period_id, NEW.mode, NEW.fee_asset_id,
        NEW.terms_version, NEW.period_start, NEW.period_end, NEW.opening_nav_minor, NEW.prior_hwm_minor, NEW.hurdle_annual_bps,
        NEW.fee_rate_bps, NEW.crystallization, NEW.prior_accrued_fee_minor, NEW.terms_hash, NEW.opened_by_actor_type,
        NEW.opened_by_actor_id, NEW.idempotency_key, NEW.request_hash, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.account_id, OLD.strategy_ref, OLD.period_seq, OLD.previous_period_id, OLD.mode, OLD.fee_asset_id,
        OLD.terms_version, OLD.period_start, OLD.period_end, OLD.opening_nav_minor, OLD.prior_hwm_minor, OLD.hurdle_annual_bps,
        OLD.fee_rate_bps, OLD.crystallization, OLD.prior_accrued_fee_minor, OLD.terms_hash, OLD.opened_by_actor_type,
        OLD.opened_by_actor_id, OLD.idempotency_key, OLD.request_hash, OLD.created_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_FROZEN: the terms of period % never change', OLD.id USING ERRCODE = 'FE009';
    END IF;
    IF OLD.status IS DISTINCT FROM NEW.status AND NOT (
        (OLD.status = 'OPEN' AND NEW.status = 'CRYSTALLIZED') OR (OLD.status = 'OPEN' AND NEW.status = 'CANCELLED')
        OR (OLD.status = 'CRYSTALLIZED' AND NEW.status = 'REVERSED')) THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_FROZEN: period % cannot move % -> %', OLD.id, OLD.status, NEW.status
            USING ERRCODE = 'FE009';
    END IF;
    IF OLD.status <> 'OPEN' AND (NEW.accrued_fee_minor, NEW.last_valuation_at) IS DISTINCT FROM (OLD.accrued_fee_minor, OLD.last_valuation_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_FROZEN: period % is % and accrues nothing more', OLD.id, OLD.status
            USING ERRCODE = 'FE009';
    END IF;
    IF NOT (OLD.status = 'OPEN' AND NEW.status = 'CRYSTALLIZED')
       AND (NEW.statement, NEW.statement_hash, NEW.closing_nav_gross_minor, NEW.net_flows_minor, NEW.gross_pnl_minor,
            NEW.hwm_adjusted_minor, NEW.fee_base_minor, NEW.fee_amount_minor, NEW.closing_nav_net_minor, NEW.hwm_after_minor,
            NEW.crystallization_journal_transaction_id, NEW.crystallization_effect_id, NEW.crystallized_at)
           IS DISTINCT FROM
           (OLD.statement, OLD.statement_hash, OLD.closing_nav_gross_minor, OLD.net_flows_minor, OLD.gross_pnl_minor,
            OLD.hwm_adjusted_minor, OLD.fee_base_minor, OLD.fee_amount_minor, OLD.closing_nav_net_minor, OLD.hwm_after_minor,
            OLD.crystallization_journal_transaction_id, OLD.crystallization_effect_id, OLD.crystallized_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_FROZEN: the statement of period % is written once, when it crystallizes', OLD.id
            USING ERRCODE = 'FE009';
    END IF;
    IF NOT (OLD.status = 'CRYSTALLIZED' AND NEW.status = 'REVERSED')
       AND (NEW.reversal_journal_transaction_id, NEW.reversal_reason_code, NEW.reversed_at)
           IS DISTINCT FROM (OLD.reversal_journal_transaction_id, OLD.reversal_reason_code, OLD.reversed_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_PERIOD_FROZEN: the reversal of period % is written once, by CRYSTALLIZED -> REVERSED', OLD.id
            USING ERRCODE = 'FE009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_performance_period_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER performance_fee_periods_guard BEFORE UPDATE ON performance_fee_periods
    FOR EACH ROW EXECUTE FUNCTION cp_performance_period_guard();

CREATE TABLE performance_fee_period_transitions (
    id              uuid PRIMARY KEY,
    period_id       uuid NOT NULL REFERENCES performance_fee_periods(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    actor_id        text NOT NULL,
    reason          text NOT NULL CHECK (length(reason) > 0),
    occurred_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX performance_fee_period_transitions_idx ON performance_fee_period_transitions (period_id, occurred_at);
CREATE TRIGGER performance_fee_period_transitions_immutable BEFORE UPDATE OR DELETE ON performance_fee_period_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
-- OPEN -> CRYSTALLIZED | CANCELLED, CRYSTALLIZED -> REVERSED is acyclic: the accumulating binding applies.
CREATE TRIGGER performance_fee_period_transitions_flag_edge AFTER INSERT ON performance_fee_period_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edges('period_id', 'from_status', 'to_status', 'performance_period_status');
CREATE CONSTRAINT TRIGGER performance_fee_periods_require_transition AFTER UPDATE OF status ON performance_fee_periods
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edges('status', 'performance_period_status');

CREATE TABLE performance_fee_flows (
    id                      uuid PRIMARY KEY,
    period_id               uuid NOT NULL REFERENCES performance_fee_periods(id),
    seq                     integer NOT NULL CHECK (seq >= 1),
    occurred_at             timestamptz NOT NULL,
    -- A deposit is positive, a withdrawal negative; simultaneous flows are netted by the caller.
    amount_minor            bigint NOT NULL CHECK (amount_minor <> 0),
    nav_before_minor        bigint NOT NULL CHECK (nav_before_minor >= 0),
    recorded_by_actor_type  text NOT NULL CHECK (recorded_by_actor_type IN ('USER','OPERATOR','SERVICE','SYSTEM')),
    recorded_by_actor_id    text NOT NULL CHECK (length(recorded_by_actor_id) BETWEEN 1 AND 200),
    idempotency_key         text NOT NULL CHECK (length(idempotency_key) BETWEEN 1 AND 256),
    request_hash            bytea NOT NULL CHECK (length(request_hash) = 32),
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (period_id, seq),
    UNIQUE (period_id, idempotency_key),
    CONSTRAINT performance_fee_flows_withdrawal_bounded CHECK (amount_minor > 0 OR -amount_minor <= nav_before_minor)
);
CREATE TRIGGER performance_fee_flows_immutable BEFORE UPDATE OR DELETE ON performance_fee_flows
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_performance_flow_in_order() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p     record;
    last  record;
BEGIN
    SELECT status, period_start, period_end, last_valuation_at INTO p FROM performance_fee_periods WHERE id = NEW.period_id;
    SELECT seq, occurred_at INTO last FROM performance_fee_flows WHERE period_id = NEW.period_id ORDER BY seq DESC LIMIT 1;
    IF p.status IS DISTINCT FROM 'OPEN' OR NEW.occurred_at <= p.period_start OR NEW.occurred_at > p.period_end
       OR NEW.seq <> coalesce(last.seq, 0) + 1 OR (last.occurred_at IS NOT NULL AND NEW.occurred_at <= last.occurred_at)
       OR (p.last_valuation_at IS NOT NULL AND NEW.occurred_at <= p.last_valuation_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_FLOW_ORDER: flow % of period % is out of order or outside the open period',
            NEW.seq, NEW.period_id USING ERRCODE = 'FE006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_performance_flow_in_order() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER performance_fee_flows_in_order BEFORE INSERT ON performance_fee_flows
    FOR EACH ROW EXECUTE FUNCTION cp_performance_flow_in_order();

CREATE TABLE performance_fee_lines (
    id                       uuid PRIMARY KEY,
    period_id                uuid NOT NULL REFERENCES performance_fee_periods(id),
    seq                      integer NOT NULL CHECK (seq >= 1),
    kind                     text NOT NULL CHECK (kind IN ('ACCRUAL','REVERSAL','NO_CHANGE','CRYSTALLIZATION')),
    occurred_at              timestamptz NOT NULL,
    nav_minor                bigint NOT NULL CHECK (nav_minor >= 0),
    hwm_adjusted_minor       bigint NOT NULL,
    hurdle_threshold_minor   bigint NOT NULL,
    fee_base_minor           bigint NOT NULL CHECK (fee_base_minor >= 0),
    amount_minor             bigint NOT NULL,
    accrual_before_minor     bigint NOT NULL CHECK (accrual_before_minor >= 0),
    accrual_after_minor      bigint NOT NULL CHECK (accrual_after_minor >= 0),
    journal_transaction_id   uuid UNIQUE REFERENCES journal_transactions(id),
    effect_id                uuid NOT NULL UNIQUE REFERENCES economic_effects(id),
    created_at               timestamptz NOT NULL DEFAULT now(),
    UNIQUE (period_id, seq),
    CONSTRAINT performance_fee_lines_sign CHECK (
        (kind <> 'ACCRUAL' OR amount_minor > 0) AND (kind <> 'REVERSAL' OR amount_minor < 0)
        AND (kind <> 'NO_CHANGE' OR amount_minor = 0) AND (kind <> 'CRYSTALLIZATION' OR amount_minor >= 0)),
    CONSTRAINT performance_fee_lines_accrual CHECK (
        (kind = 'CRYSTALLIZATION' AND accrual_after_minor = 0 AND amount_minor = accrual_before_minor)
        OR (kind <> 'CRYSTALLIZATION' AND accrual_after_minor = accrual_before_minor + amount_minor)),
    -- Every non-zero line moved the ledger; a zero line moved nothing.
    CONSTRAINT performance_fee_lines_posted CHECK ((journal_transaction_id IS NULL) = (amount_minor = 0))
);
CREATE TRIGGER performance_fee_lines_immutable BEFORE UPDATE OR DELETE ON performance_fee_lines
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_performance_line_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    p      record;
    last   record;
    j      record;
    eff    record;
    legs   record;
    q      numeric;
    unit   numeric;
BEGIN
    SELECT account_id, status, prior_accrued_fee_minor, fee_asset_id INTO p FROM performance_fee_periods WHERE id = NEW.period_id;
    SELECT seq, kind, occurred_at, accrual_after_minor INTO last FROM performance_fee_lines
     WHERE period_id = NEW.period_id ORDER BY seq DESC LIMIT 1;
    IF p.status IS DISTINCT FROM 'OPEN' OR NEW.seq <> coalesce(last.seq, 0) + 1 OR last.kind = 'CRYSTALLIZATION'
       OR NEW.accrual_before_minor <> coalesce(last.accrual_after_minor, p.prior_accrued_fee_minor)
       OR (last.occurred_at IS NOT NULL AND NEW.occurred_at < last.occurred_at) THEN
        RAISE EXCEPTION 'PERFORMANCE_LINE_CHAIN: line % of period % does not continue its accrual chain', NEW.seq, NEW.period_id
            USING ERRCODE = 'FE007';
    END IF;
    SELECT status, subsystem, journal_transaction_id, posts_ledger INTO eff FROM economic_effects WHERE id = NEW.effect_id;
    IF eff.status IS DISTINCT FROM 'APPLIED' OR eff.subsystem IS DISTINCT FROM 'FEES'
       OR eff.journal_transaction_id IS DISTINCT FROM NEW.journal_transaction_id THEN
        RAISE EXCEPTION 'PERFORMANCE_LINE_CHAIN: line % of period % names effect % which did not apply its journal',
            NEW.seq, NEW.period_id, NEW.effect_id USING ERRCODE = 'FE007';
    END IF;
    IF NEW.journal_transaction_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT decimals INTO unit FROM assets WHERE id = p.fee_asset_id;
    q := abs(NEW.amount_minor)::numeric * power(10::numeric, unit - 2);
    SELECT kind INTO j FROM journal_transactions WHERE id = NEW.journal_transaction_id;
    SELECT
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_ACCRUED' AND e.side = 'DEBIT'), 0) AS accrued_dr,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_ACCRUED' AND e.side = 'CREDIT'), 0) AS accrued_cr,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_DEFERRED' AND e.side = 'DEBIT'), 0) AS deferred_dr,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_DEFERRED' AND e.side = 'CREDIT'), 0) AS deferred_cr,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p.account_id AND la.code = 'FEES_PERFORMANCE' AND e.side = 'DEBIT'), 0) AS charged,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = p.account_id AND la.code = 'WALLET' AND e.side = 'CREDIT'), 0) AS paid,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_REVENUE' AND e.side = 'CREDIT'), 0) AS revenue,
        coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_RECEIVABLE' AND e.side = 'DEBIT'), 0) AS receivable,
        count(*) AS n,
        coalesce(bool_and(e.asset_id = p.fee_asset_id), false) AS one_asset
      INTO legs
      FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id;
    IF NOT legs.one_asset
       OR (NEW.kind = 'ACCRUAL' AND (j.kind IS DISTINCT FROM 'PERFORMANCE_FEE_ACCRUAL' OR legs.n <> 2
                                     OR legs.accrued_dr <> q OR legs.deferred_cr <> q))
       OR (NEW.kind = 'REVERSAL' AND (j.kind IS DISTINCT FROM 'PERFORMANCE_FEE_ACCRUAL' OR legs.n <> 2
                                      OR legs.deferred_dr <> q OR legs.accrued_cr <> q))
       OR (NEW.kind = 'CRYSTALLIZATION' AND (j.kind IS DISTINCT FROM 'PERFORMANCE_FEE_CRYSTALLIZATION' OR legs.n <> 6
                                             OR legs.charged <> q OR legs.paid <> q OR legs.deferred_dr <> q
                                             OR legs.accrued_cr <> q OR legs.receivable <> q OR legs.revenue <> q)) THEN
        RAISE EXCEPTION 'PERFORMANCE_LINE_CHAIN: journal % does not post line % of period % as its kind requires',
            NEW.journal_transaction_id, NEW.seq, NEW.period_id USING ERRCODE = 'FE007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_performance_line_chain() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER performance_fee_lines_chain BEFORE INSERT ON performance_fee_lines
    FOR EACH ROW EXECUTE FUNCTION cp_performance_line_chain();

CREATE TABLE performance_fee_attributions (
    id                  uuid PRIMARY KEY,
    period_id           uuid NOT NULL REFERENCES performance_fee_periods(id),
    strategy            text NOT NULL CHECK (length(strategy) BETWEEN 1 AND 128),
    gross_pnl_minor     bigint NOT NULL,
    eligible            boolean NOT NULL,
    fee_share_minor     bigint NOT NULL CHECK (fee_share_minor >= 0),
    remainder_minor     bigint NOT NULL CHECK (remainder_minor BETWEEN 0 AND 1),
    created_at          timestamptz NOT NULL DEFAULT now(),
    UNIQUE (period_id, strategy),
    CONSTRAINT performance_fee_attributions_eligible CHECK (eligible = (gross_pnl_minor > 0) AND (eligible OR fee_share_minor = 0))
);
CREATE TRIGGER performance_fee_attributions_immutable BEFORE UPDATE OR DELETE ON performance_fee_attributions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- The outcome of a period agrees with its statement, its lines and its journals. Deferred to COMMIT: the
-- lines, the postings, the attribution and the period row are written by different statements.
-- +goose StatementBegin
CREATE FUNCTION cp_performance_outcome_matches() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    last    record;
    st      jsonb;
    shares  record;
    j       record;
    legs    record;
    q       numeric;
    unit    numeric;
BEGIN
    SELECT kind, amount_minor, accrual_after_minor, occurred_at, journal_transaction_id, effect_id INTO last
      FROM performance_fee_lines WHERE period_id = NEW.id ORDER BY seq DESC LIMIT 1;
    IF NEW.accrued_fee_minor <> coalesce(last.accrual_after_minor, NEW.prior_accrued_fee_minor)
       OR NEW.last_valuation_at IS DISTINCT FROM last.occurred_at THEN
        RAISE EXCEPTION 'PERFORMANCE_OUTCOME_MISMATCH: period % carries accrual % but its last line says %',
            NEW.id, NEW.accrued_fee_minor, last.accrual_after_minor USING ERRCODE = 'FE008';
    END IF;
    IF NEW.status = 'OPEN' OR NEW.status = 'CANCELLED' THEN
        IF last.kind = 'CRYSTALLIZATION' THEN
            RAISE EXCEPTION 'PERFORMANCE_OUTCOME_MISMATCH: period % has a crystallization line but is %', NEW.id, NEW.status
                USING ERRCODE = 'FE008';
        END IF;
        RETURN NULL;
    END IF;
    st := convert_from(NEW.statement, 'UTF8')::jsonb;
    IF last.kind IS DISTINCT FROM 'CRYSTALLIZATION' OR last.amount_minor <> NEW.fee_amount_minor
       OR last.journal_transaction_id IS DISTINCT FROM NEW.crystallization_journal_transaction_id
       OR last.effect_id <> NEW.crystallization_effect_id
       OR (st ->> 'kind') IS DISTINCT FROM 'fees.performance_statement.v1'
       OR (st ->> 'mode') IS DISTINCT FROM 'SANDBOX_CALCULATION'
       OR (st ->> 'account_ref') IS DISTINCT FROM (NEW.account_id::text || '/' || NEW.strategy_ref)
       OR (st ->> 'terms_version') IS DISTINCT FROM NEW.terms_version
       OR (st ->> 'opening_nav')::numeric * 100 <> NEW.opening_nav_minor
       OR (st ->> 'hwm_before')::numeric * 100 <> NEW.prior_hwm_minor
       OR (st ->> 'closing_nav_gross')::numeric * 100 <> NEW.closing_nav_gross_minor
       OR (st ->> 'net_flows')::numeric * 100 <> NEW.net_flows_minor
       OR (st ->> 'gross_pnl')::numeric * 100 <> NEW.gross_pnl_minor
       OR (st ->> 'hwm_adjusted')::numeric * 100 <> NEW.hwm_adjusted_minor
       OR (st ->> 'fee_base')::numeric * 100 <> NEW.fee_base_minor
       OR (st ->> 'fee_rate_bps')::integer <> NEW.fee_rate_bps
       OR (st ->> 'fee_amount')::numeric * 100 <> NEW.fee_amount_minor
       OR (st ->> 'closing_nav_net')::numeric * 100 <> NEW.closing_nav_net_minor
       OR (st ->> 'hwm_after')::numeric * 100 <> NEW.hwm_after_minor
       OR NEW.closing_nav_net_minor <> NEW.closing_nav_gross_minor - NEW.fee_amount_minor
       OR NEW.hwm_after_minor <> greatest(NEW.hwm_adjusted_minor, NEW.closing_nav_net_minor)
       OR NEW.gross_pnl_minor <> NEW.closing_nav_gross_minor - NEW.opening_nav_minor - NEW.net_flows_minor THEN
        RAISE EXCEPTION 'PERFORMANCE_OUTCOME_MISMATCH: period % does not carry the figures of its statement', NEW.id
            USING ERRCODE = 'FE008';
    END IF;
    SELECT count(*) AS n, coalesce(sum(fee_share_minor), 0) AS total INTO shares
      FROM performance_fee_attributions WHERE period_id = NEW.id;
    IF shares.n > 0 AND shares.total <> NEW.fee_amount_minor THEN
        RAISE EXCEPTION 'PERFORMANCE_OUTCOME_MISMATCH: the attribution of period % sums to %, not its fee %',
            NEW.id, shares.total, NEW.fee_amount_minor USING ERRCODE = 'FE008';
    END IF;
    IF NEW.status = 'REVERSED' AND NEW.fee_amount_minor > 0 THEN
        SELECT decimals INTO unit FROM assets WHERE id = NEW.fee_asset_id;
        q := NEW.fee_amount_minor::numeric * power(10::numeric, unit - 2);
        SELECT kind, reversal_of, reference_type, reference_id INTO j FROM journal_transactions WHERE id = NEW.reversal_journal_transaction_id;
        SELECT
            coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'WALLET' AND e.side = 'DEBIT'), 0) AS refund,
            coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id AND la.code = 'FEES_PERFORMANCE' AND e.side = 'CREDIT'), 0) AS uncharged,
            coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PERFORMANCE_FEE_REVENUE' AND e.side = 'DEBIT'), 0) AS revenue,
            coalesce(sum(e.quantity) FILTER (WHERE la.owner_type = 'PLATFORM' AND la.code = 'PLATFORM_FEE_RECEIVABLE' AND e.side = 'CREDIT'), 0) AS receivable,
            count(*) AS n
          INTO legs
          FROM journal_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.transaction_id = NEW.reversal_journal_transaction_id;
        IF j.kind IS DISTINCT FROM 'COMPENSATION' OR j.reversal_of IS DISTINCT FROM NEW.crystallization_journal_transaction_id
           OR j.reference_type IS DISTINCT FROM 'performance_fee_reversal' OR j.reference_id IS DISTINCT FROM NEW.id::text
           OR legs.n <> 4 OR legs.refund <> q OR legs.uncharged <> q OR legs.revenue <> q OR legs.receivable <> q THEN
            RAISE EXCEPTION 'PERFORMANCE_OUTCOME_MISMATCH: reversal % does not return exactly the fee of period %',
                NEW.reversal_journal_transaction_id, NEW.id USING ERRCODE = 'FE008';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_performance_outcome_matches() SET search_path = pg_catalog, public, pg_temp;
CREATE CONSTRAINT TRIGGER performance_fee_periods_outcome_matches AFTER UPDATE ON performance_fee_periods
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_performance_outcome_matches();

-- ---------------------------------------------------------------------------
-- 5. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON fee_schedules, fee_quote_disclosures, fee_bookings, fee_booking_transitions,
                        performance_fee_periods, performance_fee_period_transitions, performance_fee_flows,
                        performance_fee_lines, performance_fee_attributions TO cp_app;
GRANT UPDATE (status, reversal_journal_transaction_id, reversal_effect_id, reversal_reason_code, reversed_by_actor_type,
              reversed_by_actor_id, reversed_at) ON fee_bookings TO cp_app;
GRANT UPDATE (status, accrued_fee_minor, last_valuation_at, statement, statement_hash, closing_nav_gross_minor,
              net_flows_minor, gross_pnl_minor, hwm_adjusted_minor, fee_base_minor, fee_amount_minor, closing_nav_net_minor,
              hwm_after_minor, crystallization_journal_transaction_id, crystallization_effect_id, crystallized_at,
              reversal_journal_transaction_id, reversal_reason_code, reversed_at) ON performance_fee_periods TO cp_app;
GRANT SELECT ON fee_schedules, fee_quote_disclosures, fee_bookings, fee_booking_transitions, performance_fee_periods,
               performance_fee_period_transitions, performance_fee_flows, performance_fee_lines,
               performance_fee_attributions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: fee schedules, disclosures, bookings and performance statements are financial history
