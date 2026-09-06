-- +goose Up
-- Idempotency contract (PART 36): one row per (actor, endpoint, key).
-- Same key + same request hash replays the stored result; same key + a
-- different hash is a deterministic conflict. Rows expire; cleanup is an
-- operations job (cp_app cannot DELETE).

CREATE TABLE idempotency_keys (
    actor_id        text        NOT NULL,
    endpoint        text        NOT NULL,
    key             text        NOT NULL,
    request_hash    text        NOT NULL,
    status          text        NOT NULL,
    response_status int,
    resource_type   text,
    resource_id     text,
    response_body   jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    completed_at    timestamptz,
    expires_at      timestamptz NOT NULL,
    CONSTRAINT idempotency_keys_pkey PRIMARY KEY (actor_id, endpoint, key),
    CONSTRAINT idempotency_keys_status_check CHECK (status IN ('IN_PROGRESS', 'COMPLETED', 'FAILED')),
    CONSTRAINT idempotency_keys_key_length_check CHECK (char_length(key) BETWEEN 1 AND 255)
);

-- Expiry sweep by the operations cleanup job.
CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- +goose StatementBegin
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_app') THEN
        GRANT SELECT, INSERT, UPDATE ON idempotency_keys TO cp_app;
        REVOKE DELETE, TRUNCATE, REFERENCES, TRIGGER ON idempotency_keys FROM cp_app;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_readonly') THEN
        GRANT SELECT ON idempotency_keys TO cp_readonly;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_ops') THEN
        GRANT SELECT, DELETE ON idempotency_keys TO cp_ops;
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose Down
DROP TABLE IF EXISTS idempotency_keys;
