-- +goose Up
-- The canonical instrument model (advanced goal §4; ADR-0020; internal/instruments/canonical.go).
--
-- One instruments row per tradable thing, one type, and one versioned terms document. Spot crypto stays
-- SPOT_PAIR and a prediction-market contract is the EVENT_OUTCOME type V1 already reserved; the new types
-- are STABLE_VALUE, EQUITY, ETF, FUTURE, PERPETUAL, OPTION and SYNTHETIC.
--
-- instrument_versions is the append-only history of terms: every amendment (a symbol change, a corporate
-- action adjusting the multiplier, a new oracle source) is a new version with its own content hash, and
-- versions are consecutive. instrument_terms is the current version in normalized columns, so a query can
-- ask for "every perpetual with an 8-hour funding interval" without parsing JSON.
--
-- Every advanced type settles only in a SIMULATED asset in this build. That keeps the V1 promise that an
-- EVENT_OUTCOME instrument is never activatable over real value, while letting a sandbox prediction
-- market exist.
--
-- Custom SQLSTATEs: IN001 an advanced instrument settling in non-SIMULATED value, IN002 a terms version
-- that is not the next one.

ALTER TABLE instruments DROP CONSTRAINT instruments_type_check;
ALTER TABLE instruments ADD CONSTRAINT instruments_type_check CHECK (type IN (
    'SPOT_PAIR','EVENT_OUTCOME','STABLE_VALUE','EQUITY','ETF','FUTURE','PERPETUAL','OPTION','SYNTHETIC'));

-- +goose StatementBegin
CREATE FUNCTION cp_advanced_instrument_is_simulated() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    dom text;
BEGIN
    IF NEW.type = 'SPOT_PAIR' THEN
        RETURN NEW;
    END IF;
    SELECT value_domain INTO dom FROM assets WHERE id = NEW.settlement_asset_id;
    IF dom IS DISTINCT FROM 'SIMULATED' THEN
        RAISE EXCEPTION 'ADVANCED_INSTRUMENT_REAL_VALUE: % instrument % settles in % value; advanced instruments settle only in SIMULATED value in this build',
            NEW.type, NEW.id, coalesce(dom, 'no') USING ERRCODE = 'IN001';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_advanced_instrument_is_simulated() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER instruments_advanced_is_simulated BEFORE INSERT OR UPDATE OF type, settlement_asset_id ON instruments
    FOR EACH ROW EXECUTE FUNCTION cp_advanced_instrument_is_simulated();

CREATE TABLE instrument_versions (
    id              uuid PRIMARY KEY,
    instrument_id   uuid NOT NULL REFERENCES instruments(id),
    version         integer NOT NULL CHECK (version >= 1),
    terms           jsonb NOT NULL,
    content_hash    bytea NOT NULL CHECK (length(content_hash) = 32),
    reason          text NOT NULL CHECK (length(reason) > 0),
    actor_type      text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id        text NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (instrument_id, version)
);
CREATE TRIGGER instrument_versions_immutable BEFORE UPDATE OR DELETE ON instrument_versions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- +goose StatementBegin
CREATE FUNCTION cp_instrument_version_is_next() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    latest integer;
BEGIN
    SELECT max(version) INTO latest FROM instrument_versions WHERE instrument_id = NEW.instrument_id;
    IF NEW.version <> coalesce(latest, 0) + 1 THEN
        RAISE EXCEPTION 'TERMS_VERSION_GAP: instrument % version % is not the next after %', NEW.instrument_id, NEW.version, coalesce(latest, 0)
            USING ERRCODE = 'IN002';
    END IF;
    RETURN NEW;
END;
$$;
-- +goose StatementEnd
ALTER FUNCTION cp_instrument_version_is_next() SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER instrument_versions_consecutive BEFORE INSERT ON instrument_versions
    FOR EACH ROW EXECUTE FUNCTION cp_instrument_version_is_next();

