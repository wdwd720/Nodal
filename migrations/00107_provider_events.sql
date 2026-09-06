-- +goose Up
-- Raw provider event registry (webhooks and polled provider events). Dedup for bus consumers is inbox_messages;
-- this table is the evidence record (PARTS 30, 75, 183, 188).

CREATE TABLE provider_events (
    id                     uuid PRIMARY KEY,
    provider               text NOT NULL,
    event_type             text NOT NULL,
    provider_event_id      text NOT NULL,
    received_at            timestamptz NOT NULL DEFAULT now(),
    provider_published_at  timestamptz,
    payload_hash           bytea NOT NULL,
    raw_ref                text,                           -- object archive URI of the raw request
    signature_verified     boolean NOT NULL,
    verification_error     text,
    processing_status      text NOT NULL CHECK (processing_status IN ('RECEIVED','PROCESSED','IGNORED','FAILED')),
    canonical_event_id     uuid,
    processed_at           timestamptz,
    error                  text,
    request_id             text,
    UNIQUE (provider, provider_event_id)
);
CREATE INDEX provider_events_received_idx ON provider_events (provider, received_at);
CREATE INDEX provider_events_pending_idx ON provider_events (provider) WHERE processing_status = 'RECEIVED';

GRANT SELECT, INSERT, UPDATE ON provider_events TO cp_app;
GRANT SELECT ON provider_events TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: provider evidence is never dropped
