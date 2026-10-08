-- +goose Up
-- Paper and sandbox prediction markets (docs/architecture/ADVANCED_FINANCIAL_MODEL.md §5, D-053;
-- internal/predmarket). This is NOT internal/prediction, the agent forecast ledger, which owns the
-- table prediction_outcomes; nothing here reads or writes it.
--
-- A market is an EVENT_OUTCOME instrument (outcomes in its terms, expiry = close time), one
-- PREDICTION_MARKET facility whose books are that market's segregated escrow, and one six-decimal
-- SIMULATED share asset per outcome, each with its own EVENT_OUTCOME instrument (base = the share,
-- quote = the collateral) for the internal book to trade. A complete set -- one share of every outcome
-- -- is issued for exactly one collateral unit and redeemed for exactly one. Settlement pays every share
-- of the winning outcome one collateral unit and retires every share of every outcome; a void refunds
-- each share floor(unit / N) collateral base units, rounded down once per holder, and the remainder
-- stays on the facility's books as a residual the settlement record discloses.
--
-- WHAT THIS MIGRATION MAKES TRUE IN THE DATABASE, INDEPENDENT OF ANY GO CODE
--
--   1. The lifecycle is one table (cp_prediction_market_transition_legal) used twice: no history row
--      can describe an edge outside it (CHECK) and no UPDATE can take one (trigger). Every status change
--      needs a transition row naming both endpoints in the same transaction (the 00726 edge binding).
--   2. A market is born DRAFT. Its identity is frozen: only the status and the resolution pointer move,
--      and the pointer moves only in the UPDATE that enters RESOLVED.
--   3. A market cannot become SETTLED without a PAYOUT record for its current resolution, nor VOIDED
--      without a REFUND record. A settlement record states its own conservation (escrow_before =
--      total_paid + residual), a payout leaves no residual, and a refund names floor(unit / N).
--   4. Collateral and outcome-share assets are six-decimal SIMULATED assets, the unit is one million
--      base units, and the outcome set is fixed once the market leaves DRAFT.
--   5. Resolutions, disputes, settlements, the outcome registry and the transition history are
--      append-only. A resolution is recorded only for a market awaiting one, a dispute only against
--      the market's current oracle resolution, and each resolution is disputed at most once.
--   6. prediction_positions is a view over the ledger, so holdings cannot drift from the books.
--
-- Custom SQLSTATEs: PM001 a prediction-market lifecycle step the table does not allow, PM002 a
-- market's identity changed or its resolution pointer moved outside a resolution, PM003 a market
-- settled or voided without the settlement record that pays it, PM004 a collateral or outcome-share
-- asset that is not a six-decimal SIMULATED asset, PM005 an outcome registered out of range or after
-- its market left DRAFT.

-- ---------------------------------------------------------------------------
-- 1. The lifecycle, stated once
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_market_transition_legal(p_from text, p_to text) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
    SELECT (p_from, p_to) IN (
        ('DRAFT','OPEN'), ('DRAFT','VOIDED'),
        ('OPEN','CLOSED'), ('OPEN','VOIDED'),
        ('CLOSED','RESOLVING'), ('CLOSED','VOIDED'),
        ('RESOLVING','RESOLVED'), ('RESOLVING','VOIDED'),
        ('RESOLVED','SETTLED'), ('RESOLVED','DISPUTED'), ('RESOLVED','VOIDED'),
        ('DISPUTED','RESOLVED'), ('DISPUTED','VOIDED'));
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_asset_is_simulated(p_asset uuid, p_role text) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    a record;
BEGIN
    SELECT kind, value_domain, decimals INTO a FROM assets WHERE id = p_asset;
    IF NOT FOUND OR a.kind IS DISTINCT FROM 'SIMULATED' OR a.value_domain IS DISTINCT FROM 'SIMULATED'
       OR a.decimals IS DISTINCT FROM 6 THEN
        RAISE EXCEPTION 'PREDICTION_ASSET_NOT_SIMULATED: % % is not a six-decimal SIMULATED asset', p_role, p_asset
            USING ERRCODE = 'PM004';
    END IF;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_asset_is_simulated(uuid, text) SET search_path = pg_catalog, public, pg_temp;

