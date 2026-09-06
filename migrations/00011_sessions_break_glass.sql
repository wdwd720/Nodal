-- +goose Up
-- Break-glass elevation is carried by a rotated session (PART 93); the Postgres
-- session store needs to persist its expiry alongside the session row.
ALTER TABLE sessions ADD COLUMN break_glass_until timestamptz;
ALTER TABLE sessions ADD CONSTRAINT sessions_break_glass_operator_only
    CHECK (break_glass_until IS NULL OR actor_type = 'OPERATOR');

-- Expired-session purge is an operations job (auth/pgstore.PurgeExpired); the application role never deletes.
GRANT DELETE ON sessions TO cp_ops;

-- +goose Down
REVOKE DELETE ON sessions FROM cp_ops;
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_break_glass_operator_only;
ALTER TABLE sessions DROP COLUMN IF EXISTS break_glass_until;
