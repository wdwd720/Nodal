-- +goose Up
-- Who may read personal data, decided by what the columns now hold (F-47).
--
-- Migration 00010 created identity_pii with three *_encrypted columns and a
-- key_version, and granted cp_readonly and cp_ops SELECT on a named list of
-- tables that deliberately left identity_pii and sessions out. The role
-- bootstrap's blanket `ALTER DEFAULT PRIVILEGES ... GRANT SELECT ON TABLES`
-- granted both anyway, and silently won. Two deliberate statements in the
-- repository disagreed, and F-47 recorded the disagreement rather than
-- picking a side, because whether SELECT on identity_pii is an exposure
-- depends on whether the columns hold ciphertext -- and for a year nothing
-- wrote them at all.
--
-- internal/pii now writes them: AES-256-GCM under a keyring the deployment
-- supplies, each ciphertext bound to its row, its column and its key version.
-- The key never touches the database. So the question has an answer that
-- follows from the architecture instead of from preference:
--
--   * identity_pii. Neither role has a use for it. cp_readonly is analytics
--     and support reads, and PART 121 says personal data does not travel to
--     analytics; cp_ops runs retention and housekeeping, which never touch
--     this table. Ciphertext or not, a grant with no use is a grant waiting to
--     become an exposure the day a key leaks. 00010's list was right.
--
--   * sessions. cp_readonly has no use for token hashes, roles,
--     break_glass_until, IPs and user agents. cp_ops has exactly one:
--     `DELETE FROM sessions WHERE expires_at < now() - <retention>`
--     (auth/pgstore.PurgeExpired), which needs DELETE on the table and SELECT
--     on the one column it filters by. That is what it keeps -- and nothing
--     else, so the housekeeping role cannot read a session token hash.
--
-- The blanket default in the bootstrap stays: it is what makes every NEW table
-- readable by the two roles without a migration remembering to say so, and
-- test/integration/migrations names the withheld tables so the list cannot
-- grow silently.

REVOKE SELECT ON identity_pii FROM cp_readonly, cp_ops;
REVOKE SELECT ON sessions FROM cp_readonly, cp_ops;
GRANT SELECT (expires_at) ON sessions TO cp_ops;

-- +goose Down
REVOKE SELECT (expires_at) ON sessions FROM cp_ops;
GRANT SELECT ON sessions TO cp_readonly, cp_ops;
GRANT SELECT ON identity_pii TO cp_readonly, cp_ops;