CREATE TABLE instrument_terms (
    instrument_id             uuid PRIMARY KEY REFERENCES instruments(id),
    version                   integer NOT NULL,
    kind                      text NOT NULL CHECK (kind IN (
                                  'SPOT_PAIR','EVENT_OUTCOME','STABLE_VALUE','EQUITY','ETF','FUTURE','PERPETUAL','OPTION','SYNTHETIC')),
    symbol                    text NOT NULL,
    venue_code                text NOT NULL,
    exchange_mic              text CHECK (exchange_mic IS NULL OR exchange_mic ~ '^[A-Z0-9]{4}$'),
    underlying_instrument_id  uuid REFERENCES instruments(id),
    contract_multiplier       numeric(38,0) NOT NULL CHECK (contract_multiplier > 0),
    price_scale               integer NOT NULL CHECK (price_scale BETWEEN 0 AND 18),
    tick_size                 numeric(38,0) NOT NULL CHECK (tick_size > 0),
    lot_size                  numeric(38,0) NOT NULL CHECK (lot_size > 0),
    min_quantity              numeric(38,0) NOT NULL CHECK (min_quantity >= 0),
    quantity_decimals         smallint NOT NULL CHECK (quantity_decimals BETWEEN 0 AND 18),
    expiry                    timestamptz,
    strike_mantissa           numeric(38,0) CHECK (strike_mantissa IS NULL OR strike_mantissa > 0),
    option_right              text CHECK (option_right IS NULL OR option_right IN ('CALL','PUT')),
    exercise_style            text CHECK (exercise_style IS NULL OR exercise_style IN ('EUROPEAN','AMERICAN')),
    settlement_style          text NOT NULL CHECK (settlement_style IN ('CASH','PHYSICAL')),
    funding_interval_ms       bigint CHECK (funding_interval_ms IS NULL OR funding_interval_ms > 0),
    trading_calendar          text NOT NULL,
    jurisdiction_tags         text[] NOT NULL DEFAULT '{}',
    oracle_max_age_ms         bigint NOT NULL CHECK (oracle_max_age_ms > 0),
    oracle_sources            text[] NOT NULL DEFAULT '{}',
    margin_model              text NOT NULL CHECK (margin_model IN ('NONE','SPOT_MARGIN','LINEAR','OPTION','BINARY_OUTCOME')),
    corporate_action_behavior text NOT NULL CHECK (corporate_action_behavior IN ('NONE','ADJUST')),
    transferability           text NOT NULL CHECK (transferability IN ('TRANSFERABLE','NON_TRANSFERABLE')),
    outcomes                  text[],
    updated_at                timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (instrument_id, version) REFERENCES instrument_versions (instrument_id, version),
    -- The shape rules a reader would otherwise have to trust Go for.
    CHECK (kind <> 'OPTION' OR (strike_mantissa IS NOT NULL AND option_right IS NOT NULL
                                AND exercise_style IS NOT NULL AND expiry IS NOT NULL)),
    CHECK (kind = 'OPTION' OR (strike_mantissa IS NULL AND option_right IS NULL AND exercise_style IS NULL)),
    CHECK (kind <> 'FUTURE' OR expiry IS NOT NULL),
    CHECK (kind <> 'PERPETUAL' OR (expiry IS NULL AND funding_interval_ms IS NOT NULL)),
    CHECK (kind = 'PERPETUAL' OR funding_interval_ms IS NULL),
    CHECK (kind <> 'EVENT_OUTCOME' OR (cardinality(outcomes) >= 2 AND expiry IS NOT NULL)),
    CHECK (kind = 'EVENT_OUTCOME' OR outcomes IS NULL OR cardinality(outcomes) = 0)
);
CREATE INDEX instrument_terms_kind_idx ON instrument_terms (kind, symbol);
CREATE INDEX instrument_terms_underlying_idx ON instrument_terms (underlying_instrument_id) WHERE underlying_instrument_id IS NOT NULL;
CREATE INDEX instrument_terms_expiry_idx ON instrument_terms (expiry) WHERE expiry IS NOT NULL;
CREATE TRIGGER instrument_terms_updated_at BEFORE UPDATE ON instrument_terms FOR EACH ROW EXECUTE FUNCTION set_updated_at();

GRANT SELECT, INSERT ON instrument_versions TO cp_app;
GRANT SELECT, INSERT, UPDATE ON instrument_terms TO cp_app;
GRANT SELECT ON instrument_versions, instrument_terms TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: instrument terms are referenced by positions and journal history
