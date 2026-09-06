-- +goose Up
-- Append-only, per-stream hash-chained audit events (PARTS 87, 89). Merkle checkpoints and KMS signatures
-- are added by the audit/proof migration range; this table is the foundation every money path writes to.

CREATE TABLE audit_events (
    id              uuid PRIMARY KEY,
    stream          text NOT NULL,                       -- 'account:<id>' | 'admin' | 'system' | 'agent:<id>'
    stream_seq      bigint NOT NULL CHECK (stream_seq >= 1),
    actor_type      text NOT NULL,
    actor_id        text NOT NULL,
    action          text NOT NULL,
    resource_type   text NOT NULL,
    resource_id     text NOT NULL,
    before_hash     bytea,
    after_hash      bytea,
    request_id      text,
    correlation_id  text,
    policy_version  text,
    reason          text,
    source_ip       inet,
    device          text,
    evidence_ref    text,
    build_version   text,
    payload         jsonb NOT NULL DEFAULT '{}'::jsonb,
    occurred_at     timestamptz NOT NULL,
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    content_hash    bytea NOT NULL,                      -- sha256(canonical record)
    prev_hash       bytea,                               -- content_hash of (stream, stream_seq - 1); NULL only for stream_seq = 1
    UNIQUE (stream, stream_seq)
);
CREATE INDEX audit_events_resource_idx ON audit_events (resource_type, resource_id, occurred_at);
CREATE INDEX audit_events_recorded_idx ON audit_events (recorded_at, id);
CREATE INDEX audit_events_correlation_idx ON audit_events (correlation_id) WHERE correlation_id IS NOT NULL;
CREATE TRIGGER audit_events_immutable BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION forbid_mutation();

GRANT SELECT, INSERT ON audit_events TO cp_app;
GRANT SELECT ON audit_events TO cp_readonly, cp_ops;

-- +goose Down
SELECT 1; -- protected: audit history is never dropped
