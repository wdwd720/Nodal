-- +goose Up
-- Server-side OIDC login attempts (PART 90, 192): state, nonce and PKCE verifier are persisted
-- instead of being round-tripped in a cookie, so a callback can be consumed exactly once and a
-- replayed or forged state never yields a session.

CREATE TABLE login_attempts (
    state          text PRIMARY KEY,
    nonce          text NOT NULL,
    code_verifier  text NOT NULL,
    step_up        boolean NOT NULL DEFAULT false,
    return_to      text,
    ip             inet,
    user_agent     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    expires_at     timestamptz NOT NULL,
    consumed_at    timestamptz,
    outcome        text CHECK (outcome IS NULL OR outcome IN ('SUCCESS','FAILED'))
);
CREATE INDEX login_attempts_expires_idx ON login_attempts (expires_at);

GRANT SELECT, INSERT, UPDATE ON login_attempts TO cp_app;
GRANT SELECT ON login_attempts TO cp_readonly, cp_ops;
GRANT DELETE ON login_attempts TO cp_ops;

-- +goose Down
SELECT 1; -- protected range: rollback never drops tables; login_attempts is transient and purged by the ops role instead
