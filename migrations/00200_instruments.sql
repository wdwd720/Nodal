-- +goose Up
-- Universal instrument model (PARTS 32, 33, 231): Asset ← EconomicExposure ← Instrument ← VenueListing → Venue.

CREATE TABLE venues (
    id                uuid PRIMARY KEY,
    code              text NOT NULL UNIQUE,                 -- 'JUPITER'
    name              text NOT NULL,
    kind              text NOT NULL CHECK (kind IN ('DEX_AGGREGATOR','DEX','CEX','OTC')),
    chain             text,
    status            text NOT NULL CHECK (status IN ('ACTIVE','DEGRADED','DISABLED')),
    settlement_rules  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER venues_updated_at BEFORE UPDATE ON venues FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE economic_exposures (
    id                   uuid PRIMARY KEY,
    kind                 text NOT NULL CHECK (kind IN ('ASSET_PRICE','EVENT_OUTCOME')),
    description          text NOT NULL,
    underlying_asset_id  uuid REFERENCES assets(id),
    event_terms          jsonb,                              -- reserved for EVENT_OUTCOME; trading disabled in V1
    created_at           timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE instruments (
    id                   uuid PRIMARY KEY,
    type                 text NOT NULL CHECK (type IN ('SPOT_PAIR','EVENT_OUTCOME')),
    canonical_name       text NOT NULL,
    exposure_id          uuid NOT NULL REFERENCES economic_exposures(id),
    base_asset_id        uuid REFERENCES assets(id),
    quote_asset_id       uuid REFERENCES assets(id),
    settlement_asset_id  uuid NOT NULL REFERENCES assets(id),
    risk_class           text NOT NULL CHECK (risk_class IN ('SETTLEMENT','MAJOR','STANDARD','SPECULATIVE','UNSUPPORTED')),
    status               text NOT NULL CHECK (status IN ('ACTIVE','CLOSE_ONLY','RESTRICTED','HALTED','DELISTING','DELISTED')),
    active_from          timestamptz NOT NULL,
    active_until         timestamptz,
    policy_ref           text,
    metadata_version     integer NOT NULL DEFAULT 1,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CHECK (type <> 'SPOT_PAIR' OR (base_asset_id IS NOT NULL AND quote_asset_id IS NOT NULL))
);
CREATE UNIQUE INDEX instruments_spot_pair_idx ON instruments (type, base_asset_id, quote_asset_id) WHERE type = 'SPOT_PAIR';
CREATE TRIGGER instruments_updated_at BEFORE UPDATE ON instruments FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE instrument_status_transitions (
    id              uuid PRIMARY KEY,
    instrument_id   uuid NOT NULL REFERENCES instruments(id),
    from_status     text NOT NULL,
    to_status       text NOT NULL,
    actor_type      text NOT NULL CHECK (actor_type <> 'AGENT'),
    actor_id        text NOT NULL,
    reason          text NOT NULL,
    policy_version  text,
    occurred_at     timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER instrument_status_transitions_immutable BEFORE UPDATE OR DELETE ON instrument_status_transitions
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

CREATE TABLE venue_listings (
    id                   uuid PRIMARY KEY,
    venue_id             uuid NOT NULL REFERENCES venues(id),
    instrument_id        uuid NOT NULL REFERENCES instruments(id),
    venue_native_id      text NOT NULL,
    network              text NOT NULL,
    base_mint            text,
    quote_mint           text,
    tick_size            numeric(38,0),
    base_precision       smallint NOT NULL CHECK (base_precision BETWEEN 0 AND 18),
    quote_precision      smallint NOT NULL CHECK (quote_precision BETWEEN 0 AND 18),
    min_notional_quote   numeric(38,0) NOT NULL CHECK (min_notional_quote >= 0),
    max_notional_quote   numeric(38,0),
    status               text NOT NULL CHECK (status IN ('ACTIVE','DEGRADED','DISABLED')),
    settlement_rules     jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (venue_id, instrument_id)
);
CREATE TRIGGER venue_listings_updated_at BEFORE UPDATE ON venue_listings FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE external_identifiers (
    id           uuid PRIMARY KEY,
    entity_type  text NOT NULL CHECK (entity_type IN ('ASSET','INSTRUMENT','VENUE_LISTING','VENUE')),
    entity_id    uuid NOT NULL,
    provider     text NOT NULL,
    external_id  text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    UNIQUE (provider, entity_type, external_id)
);
CREATE INDEX external_identifiers_entity_idx ON external_identifiers (entity_type, entity_id);

GRANT SELECT, INSERT, UPDATE ON venues, economic_exposures, instruments, venue_listings, external_identifiers TO cp_app;
GRANT SELECT, INSERT ON instrument_status_transitions TO cp_app;
GRANT SELECT ON venues, economic_exposures, instruments, instrument_status_transitions, venue_listings, external_identifiers TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: instrument definitions are referenced by financial history
