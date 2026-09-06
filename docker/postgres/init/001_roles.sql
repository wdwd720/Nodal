-- LOCAL DEVELOPMENT ONLY. Production roles are created by Terraform/operators with secrets from Secrets Manager.
-- Role separation (PART 101): migration role, application role, read-only analytics role, operations role.

CREATE ROLE cp_migrate LOGIN PASSWORD 'cp_migrate_local';
CREATE ROLE cp_app LOGIN PASSWORD 'cp_app_local';
CREATE ROLE cp_readonly LOGIN PASSWORD 'cp_readonly_local';
CREATE ROLE cp_ops LOGIN PASSWORD 'cp_ops_local';

GRANT ALL PRIVILEGES ON DATABASE controlplane TO cp_migrate;
GRANT CONNECT ON DATABASE controlplane TO cp_app, cp_readonly, cp_ops;

-- Temporal databases (auto-setup creates schema; it needs the admin user configured in compose).
CREATE DATABASE temporal;
CREATE DATABASE temporal_visibility;

-- Test database for integration tests (migrations applied by the test harness).
CREATE DATABASE controlplane_test;
GRANT ALL PRIVILEGES ON DATABASE controlplane_test TO cp_migrate;
GRANT CONNECT ON DATABASE controlplane_test TO cp_app, cp_readonly, cp_ops;

\connect controlplane
ALTER SCHEMA public OWNER TO cp_migrate;
GRANT USAGE ON SCHEMA public TO cp_app, cp_readonly, cp_ops;
-- Default privileges for objects created by the migration role.
-- The application role gets NO default table privileges: every migration grants exactly what the
-- application needs per table (PART 101). A table without an explicit grant is unreadable by the app,
-- which fails closed and keeps privilege review local to the migration that creates the table.
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO cp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT SELECT ON TABLES TO cp_readonly, cp_ops;
CREATE EXTENSION IF NOT EXISTS pg_stat_statements;

\connect controlplane_test
ALTER SCHEMA public OWNER TO cp_migrate;
GRANT USAGE ON SCHEMA public TO cp_app, cp_readonly, cp_ops;
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO cp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT SELECT ON TABLES TO cp_readonly, cp_ops;
