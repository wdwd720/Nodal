-- +goose Up
-- Derivatives hardening (internal/derivatives; amends 00741 without editing it).
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. Marks arrive in order. A mark is never observed after it was recorded, and a mark observed before
--      the instrument's latest recorded observation cannot be added behind it (per-instrument lock, so two
--      concurrent recorders cannot interleave). Whoever records marks cannot reach back past a price that
--      is already known and choose another for a moment that has passed.
--   2. A trade (PAPER or BOOK fill) is backed by a Risk V2 decision that allowed exactly it: an ALLOW for the
--      fill's own account, its own intent (both none for a book fill without an economic intent) and its own
--      instrument, under the DERIVATIVES capability, evaluated no later than the fill; and one decision backs
--      at most one trade.
--   3. A derivative contract's terms are frozen while it has open positions: no new terms version and no change
--      to the current terms row. Inserting a position share-locks the contract's terms row, so an amendment and
--      a first open cannot pass each other.
--   4. A final settlement price is explained. Every new settlement names its price rule:
--        EXPIRY_MARK        -- the latest mark observed AND recorded at or before expiry (an option's: those
--                              carrying an index), fresh at expiry (observed no more than the oracle max age
--                              before it); a future settles at that mark, an option at its index aligned to the
--                              tick grid half-even. The settlement names that mark and is exactly that price.
--        OPERATOR_OVERRIDE  -- a price one OPERATOR proposed after expiry, on the tick grid, within the bound
--                              stored with the proposal of the rule's reference price (the price the rule derives
--                              from the same mark, fresh or not), and a SECOND, distinct OPERATOR approved before
--                              settlement. The settlement names the proposal and is exactly its price.
--      Settlements recorded before this migration keep a NULL rule; no new one can.
--   5. A cash flow that takes value from the customer's WALLET names the capital reservation it consumed: of the
--      same account and asset, CONSUMED, for at least what the flow's journal took from the wallet (checked at
--      COMMIT). Losses, variation margin, funding and premiums are taken only from available funds -- never from
--      funds reserved or held for something else.
--   6. derivative_cash_flows is indexed for the per-account register, newest first.
--
-- Custom SQLSTATEs (prefix DV, continuing 00741's DV001-DV004): DV005 a mark observed before the instrument's
-- latest recorded observation, DV006 a trade on a risk decision that does not allow it (not ALLOW, another
-- account, intent or instrument, not DERIVATIVES, made after the trade), DV007 a terms change of a contract with
-- open positions, DV008 a settlement price the expiry rule does not explain (or a settlement naming no rule, or
-- not after expiry at the contract's price scale), DV009 an override proposal that is not for an expired,
-- unsettled contract, not on its tick grid at its price scale, or whose reference is not the rule's, DV010 dual
-- control: an override approved by its proposer, decided after settlement, or a settlement at an override no
-- second operator approved, DV011 a wallet collection without a consumed reservation covering it.

-- ---------------------------------------------------------------------------
-- 1. Marks arrive in order
-- ---------------------------------------------------------------------------

ALTER TABLE derivative_marks ADD CONSTRAINT derivative_marks_observed_before_recorded CHECK (observed_at <= recorded_at);

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_mark_in_order() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest timestamptz;
BEGIN
    -- One recorder per instrument at a time (two-key advisory lock: its own key space), so the check below
    -- sees every mark committed before this one.
    PERFORM pg_advisory_xact_lock(hashtext('cp_derivative_marks'), hashtext(NEW.instrument_id::text));
    SELECT max(observed_at) INTO latest FROM derivative_marks WHERE instrument_id = NEW.instrument_id;
    IF latest IS NOT NULL AND NEW.observed_at < latest THEN
        RAISE EXCEPTION 'DERIVATIVE_MARK_OUT_OF_ORDER: instrument % has a mark observed at %; one observed at % cannot be added behind it',
            NEW.instrument_id, latest, NEW.observed_at USING ERRCODE = 'DV005';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_mark_in_order() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_marks_in_order BEFORE INSERT ON derivative_marks
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_mark_in_order();

-- ---------------------------------------------------------------------------
-- 2. A trade is backed by the decision that allowed exactly it
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_fill_authorized() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    d advanced_risk_decisions%ROWTYPE;
BEGIN
    -- A trade without any decision is refused by derivative_fills_trades_carry_a_decision (00741).
    IF NEW.source NOT IN ('PAPER','BOOK') OR NEW.risk_decision_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO d FROM advanced_risk_decisions WHERE id = NEW.risk_decision_id;
    IF d.id IS NULL OR d.verdict <> 'ALLOW'
       OR d.account_id IS DISTINCT FROM NEW.account_id
       OR d.intent_id IS DISTINCT FROM NEW.intent_id
       OR (d.input #>> '{intent,instrument_id}') IS DISTINCT FROM NEW.instrument_id::text
       OR (d.input #>> '{intent,capability}') IS DISTINCT FROM 'DERIVATIVES'
       OR d.evaluated_at > NEW.occurred_at THEN
        RAISE EXCEPTION 'DERIVATIVE_FILL_UNAUTHORIZED: % fill % of account % on % names decision %, which does not allow it',
            NEW.source, NEW.id, NEW.account_id, NEW.instrument_id, NEW.risk_decision_id USING ERRCODE = 'DV006';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_fill_authorized() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_fills_authorized BEFORE INSERT ON derivative_fills
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_fill_authorized();
CREATE UNIQUE INDEX derivative_fills_one_trade_per_decision ON derivative_fills (risk_decision_id)
    WHERE source IN ('PAPER','BOOK');

-- ---------------------------------------------------------------------------
-- 3. Terms are frozen while a contract has open positions
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_terms_frozen() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_TABLE_NAME = 'instrument_versions' THEN
        IF NEW.version = 1 THEN
            RETURN NEW;
        END IF;
    ELSIF (to_jsonb(NEW) - 'updated_at') = (to_jsonb(OLD) - 'updated_at') THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM derivative_positions WHERE instrument_id = NEW.instrument_id AND status = 'OPEN') THEN
        RAISE EXCEPTION 'DERIVATIVE_TERMS_FROZEN: instrument % has open derivative positions; its terms cannot change until they are closed or settled',
            NEW.instrument_id USING ERRCODE = 'DV007';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_terms_frozen() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER instrument_versions_derivative_terms_frozen BEFORE INSERT ON instrument_versions
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_terms_frozen();
CREATE TRIGGER instrument_terms_derivative_terms_frozen BEFORE UPDATE ON instrument_terms
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_terms_frozen();

-- A new position holds its contract's terms row (FOR SHARE) until it commits: an amendment, which takes the
-- row FOR UPDATE, waits for it and then finds it open.
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_position_holds_terms() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM 1 FROM instrument_terms WHERE instrument_id = NEW.instrument_id FOR SHARE;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_position_holds_terms() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_positions_hold_terms BEFORE INSERT ON derivative_positions
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_position_holds_terms();

-- ---------------------------------------------------------------------------
-- 4. A final settlement price is explained: the expiry rule, or a dual-controlled override
-- ---------------------------------------------------------------------------

-- p_num / p_den rounded half to even, for p_num >= 0 and p_den > 0 (money.RoundHalfEven).
-- +goose StatementBegin
CREATE FUNCTION cp_div_half_even(p_num numeric, p_den numeric) RETURNS numeric
LANGUAGE plpgsql IMMUTABLE STRICT AS $$
DECLARE
    q numeric;
    r numeric;
BEGIN
    IF p_den <= 0 OR p_num < 0 THEN
        RAISE EXCEPTION 'cp_div_half_even: % / % is outside its domain', p_num, p_den;
    END IF;
    q := div(p_num, p_den);
    r := p_num - q * p_den;
    IF 2 * r > p_den OR (2 * r = p_den AND mod(q, 2) = 1) THEN
        q := q + 1;
    END IF;
    RETURN q;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_div_half_even(numeric, numeric) SET search_path = pg_catalog, public, pg_temp;

-- The mark the expiry rule reads (internal/derivatives expiryMark is the same query): of the instrument's
-- marks observed at or before expiry and recorded at or before expiry (an option's: those carrying an index),
-- the one observed last; ties by recording time, then id. NULL when there is none.
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_expiry_mark(p_instrument uuid) RETURNS derivative_marks
LANGUAGE sql STABLE AS $$
    SELECT m.* FROM derivative_marks m
      JOIN instrument_terms t ON t.instrument_id = m.instrument_id
     WHERE m.instrument_id = p_instrument AND t.expiry IS NOT NULL
       AND m.observed_at <= t.expiry AND m.recorded_at <= t.expiry
       AND (t.kind <> 'OPTION' OR m.index_mantissa IS NOT NULL)
     ORDER BY m.observed_at DESC, m.recorded_at DESC, m.id DESC
     LIMIT 1;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_expiry_mark(uuid) SET search_path = pg_catalog, public, pg_temp;

-- The price the expiry rule derives from a mark: a future's mark; an option's index expressed at the option's
-- price scale and aligned to its tick grid, half-even (internal/derivatives alignIndex).
-- +goose StatementBegin
CREATE FUNCTION cp_derivative_rule_price(p_instrument uuid, p_mark derivative_marks) RETURNS numeric
LANGUAGE plpgsql STABLE AS $$
DECLARE
    t instrument_terms%ROWTYPE;
BEGIN
    SELECT * INTO t FROM instrument_terms WHERE instrument_id = p_instrument;
    IF t.instrument_id IS NULL OR p_mark.id IS NULL THEN
        RETURN NULL;
    END IF;
    IF t.kind = 'OPTION' THEN
        IF p_mark.index_mantissa IS NULL THEN
            RETURN NULL;
        END IF;
        RETURN t.tick_size * cp_div_half_even(p_mark.index_mantissa * power(10::numeric, t.price_scale),
                                              power(10::numeric, p_mark.index_scale) * t.tick_size);
    END IF;
    RETURN p_mark.mark_mantissa;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_rule_price(uuid, derivative_marks) SET search_path = pg_catalog, public, pg_temp;

CREATE TABLE derivative_settlement_overrides (
    id                      uuid PRIMARY KEY,
    instrument_id           uuid NOT NULL REFERENCES instruments(id),
    -- The proposed settlement price at the contract's price scale (an option's: the underlying's).
    price_mantissa          numeric(38,0) NOT NULL CHECK (price_mantissa >= 0),
    price_scale             integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    -- The rule's reference at expiry, and the policy's bound in force when the proposal was made.
    reference_mark_id       uuid NOT NULL REFERENCES derivative_marks(id),
    reference_mantissa      numeric(38,0) NOT NULL CHECK (reference_mantissa >= 0),
    max_deviation_bps       bigint NOT NULL CHECK (max_deviation_bps BETWEEN 1 AND 10000),
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    evidence_ref            text NOT NULL CHECK (length(evidence_ref) BETWEEN 1 AND 500),
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    proposed_by_actor_type  text NOT NULL CHECK (proposed_by_actor_type = 'OPERATOR'),
    proposed_by_actor_id    text NOT NULL CHECK (length(proposed_by_actor_id) BETWEEN 1 AND 200),
    proposed_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- Exact: |price - reference| * 10 000 <= bound * reference (internal/derivatives withinBound).
    CONSTRAINT derivative_settlement_overrides_bounded CHECK (
        abs(price_mantissa - reference_mantissa) * 10000 <= max_deviation_bps * reference_mantissa)
);
CREATE INDEX derivative_settlement_overrides_instrument_idx ON derivative_settlement_overrides (instrument_id, proposed_at DESC, id DESC);
CREATE TRIGGER derivative_settlement_overrides_immutable BEFORE UPDATE OR DELETE ON derivative_settlement_overrides
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_override_proposal_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    t instrument_terms%ROWTYPE;
    m derivative_marks%ROWTYPE;
BEGIN
    SELECT * INTO t FROM instrument_terms WHERE instrument_id = NEW.instrument_id;
    IF t.instrument_id IS NULL OR t.kind NOT IN ('FUTURE','OPTION') OR t.expiry IS NULL OR NEW.proposed_at < t.expiry THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_INVALID: override % is not for an expired dated contract', NEW.id
            USING ERRCODE = 'DV009';
    END IF;
    IF EXISTS (SELECT 1 FROM derivative_settlements WHERE instrument_id = NEW.instrument_id) THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_INVALID: instrument % is already settled', NEW.instrument_id USING ERRCODE = 'DV009';
    END IF;
    IF NEW.price_scale <> t.price_scale OR mod(NEW.price_mantissa, t.tick_size) <> 0 THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_INVALID: override % is not on the contract''s tick grid at its price scale', NEW.id
            USING ERRCODE = 'DV009';
    END IF;
    m := cp_derivative_expiry_mark(NEW.instrument_id);
    IF m.id IS NULL OR m.id <> NEW.reference_mark_id
       OR NEW.reference_mantissa IS DISTINCT FROM cp_derivative_rule_price(NEW.instrument_id, m) THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_INVALID: override % is not bounded by the expiry rule''s reference', NEW.id
            USING ERRCODE = 'DV009';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_override_proposal_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_settlement_overrides_guard BEFORE INSERT ON derivative_settlement_overrides
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_override_proposal_guard();

CREATE TABLE derivative_settlement_override_decisions (
    id                     uuid PRIMARY KEY,
    override_id            uuid NOT NULL UNIQUE REFERENCES derivative_settlement_overrides(id),
    decision               text NOT NULL CHECK (decision IN ('APPROVED','REJECTED')),
    reason                 text NOT NULL CHECK (length(reason) BETWEEN 1 AND 1000),
    decided_by_actor_type  text NOT NULL CHECK (decided_by_actor_type = 'OPERATOR'),
    decided_by_actor_id    text NOT NULL CHECK (length(decided_by_actor_id) BETWEEN 1 AND 200),
    decided_at             timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER derivative_settlement_override_decisions_immutable BEFORE UPDATE OR DELETE ON derivative_settlement_override_decisions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_override_decision_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    o derivative_settlement_overrides%ROWTYPE;
BEGIN
    SELECT * INTO o FROM derivative_settlement_overrides WHERE id = NEW.override_id;
    IF o.id IS NULL THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_DUAL_CONTROL: override % does not exist', NEW.override_id USING ERRCODE = 'DV010';
    END IF;
    IF NEW.decision = 'APPROVED' AND NEW.decided_by_actor_id = o.proposed_by_actor_id THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_DUAL_CONTROL: override % was proposed by % and cannot be approved by them',
            o.id, o.proposed_by_actor_id USING ERRCODE = 'DV010';
    END IF;
    IF EXISTS (SELECT 1 FROM derivative_settlements WHERE instrument_id = o.instrument_id) THEN
        RAISE EXCEPTION 'DERIVATIVE_OVERRIDE_DUAL_CONTROL: instrument % is settled; override % can no longer be decided',
            o.instrument_id, o.id USING ERRCODE = 'DV010';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_override_decision_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_settlement_override_decisions_guard BEFORE INSERT ON derivative_settlement_override_decisions
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_override_decision_guard();

-- Settlements recorded before this migration have no rule (NULL passes a CHECK); every new one names it.
ALTER TABLE derivative_settlements
    ADD COLUMN price_rule text CHECK (price_rule IN ('EXPIRY_MARK','OPERATOR_OVERRIDE')),
    ADD COLUMN mark_id uuid REFERENCES derivative_marks(id),
    ADD COLUMN override_id uuid UNIQUE REFERENCES derivative_settlement_overrides(id);
ALTER TABLE derivative_settlements ADD CONSTRAINT derivative_settlements_override_iff_rule
    CHECK ((price_rule = 'OPERATOR_OVERRIDE') = (override_id IS NOT NULL));
ALTER TABLE derivative_settlements ADD CONSTRAINT derivative_settlements_ruled_names_its_mark
    CHECK (price_rule IS NULL OR mark_id IS NOT NULL);

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_settlement_explained() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    t instrument_terms%ROWTYPE;
    m derivative_marks%ROWTYPE;
    o derivative_settlement_overrides%ROWTYPE;
    d derivative_settlement_override_decisions%ROWTYPE;
BEGIN
    SELECT * INTO t FROM instrument_terms WHERE instrument_id = NEW.instrument_id;
    IF NEW.price_rule IS NULL OR t.instrument_id IS NULL OR t.expiry IS NULL OR NEW.settled_at < t.expiry
       OR NEW.price_scale <> t.price_scale THEN
        RAISE EXCEPTION 'DERIVATIVE_SETTLEMENT_UNEXPLAINED: settlement % of % names no price rule, or is not after expiry at the contract''s price scale',
            NEW.id, NEW.instrument_id USING ERRCODE = 'DV008';
    END IF;
    m := cp_derivative_expiry_mark(NEW.instrument_id);
    IF NEW.price_rule = 'EXPIRY_MARK' THEN
        IF m.id IS NULL OR NEW.mark_id IS DISTINCT FROM m.id OR NEW.override_id IS NOT NULL
           OR m.observed_at < t.expiry - t.oracle_max_age_ms * interval '1 millisecond'
           OR NEW.settlement_mantissa IS DISTINCT FROM cp_derivative_rule_price(NEW.instrument_id, m) THEN
            RAISE EXCEPTION 'DERIVATIVE_SETTLEMENT_UNEXPLAINED: settlement % of % at % is not the price the expiry rule derives from a fresh expiry mark',
                NEW.id, NEW.instrument_id, NEW.settlement_mantissa USING ERRCODE = 'DV008';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO o FROM derivative_settlement_overrides WHERE id = NEW.override_id;
    SELECT * INTO d FROM derivative_settlement_override_decisions WHERE override_id = NEW.override_id;
    IF o.id IS NULL OR o.instrument_id <> NEW.instrument_id OR o.price_mantissa <> NEW.settlement_mantissa
       OR o.price_scale <> NEW.price_scale OR NEW.mark_id IS DISTINCT FROM o.reference_mark_id
       OR m.id IS DISTINCT FROM o.reference_mark_id
       OR d.id IS NULL OR d.decision <> 'APPROVED' OR d.decided_by_actor_type <> 'OPERATOR'
       OR d.decided_by_actor_id = o.proposed_by_actor_id THEN
        RAISE EXCEPTION 'DERIVATIVE_SETTLEMENT_DUAL_CONTROL: settlement % of % is not at an override proposed by one operator and approved by another',
            NEW.id, NEW.instrument_id USING ERRCODE = 'DV010';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_settlement_explained() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER derivative_settlements_explained BEFORE INSERT ON derivative_settlements
    FOR EACH ROW EXECUTE FUNCTION cp_derivative_settlement_explained();

-- ---------------------------------------------------------------------------
-- 5. A wallet collection consumed a reservation covering it
-- ---------------------------------------------------------------------------

ALTER TABLE derivative_cash_flows ADD COLUMN reservation_id uuid UNIQUE REFERENCES asset_reservations(id);

-- +goose StatementBegin
CREATE FUNCTION cp_derivative_collection_reserved() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    taken numeric;
    r     asset_reservations%ROWTYPE;
BEGIN
    IF NEW.journal_transaction_id IS NULL THEN
        RETURN NULL;
    END IF;
    -- What the flow's journal took from the customer's WALLET in the flow's asset (net credits).
    SELECT coalesce(sum(CASE WHEN e.side = 'CREDIT' THEN e.quantity ELSE -e.quantity END), 0) INTO taken
      FROM journal_entries e
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.transaction_id = NEW.journal_transaction_id
       AND la.owner_type = 'CUSTOMER' AND la.owner_id = NEW.account_id
       AND la.code = 'WALLET' AND e.asset_id = NEW.settlement_asset_id;
    IF taken <= 0 THEN
        RETURN NULL;
    END IF;
    SELECT * INTO r FROM asset_reservations WHERE id = NEW.reservation_id;
    IF r.id IS NULL OR r.account_id <> NEW.account_id OR r.asset_id <> NEW.settlement_asset_id
       OR r.status <> 'CONSUMED' OR r.consumed_quantity < taken THEN
        RAISE EXCEPTION 'DERIVATIVE_COLLECTION_UNRESERVED: flow % took % from the wallet of account % without a consumed reservation covering it',
            NEW.id, taken, NEW.account_id USING ERRCODE = 'DV011';
    END IF;
    RETURN NULL;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_derivative_collection_reserved() SET search_path = pg_catalog, public, pg_temp;
-- Named to fire after derivative_cash_flows_match_journal (00741, DV004): a flow that disagrees with its own
-- journal is refused as that first.
CREATE CONSTRAINT TRIGGER derivative_cash_flows_wallet_take_reserved AFTER INSERT ON derivative_cash_flows
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cp_derivative_collection_reserved();

-- ---------------------------------------------------------------------------
-- 6. The per-account register, newest first
-- ---------------------------------------------------------------------------

CREATE INDEX derivative_cash_flows_account_idx ON derivative_cash_flows (account_id, occurred_at DESC, id DESC);

-- ---------------------------------------------------------------------------
-- 7. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON derivative_settlement_overrides, derivative_settlement_override_decisions TO cp_app;
GRANT SELECT ON derivative_settlement_overrides, derivative_settlement_override_decisions TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: settlements, overrides and the cash-flow register are financial history