-- ---------------------------------------------------------------------------
-- 2. Markets
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_markets (
    id                     uuid PRIMARY KEY,
    symbol                 text NOT NULL UNIQUE CHECK (symbol ~ '^[A-Z0-9][A-Z0-9_-]{0,31}$'),
    question               text NOT NULL CHECK (length(question) BETWEEN 1 AND 500),
    mode                   text NOT NULL CHECK (mode IN ('PAPER','SANDBOX')),
    -- One facility per market: each market's escrow is its own segregated book.
    facility_id            uuid NOT NULL UNIQUE REFERENCES facilities(id),
    instrument_id          uuid NOT NULL UNIQUE REFERENCES instruments(id),
    collateral_asset_id    uuid NOT NULL REFERENCES assets(id),
    outcome_count          smallint NOT NULL CHECK (outcome_count BETWEEN 2 AND 64),
    -- Collateral base units one share pays: one collateral unit of a six-decimal asset.
    unit_quantity          numeric(38,0) NOT NULL CHECK (unit_quantity = 1000000),
    open_at                timestamptz NOT NULL,
    close_at               timestamptz NOT NULL,
    dispute_window_ms      bigint NOT NULL CHECK (dispute_window_ms BETWEEN 0 AND 2592000000),
    oracle_source          text NOT NULL CHECK (length(oracle_source) BETWEEN 1 AND 200),
    oracle_public_key      bytea NOT NULL CHECK (length(oracle_public_key) = 32),
    definition_hash        bytea NOT NULL CHECK (length(definition_hash) = 32),
    status                 text NOT NULL CHECK (status IN (
                               'DRAFT','OPEN','CLOSED','RESOLVING','RESOLVED','DISPUTED','SETTLED','VOIDED')),
    current_resolution_id  uuid,
    created_by_actor_type  text NOT NULL CHECK (created_by_actor_type = 'OPERATOR'),
    created_by_actor_id    text NOT NULL CHECK (length(created_by_actor_id) BETWEEN 1 AND 200),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT prediction_markets_trading_window CHECK (close_at > open_at),
    CONSTRAINT prediction_markets_resolution_present CHECK (
        current_resolution_id IS NOT NULL OR (status <> 'RESOLVED' AND status <> 'DISPUTED' AND status <> 'SETTLED'))
);
CREATE INDEX prediction_markets_due_idx ON prediction_markets (status, close_at);
CREATE TRIGGER prediction_markets_updated_at BEFORE UPDATE ON prediction_markets
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- 3. The transition history and its binding
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_market_transitions (
    id              uuid PRIMARY KEY,
    market_id       uuid NOT NULL REFERENCES prediction_markets(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type IN ('OPERATOR','SYSTEM')),
    actor_id        text NOT NULL CHECK (length(actor_id) BETWEEN 1 AND 200),
    reason          text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    evidence_ref    text,
    correlation_id  text,
    occurred_at     timestamptz NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    -- The birth row is NONE -> DRAFT; every other row is an edge of the table.
    CONSTRAINT prediction_market_transitions_edge_legal CHECK (
        (from_status = 'NONE' AND to_status = 'DRAFT') OR cp_prediction_market_transition_legal(from_status, to_status))
);
CREATE INDEX prediction_market_transitions_market_idx ON prediction_market_transitions (market_id, occurred_at, id);
CREATE TRIGGER prediction_market_transitions_immutable BEFORE UPDATE OR DELETE ON prediction_market_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();
CREATE TRIGGER prediction_market_transitions_flag_edge AFTER INSERT ON prediction_market_transitions
    FOR EACH ROW EXECUTE FUNCTION cp_flag_transition_edge('market_id', 'from_status', 'to_status', 'pm_status');
CREATE CONSTRAINT TRIGGER prediction_markets_require_transition AFTER UPDATE OF status ON prediction_markets
    DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
    EXECUTE FUNCTION cp_require_transition_edge('status', 'pm_status');

