-- +goose Up
-- Transactional outbox (events written in the same transaction as financial
-- state) and inbox (exactly-once processing of inbound provider messages).

CREATE TABLE outbox_events (
    id               uuid        PRIMARY KEY,
    topic            text        NOT NULL,
    partition_key    text        NOT NULL,
    event_type       text        NOT NULL,
    schema_version   int         NOT NULL,
    source           text        NOT NULL,
    aggregate_type   text,
    aggregate_id     text,
    correlation_id   text,
    causation_id     text,
    occurred_at      timestamptz NOT NULL,
    recorded_at      timestamptz NOT NULL DEFAULT now(),
    dedup_key        text,
    headers          jsonb       NOT NULL DEFAULT '{}'::jsonb,
    payload          jsonb       NOT NULL,
    published_at     timestamptz,
    publish_attempts int         NOT NULL DEFAULT 0,
    last_error       text,
    CONSTRAINT outbox_events_schema_version_check CHECK (schema_version > 0),
    CONSTRAINT outbox_events_publish_attempts_check CHECK (publish_attempts >= 0)
);

-- Relay scan: unpublished rows, oldest first.
CREATE INDEX outbox_events_unpublished_idx
    ON outbox_events (recorded_at, id)
    WHERE published_at IS NULL;

-- Producer-side de-duplication within a topic.
CREATE UNIQUE INDEX outbox_events_topic_dedup_key_uidx
    ON outbox_events (topic, dedup_key)
    WHERE dedup_key IS NOT NULL;

CREATE TABLE inbox_messages (
    source         text        NOT NULL,
    message_id     text        NOT NULL,
    schema_version int         NOT NULL,
    received_at    timestamptz NOT NULL DEFAULT now(),
    processed_at   timestamptz,
    status         text        NOT NULL,
    error          text,
    payload_hash   text,
    CONSTRAINT inbox_messages_pkey PRIMARY KEY (source, message_id),
    CONSTRAINT inbox_messages_status_check CHECK (status IN ('RECEIVED', 'PROCESSED', 'FAILED'))
);

-- Role separation (PART 101). Default privileges already grant DML to cp_app;
-- DELETE is revoked explicitly: outbox/inbox retention is an operations job.
-- Guarded so the migration is portable to environments where roles are
-- provisioned under other names (missing role == no privileges: fail closed).
-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_app') THEN
        GRANT SELECT, INSERT, UPDATE ON outbox_events, inbox_messages TO cp_app;
        REVOKE DELETE, TRUNCATE, REFERENCES, TRIGGER ON outbox_events, inbox_messages FROM cp_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_readonly') THEN
        GRANT SELECT ON outbox_events, inbox_messages TO cp_readonly;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_ops') THEN
        GRANT SELECT, DELETE ON outbox_events, inbox_messages TO cp_ops;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS outbox_events;
