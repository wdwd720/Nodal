-- PRODUCTION ROLE BOOTSTRAP (PART 101, DECISION_REGISTER D-016).
--
-- Mirrors docker/postgres/init/001_roles.sql for AWS RDS, minus the LOCAL-only
-- databases (temporal, temporal_visibility, controlplane_test). Executed once
-- per database by the db-bootstrap one-shot ECS task (DEPLOYMENT.md section 4)
-- as the RDS master user. Passwords arrive as psql variables injected from
-- Secrets Manager; nothing secret is written in this file:
--
--   psql "$PGCONN" -v ON_ERROR_STOP=1 \
--     -v cp_migrate_password="$CP_MIGRATE_PASSWORD" \
--     -v cp_app_password="$CP_APP_PASSWORD" \
--     -v cp_readonly_password="$CP_READONLY_PASSWORD" \
--     -v cp_ops_password="$CP_OPS_PASSWORD" \
--     -v cp_app_connection_limit=200 \
--     -f roles.sql
--
-- Idempotent: re-running resets the passwords and re-applies the grants.

\set ON_ERROR_STOP on

-- Roles (created only when absent; password always (re)set from the secret).
SELECT format('CREATE ROLE cp_migrate LOGIN PASSWORD %L', :'cp_migrate_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_migrate') \gexec
SELECT format('CREATE ROLE cp_app LOGIN PASSWORD %L', :'cp_app_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_app') \gexec
SELECT format('CREATE ROLE cp_readonly LOGIN PASSWORD %L', :'cp_readonly_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_readonly') \gexec
SELECT format('CREATE ROLE cp_ops LOGIN PASSWORD %L', :'cp_ops_password')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'cp_ops') \gexec

ALTER ROLE cp_migrate  PASSWORD :'cp_migrate_password';
ALTER ROLE cp_app      PASSWORD :'cp_app_password';
ALTER ROLE cp_readonly PASSWORD :'cp_readonly_password';
ALTER ROLE cp_ops      PASSWORD :'cp_ops_password';

-- No unlimited connection spawning (PART 139): the pool limits are enforced
-- in config, the role limit is the backstop.
ALTER ROLE cp_app      CONNECTION LIMIT :cp_app_connection_limit;
ALTER ROLE cp_readonly CONNECTION LIMIT 20;
ALTER ROLE cp_ops      CONNECTION LIMIT 10;
ALTER ROLE cp_migrate  CONNECTION LIMIT 5;

-- The master user must be a member of cp_migrate to hand over schema
-- ownership and to set default privileges on its behalf (PostgreSQL rule:
-- "you must be a member of the new owning role"). On RDS the master user is
-- not a superuser, so this grant is required, not cosmetic.
GRANT cp_migrate TO CURRENT_USER;

GRANT ALL PRIVILEGES ON DATABASE controlplane TO cp_migrate;
GRANT CONNECT ON DATABASE controlplane TO cp_app, cp_readonly, cp_ops;

-- Schema: owned by the migration role; nobody else may create objects.
ALTER SCHEMA public OWNER TO cp_migrate;
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE ON SCHEMA public TO cp_app, cp_readonly, cp_ops;

-- Default privileges for objects created by the migration role.
-- The application role gets NO default table privileges (D-016): every
-- migration grants exactly what the application needs per table. A table
-- without an explicit grant is unreadable by the app, which fails closed and
-- keeps privilege review local to the migration that creates the table.
-- Only sequence usage is defaulted so inserts into granted tables work.
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO cp_app;
-- The read roles DO get a default SELECT, and that is a deliberate contract
-- rather than an oversight: test/integration/migrations/privileges_test.go
-- asserts "cp_readonly and cp_ops can SELECT everything and write nothing" for
-- every table in the schema, and cp_ops performs retention cleanup on sessions,
-- which needs SELECT on the columns it filters on.
--
-- It does sit awkwardly beside migration 00010, whose grant list deliberately
-- withholds identity_pii and sessions from these two roles. That list has no
-- effect while this line exists. The contradiction is recorded as F-47, OPEN:
-- resolving it is a decision about who may read encrypted PII, and it belongs
-- to whoever owns that policy.
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT SELECT ON TABLES TO cp_readonly, cp_ops;

-- Query statistics for the operations role (parameter group preloads it).
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;
GRANT pg_read_all_stats TO cp_ops;