-- ---------------------------------------------------------------------------
-- 4. The outcome registry
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_outcomes_registry (
    id              uuid PRIMARY KEY,
    market_id       uuid NOT NULL REFERENCES prediction_markets(id),
    outcome_index   smallint NOT NULL CHECK (outcome_index BETWEEN 0 AND 63),
    symbol          text NOT NULL CHECK (symbol ~ '^[A-Z0-9][A-Z0-9_-]{0,23}$'),
    share_asset_id  uuid NOT NULL UNIQUE REFERENCES assets(id),
    instrument_id   uuid NOT NULL UNIQUE REFERENCES instruments(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (market_id, outcome_index),
    UNIQUE (market_id, symbol)
);
CREATE TRIGGER prediction_outcomes_registry_immutable BEFORE UPDATE OR DELETE ON prediction_outcomes_registry
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_outcome_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, outcome_count INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR m.status <> 'DRAFT' OR NEW.outcome_index >= m.outcome_count THEN
        RAISE EXCEPTION 'PREDICTION_OUTCOME_SET_FROZEN: outcome % of market % is out of range or its market is not a DRAFT',
            NEW.outcome_index, NEW.market_id USING ERRCODE = 'PM005';
    END IF;
    PERFORM cp_prediction_asset_is_simulated(NEW.share_asset_id, 'outcome share asset');
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_outcome_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_outcomes_registry_guard BEFORE INSERT ON prediction_outcomes_registry
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_outcome_guard();

-- ---------------------------------------------------------------------------
-- 5. Resolutions and disputes
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_resolutions (
    id                      uuid PRIMARY KEY,
    market_id               uuid NOT NULL REFERENCES prediction_markets(id),
    kind                    text NOT NULL CHECK (kind IN ('ORACLE','DISPUTE_UPHELD','DISPUTE_OVERTURNED')),
    outcome_index           smallint NOT NULL,
    supersedes_id           uuid,
    dispute_id              uuid,
    report_hash             bytea CHECK (report_hash IS NULL OR length(report_hash) = 32),
    report_source           text,
    report_observed_at      timestamptz,
    report_signature        bytea CHECK (report_signature IS NULL OR length(report_signature) = 64),
    evidence_ref            text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256         bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    accepted_by_actor_type  text NOT NULL CHECK (accepted_by_actor_type = 'OPERATOR'),
    accepted_by_actor_id    text NOT NULL CHECK (length(accepted_by_actor_id) BETWEEN 1 AND 200),
    reason                  text NOT NULL CHECK (length(reason) BETWEEN 1 AND 2000),
    accepted_at             timestamptz NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, market_id),
    UNIQUE (id, market_id, outcome_index),
    -- Oracle replay is idempotent because a report is accepted at most once per market.
    UNIQUE (market_id, report_hash),
    FOREIGN KEY (market_id, outcome_index) REFERENCES prediction_outcomes_registry (market_id, outcome_index),
    FOREIGN KEY (supersedes_id, market_id) REFERENCES prediction_resolutions (id, market_id),
    CONSTRAINT prediction_resolutions_report_whole CHECK (
        (report_hash IS NULL) = (report_signature IS NULL)
        AND (report_hash IS NULL) = (report_observed_at IS NULL)
        AND (report_hash IS NULL) = (report_source IS NULL)),
    CONSTRAINT prediction_resolutions_oracle_is_reported CHECK (
        kind <> 'ORACLE' OR (report_hash IS NOT NULL AND supersedes_id IS NULL AND dispute_id IS NULL)),
    CONSTRAINT prediction_resolutions_ruling_closes_a_dispute CHECK (
        kind = 'ORACLE' OR (dispute_id IS NOT NULL AND supersedes_id IS NOT NULL)),
    CONSTRAINT prediction_resolutions_overturn_is_reported CHECK (
        kind <> 'DISPUTE_OVERTURNED' OR report_hash IS NOT NULL)
);
CREATE TRIGGER prediction_resolutions_immutable BEFORE UPDATE OR DELETE ON prediction_resolutions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE prediction_markets ADD CONSTRAINT prediction_markets_current_resolution_fkey
    FOREIGN KEY (current_resolution_id, id) REFERENCES prediction_resolutions (id, market_id);

