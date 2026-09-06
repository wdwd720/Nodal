-- +goose Up
-- Point-in-time reality engine, Postgres side (PARTS 74, 75, 77, 79, 120, 122, 124, 199, 200):
-- data sources with licensing/retention classes, raw archive index, ingest checkpoints, stream gaps, provider health.
-- Normalized high-volume events live in ClickHouse (docs/architecture/POINT_IN_TIME.md, Appendix A).

CREATE TABLE data_sources (
    id                        uuid PRIMARY KEY,
    code                      text NOT NULL UNIQUE,
    provider                  text NOT NULL,
    kind                      text NOT NULL CHECK (kind IN ('MARKET_DATA','ONCHAIN','SOCIAL','WALLET_INTELLIGENCE','MODEL','INTERNAL')),
    retention_class           text NOT NULL CHECK (retention_class IN ('FINANCIAL_RECORD','SECURITY_AUDIT','RAW_MARKET_DATA','SOCIAL_DATA','MODEL_IO','OPERATIONAL_LOG')),
    retention_days            integer NOT NULL CHECK (retention_days > 0),   -- configured per source; PROD floors enforced by config.Validate
    redistribution_policy     text NOT NULL CHECK (redistribution_policy IN ('NONE','INTERNAL_ONLY','CUSTOMER_DISPLAY','REDISTRIBUTABLE')),
    historical_use_permitted  text NOT NULL CHECK (historical_use_permitted IN ('YES','NO','UNKNOWN')),
    persistence_capability    text NOT NULL CHECK (persistence_capability IN ('ALLOWED','BLOCKED')),
    environments              text[] NOT NULL DEFAULT '{}',
    license_ref               text,
    contract_ref              text,
    dedup_strategy            text NOT NULL CHECK (dedup_strategy IN ('PROVIDER_ID','COMPOSITE_HASH')),
    heartbeat_timeout_ms      integer NOT NULL CHECK (heartbeat_timeout_ms > 0),
    supports_replay           boolean NOT NULL DEFAULT false,
    supports_sequence         boolean NOT NULL DEFAULT false,
    status                    text NOT NULL CHECK (status IN ('ACTIVE','DEGRADED','DISABLED')),
    created_by_actor_type     text NOT NULL CHECK (created_by_actor_type <> 'AGENT'),
    created_by_actor_id       text NOT NULL,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    -- PART 120: unknown or absent legal rights => no permanent historical dependence.
    CHECK (historical_use_permitted = 'YES' OR persistence_capability = 'BLOCKED')
);
CREATE TRIGGER data_sources_updated_at BEFORE UPDATE ON data_sources FOR EACH ROW EXECUTE FUNCTION set_updated_at();
ALTER TABLE tools ADD CONSTRAINT tools_data_source_fk FOREIGN KEY (data_source_id) REFERENCES data_sources(id);

-- Index of every raw provider payload archived to object storage (PART 75, 124). Immutable.
CREATE TABLE raw_archive_objects (
    id                     uuid PRIMARY KEY,
    data_source_id         uuid NOT NULL REFERENCES data_sources(id),
    provider               text NOT NULL,
    event_type             text NOT NULL,
    source_event_id        text,
    dedup_key              text NOT NULL,
    schema_version         integer NOT NULL CHECK (schema_version >= 1),
    object_uri             text NOT NULL UNIQUE,
    object_hash            bytea NOT NULL CHECK (length(object_hash) = 32),
    size_bytes             bigint NOT NULL CHECK (size_bytes >= 0),
    content_type           text NOT NULL,
    partition_key          text NOT NULL,                                -- provider/event_type/vN/YYYY/MM/DD/HH
    stream                 text,
    source_partition       text,
    source_offset          text,
    sequence               bigint,
    source_event_at        timestamptz,                                  -- provider clock, untrusted
    provider_published_at  timestamptz,                                  -- provider clock, untrusted
    platform_received_at   timestamptz NOT NULL,                         -- our clock; knowledge-time anchor
    ingested_at            timestamptz NOT NULL DEFAULT now(),
    retention_class        text NOT NULL CHECK (retention_class IN ('FINANCIAL_RECORD','SECURITY_AUDIT','RAW_MARKET_DATA','SOCIAL_DATA','MODEL_IO','OPERATIONAL_LOG')),
    retention_until        timestamptz NOT NULL,
    correlation_id         text,
    UNIQUE (provider, event_type, dedup_key)
);
CREATE INDEX raw_archive_objects_received_idx ON raw_archive_objects (data_source_id, platform_received_at);
CREATE INDEX raw_archive_objects_source_event_idx ON raw_archive_objects (provider, source_event_id) WHERE source_event_id IS NOT NULL;
CREATE INDEX raw_archive_objects_retention_idx ON raw_archive_objects (retention_until);
CREATE TRIGGER raw_archive_objects_immutable BEFORE UPDATE OR DELETE ON raw_archive_objects
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