CREATE TABLE prediction_disputes (
    id                    uuid PRIMARY KEY,
    market_id             uuid NOT NULL REFERENCES prediction_markets(id),
    -- A resolution is disputed at most once.
    resolution_id         uuid NOT NULL UNIQUE,
    raised_by_actor_type  text NOT NULL CHECK (raised_by_actor_type = 'OPERATOR'),
    raised_by_actor_id    text NOT NULL CHECK (length(raised_by_actor_id) BETWEEN 1 AND 200),
    reason                text NOT NULL CHECK (length(reason) BETWEEN 8 AND 2000),
    evidence_ref          text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256       bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    raised_at             timestamptz NOT NULL,
    created_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (id, market_id),
    FOREIGN KEY (resolution_id, market_id) REFERENCES prediction_resolutions (id, market_id)
);
CREATE TRIGGER prediction_disputes_immutable BEFORE UPDATE OR DELETE ON prediction_disputes
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

ALTER TABLE prediction_resolutions ADD CONSTRAINT prediction_resolutions_dispute_fkey
    FOREIGN KEY (dispute_id, market_id) REFERENCES prediction_disputes (id, market_id);

-- A resolution is recorded only for a market awaiting one: an oracle resolution while RESOLVING, a
-- dispute ruling while DISPUTED, superseding the disputed resolution and naming the dispute against it.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_resolution_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, current_resolution_id INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND
       OR (NEW.kind = 'ORACLE' AND m.status <> 'RESOLVING')
       OR (NEW.kind <> 'ORACLE' AND (
              m.status <> 'DISPUTED'
              OR NEW.supersedes_id IS DISTINCT FROM m.current_resolution_id
              OR NOT EXISTS (SELECT 1 FROM prediction_disputes d
                              WHERE d.id = NEW.dispute_id AND d.resolution_id = NEW.supersedes_id))) THEN
        RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % is not awaiting a % resolution', NEW.market_id, NEW.kind
            USING ERRCODE = 'PM001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_resolution_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_resolutions_guard BEFORE INSERT ON prediction_resolutions
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_resolution_guard();

-- A dispute is raised only against the current ORACLE resolution of a RESOLVED market: a ruling is
-- final, and the UNIQUE on resolution_id makes each resolution disputable once.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_dispute_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, current_resolution_id INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR m.status <> 'RESOLVED' OR NEW.resolution_id IS DISTINCT FROM m.current_resolution_id
       OR NOT EXISTS (SELECT 1 FROM prediction_resolutions r WHERE r.id = NEW.resolution_id AND r.kind = 'ORACLE') THEN
        RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % has no current oracle resolution % that can be disputed',
            NEW.market_id, NEW.resolution_id USING ERRCODE = 'PM001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_dispute_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_disputes_guard BEFORE INSERT ON prediction_disputes
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_dispute_guard();

-- ---------------------------------------------------------------------------
-- 6. Settlements
-- ---------------------------------------------------------------------------

CREATE TABLE prediction_settlements (
    id                     uuid PRIMARY KEY,
    -- A market settles (or refunds) exactly once.
    market_id              uuid NOT NULL UNIQUE REFERENCES prediction_markets(id),
    kind                   text NOT NULL CHECK (kind IN ('PAYOUT','REFUND')),
    resolution_id          uuid,
    winning_outcome_index  smallint,
    outcome_count          smallint NOT NULL CHECK (outcome_count BETWEEN 2 AND 64),
    unit_quantity          numeric(38,0) NOT NULL CHECK (unit_quantity > 0),
    refund_rate_per_share  numeric(38,0),
    rounding_policy        text NOT NULL CHECK (rounding_policy IN ('EXACT','FLOOR_PER_HOLDER')),
    escrow_before          numeric(38,0) NOT NULL CHECK (escrow_before >= 0),
    total_paid             numeric(38,0) NOT NULL CHECK (total_paid >= 0),
    residual               numeric(38,0) NOT NULL CHECK (residual >= 0),
    holder_count           integer NOT NULL CHECK (holder_count >= 0),
    evidence_ref           text NOT NULL CHECK (length(evidence_ref) > 0),
    evidence_sha256        bytea NOT NULL CHECK (length(evidence_sha256) = 32),
    settled_by_actor_type  text NOT NULL CHECK (settled_by_actor_type = 'OPERATOR'),
    settled_by_actor_id    text NOT NULL CHECK (length(settled_by_actor_id) BETWEEN 1 AND 200),
    settled_at             timestamptz NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (resolution_id, market_id, winning_outcome_index)
        REFERENCES prediction_resolutions (id, market_id, outcome_index),
    -- Conservation, stated by the record itself: every base unit of escrow was paid or is the
    -- disclosed residual.
    CONSTRAINT prediction_settlements_conserves CHECK (escrow_before = total_paid + residual),
    CONSTRAINT prediction_settlements_payout_is_exact CHECK (kind <> 'PAYOUT' OR (
        residual = 0 AND resolution_id IS NOT NULL AND winning_outcome_index IS NOT NULL
        AND refund_rate_per_share IS NULL AND rounding_policy = 'EXACT')),
    CONSTRAINT prediction_settlements_refund_rate CHECK (kind <> 'REFUND' OR (
        resolution_id IS NULL AND winning_outcome_index IS NULL AND refund_rate_per_share IS NOT NULL
        AND refund_rate_per_share = floor(unit_quantity / outcome_count) AND rounding_policy = 'FLOOR_PER_HOLDER'))
);
CREATE TRIGGER prediction_settlements_immutable BEFORE UPDATE OR DELETE ON prediction_settlements
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- A payout record pays the market's current resolution of a RESOLVED market; a refund record belongs
-- to a market that is neither settled nor voided.
-- +goose StatementBegin
CREATE FUNCTION cp_prediction_settlement_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    m record;
BEGIN
    SELECT status, current_resolution_id, outcome_count INTO m FROM prediction_markets WHERE id = NEW.market_id;
    IF NOT FOUND OR NEW.outcome_count IS DISTINCT FROM m.outcome_count
       OR (NEW.kind = 'PAYOUT' AND (m.status <> 'RESOLVED' OR NEW.resolution_id IS DISTINCT FROM m.current_resolution_id))
       OR (NEW.kind = 'REFUND' AND (m.status = 'SETTLED' OR m.status = 'VOIDED')) THEN
        RAISE EXCEPTION 'PREDICTION_UNSETTLED: a % record does not pay market % as it stands', NEW.kind, NEW.market_id
            USING ERRCODE = 'PM003';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_settlement_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_settlements_guard BEFORE INSERT ON prediction_settlements
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_settlement_guard();