-- Ingest position per (source, stream, partition, consumer) (PART 77, 200).
CREATE TABLE ingest_checkpoints (
    id                         uuid PRIMARY KEY,
    data_source_id             uuid NOT NULL REFERENCES data_sources(id),
    stream                     text NOT NULL,
    source_partition           text NOT NULL DEFAULT '0',
    consumer                   text NOT NULL,
    last_sequence              bigint,
    last_source_offset         text,
    last_source_event_at       timestamptz,
    last_platform_received_at  timestamptz,
    last_raw_object_id         uuid REFERENCES raw_archive_objects(id),
    events_since_start         bigint NOT NULL DEFAULT 0 CHECK (events_since_start >= 0),
    status                     text NOT NULL CHECK (status IN ('ACTIVE','DISCONNECTED','REPLAYING','GAP','STOPPED')),
    connected_at               timestamptz,
    disconnected_at            timestamptz,
    version                    bigint NOT NULL DEFAULT 0,
    updated_at                 timestamptz NOT NULL DEFAULT now(),
    UNIQUE (data_source_id, stream, source_partition, consumer)
);
CREATE TRIGGER ingest_checkpoints_updated_at BEFORE UPDATE ON ingest_checkpoints FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Detected gaps, reconnects, replays and ordering anomalies (PART 77, 200). Strategies over an open or
-- unrecoverable gap cannot pretend continuity.
CREATE TABLE stream_gaps (
    id                      uuid PRIMARY KEY,
    data_source_id          uuid NOT NULL REFERENCES data_sources(id),
    checkpoint_id           uuid REFERENCES ingest_checkpoints(id),
    stream                  text NOT NULL,
    source_partition        text NOT NULL DEFAULT '0',
    kind                    text NOT NULL CHECK (kind IN ('GAP','SILENCE','RECONNECT','REPLAY','ORDERING_ANOMALY')),
    expected_sequence       bigint,
    observed_sequence       bigint,
    gap_start_sequence      bigint,
    gap_end_sequence        bigint,
    gap_start_at            timestamptz NOT NULL,
    gap_end_at              timestamptz,
    detected_at             timestamptz NOT NULL DEFAULT now(),
    resolution              text NOT NULL DEFAULT 'OPEN' CHECK (resolution IN ('OPEN','REPLAYED','UNRECOVERABLE','ACKNOWLEDGED')),
    resolved_at             timestamptz,
    resolved_by_actor_type  text CHECK (resolved_by_actor_type IS NULL OR resolved_by_actor_type IN ('OPERATOR','SYSTEM')),
    resolved_by_actor_id    text,
    detail                  jsonb NOT NULL DEFAULT '{}'::jsonb,
    correlation_id          text,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    CHECK (resolution = 'OPEN' OR (resolved_at IS NOT NULL AND resolved_by_actor_id IS NOT NULL)),
    CHECK (gap_end_at IS NULL OR gap_end_at >= gap_start_at)
);
CREATE INDEX stream_gaps_open_idx ON stream_gaps (data_source_id, stream) WHERE resolution = 'OPEN';
CREATE INDEX stream_gaps_window_idx ON stream_gaps (data_source_id, gap_start_at, gap_end_at);
CREATE TRIGGER stream_gaps_updated_at BEFORE UPDATE ON stream_gaps FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- Provider health samples (PART 79); the current state is the latest sample per (provider, role).
CREATE TABLE provider_health_samples (
    id                  uuid PRIMARY KEY,
    provider            text NOT NULL,
    role                text NOT NULL CHECK (role IN ('DATA','OBSERVATION','EXECUTION','FUNDING','WALLET','MODEL')),
    data_source_id      uuid REFERENCES data_sources(id),
    state               text NOT NULL CHECK (state IN ('HEALTHY','DEGRADED','UNHEALTHY','DISABLED')),
    error_rate_bps      integer NOT NULL CHECK (error_rate_bps BETWEEN 0 AND 10000),
    p50_latency_ms      integer NOT NULL CHECK (p50_latency_ms >= 0),
    p99_latency_ms      integer NOT NULL CHECK (p99_latency_ms >= 0),
    staleness_ms        bigint NOT NULL CHECK (staleness_ms >= 0),
    window_ms           integer NOT NULL CHECK (window_ms > 0),
    sample_count        integer NOT NULL CHECK (sample_count >= 0),
    reason_codes        text[] NOT NULL DEFAULT '{}',
    evaluator_version   text NOT NULL,
    evaluated_at        timestamptz NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX provider_health_samples_latest_idx ON provider_health_samples (provider, role, evaluated_at DESC);
CREATE TRIGGER provider_health_samples_immutable BEFORE UPDATE OR DELETE ON provider_health_samples
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT, UPDATE ON data_sources, ingest_checkpoints, stream_gaps TO cp_app;
GRANT SELECT, INSERT ON raw_archive_objects, provider_health_samples TO cp_app;
GRANT SELECT ON data_sources, raw_archive_objects, ingest_checkpoints, stream_gaps, provider_health_samples TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: the raw archive index and gap history are evidence for every downstream decision