-- ---------------------------------------------------------------------------
-- 7. The market guard: birth, frozen identity, legal edges, settled means paid
-- ---------------------------------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION cp_prediction_market_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.status <> 'DRAFT' OR NEW.current_resolution_id IS NOT NULL THEN
            RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % must be born DRAFT with no resolution, not %', NEW.id, NEW.status
                USING ERRCODE = 'PM001';
        END IF;
        PERFORM cp_prediction_asset_is_simulated(NEW.collateral_asset_id, 'collateral asset');
        RETURN NEW;
    END IF;

    IF (NEW.id, NEW.symbol, NEW.question, NEW.mode, NEW.facility_id, NEW.instrument_id, NEW.collateral_asset_id,
        NEW.outcome_count, NEW.unit_quantity, NEW.open_at, NEW.close_at, NEW.dispute_window_ms, NEW.oracle_source,
        NEW.oracle_public_key, NEW.definition_hash, NEW.created_by_actor_type, NEW.created_by_actor_id, NEW.created_at)
       IS DISTINCT FROM
       (OLD.id, OLD.symbol, OLD.question, OLD.mode, OLD.facility_id, OLD.instrument_id, OLD.collateral_asset_id,
        OLD.outcome_count, OLD.unit_quantity, OLD.open_at, OLD.close_at, OLD.dispute_window_ms, OLD.oracle_source,
        OLD.oracle_public_key, OLD.definition_hash, OLD.created_by_actor_type, OLD.created_by_actor_id, OLD.created_at) THEN
        RAISE EXCEPTION 'PREDICTION_IDENTITY_FROZEN: market % may change only its status and its resolution', OLD.id
            USING ERRCODE = 'PM002';
    END IF;

    IF NEW.current_resolution_id IS DISTINCT FROM OLD.current_resolution_id
       AND NOT (NEW.status = 'RESOLVED' AND OLD.status <> 'RESOLVED' AND NEW.current_resolution_id IS NOT NULL) THEN
        RAISE EXCEPTION 'PREDICTION_IDENTITY_FROZEN: market % moved its resolution outside a transition into RESOLVED', OLD.id
            USING ERRCODE = 'PM002';
    END IF;

    IF NEW.status IS DISTINCT FROM OLD.status THEN
        IF NOT cp_prediction_market_transition_legal(OLD.status, NEW.status) THEN
            RAISE EXCEPTION 'PREDICTION_LIFECYCLE: market % cannot move % -> %', OLD.id, OLD.status, NEW.status
                USING ERRCODE = 'PM001';
        END IF;
        IF NEW.status = 'SETTLED' AND NOT EXISTS (
               SELECT 1 FROM prediction_settlements s
                WHERE s.market_id = NEW.id AND s.kind = 'PAYOUT' AND s.resolution_id = NEW.current_resolution_id) THEN
            RAISE EXCEPTION 'PREDICTION_UNSETTLED: market % cannot be SETTLED without the payout of its current resolution', NEW.id
                USING ERRCODE = 'PM003';
        END IF;
        IF NEW.status = 'VOIDED' AND NOT EXISTS (
               SELECT 1 FROM prediction_settlements s WHERE s.market_id = NEW.id AND s.kind = 'REFUND') THEN
            RAISE EXCEPTION 'PREDICTION_UNSETTLED: market % cannot be VOIDED without its refund record', NEW.id
                USING ERRCODE = 'PM003';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_prediction_market_guard() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER prediction_markets_guard BEFORE INSERT OR UPDATE ON prediction_markets
    FOR EACH ROW EXECUTE FUNCTION cp_prediction_market_guard();

-- ---------------------------------------------------------------------------
-- 8. Positions: a projection of the ledger, never a second copy of it
-- ---------------------------------------------------------------------------

-- Outcome shares are held in customer WALLET accounts. The value_domain predicate lets the planner use
-- ledger_accounts_value_domain_idx (00710); every share asset is SIMULATED (PM004).
CREATE VIEW prediction_positions AS
SELECT o.market_id,
       la.owner_id     AS account_id,
       o.outcome_index,
       o.symbol        AS outcome_symbol,
       o.share_asset_id,
       b.balance       AS quantity,
       b.updated_at    AS as_of
  FROM prediction_outcomes_registry o
  JOIN ledger_accounts la ON la.value_domain = 'SIMULATED' AND la.asset_id = o.share_asset_id
                         AND la.owner_type = 'CUSTOMER' AND la.code = 'WALLET'
  JOIN ledger_balances b ON b.ledger_account_id = la.id
 WHERE b.balance > 0;

-- ---------------------------------------------------------------------------
-- 9. Privileges
-- ---------------------------------------------------------------------------

GRANT SELECT, INSERT ON prediction_markets, prediction_market_transitions, prediction_outcomes_registry,
                        prediction_resolutions, prediction_disputes, prediction_settlements TO cp_app;
-- Only the status and the resolution pointer move; the guard decides when.
GRANT UPDATE (status, current_resolution_id) ON prediction_markets TO cp_app;
GRANT SELECT ON prediction_positions TO cp_app;
GRANT SELECT ON prediction_markets, prediction_market_transitions, prediction_outcomes_registry,
                prediction_resolutions, prediction_disputes, prediction_settlements, prediction_positions
             TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: prediction market history, its settlements and the escrow they explain are never dropped
