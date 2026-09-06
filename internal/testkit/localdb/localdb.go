// Package localdb provisions isolated, fully migrated PostgreSQL databases on
// the LOCAL docker-compose stack for integration tests and drills.
//
// It refuses any admin DSN that does not point at 127.0.0.1/localhost, so it
// can never touch a shared environment. Roles and default privileges mirror
// docker/postgres/init/001_roles.sql: the application role receives no
// default table privileges (DECISION_REGISTER D-016).
//
// This package is test/dev tooling and must never be imported by production
// binaries (enforced by lintfin's test-only import rule).
package localdb

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/db/migrate"
)

// LOCAL docker-compose credentials only (see docker/postgres/init/001_roles.sql).
const (
	DefaultAdminDSN = "postgres://cp_admin:cp_admin_local@127.0.0.1:5433/postgres?sslmode=disable" //nolint:gosec // local-only compose credential
	appPassword     = "cp_app_local"                                                               //nolint:gosec // local-only compose credential
	migratePassword = "cp_migrate_local"                                                           //nolint:gosec // local-only compose credential
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// Database describes a provisioned database.
type Database struct {
	Name       string
	AdminDSN   string // admin role connected to this database
	AppDSN     string
	MigrateDSN string
}

// Options control provisioning.
type Options struct {
	AdminDSN string // admin connection to the maintenance database; DefaultAdminDSN when empty
	Migrate  bool   // apply all embedded migrations after creation
}

func parseAdmin(dsn string) (*url.URL, error) {
	if dsn == "" {
		dsn = DefaultAdminDSN
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("localdb: admin dsn: %w", err)
	}
	host := u.Hostname()
	if host != "127.0.0.1" && host != "localhost" {
		return nil, fmt.Errorf("localdb: refusing to provision databases on non-local host %q", host)
	}
	return u, nil
}

// Provision drops and recreates controlplane_test_<suffix>, applies the role
// grants, and (optionally) migrates it.
func Provision(ctx context.Context, suffix string, opts Options) (Database, error) {
	if !nameRE.MatchString(suffix) {
		return Database{}, fmt.Errorf("localdb: suffix must match %s", nameRE)
	}
	u, err := parseAdmin(opts.AdminDSN)
	if err != nil {
		return Database{}, err
	}
	dbName := "controlplane_test_" + suffix
	if err := dropDatabase(ctx, u.String(), dbName); err != nil {
		return Database{}, err
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return Database{}, fmt.Errorf("localdb: connect admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, stmt := range []string{
		`CREATE DATABASE ` + quoteIdent(dbName) + ` OWNER cp_migrate`,
		`GRANT ALL PRIVILEGES ON DATABASE ` + quoteIdent(dbName) + ` TO cp_migrate`,
		`GRANT CONNECT ON DATABASE ` + quoteIdent(dbName) + ` TO cp_app, cp_readonly, cp_ops`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return Database{}, fmt.Errorf("localdb: %s: %w", stmt, err)
		}
	}
	d := describe(u, dbName)
	if err := PrepareSchema(ctx, d.AdminDSN); err != nil {
		return Database{}, err
	}
	if opts.Migrate {
		if err := migrate.Up(ctx, d.MigrateDSN); err != nil {
			return Database{}, fmt.Errorf("localdb: migrate: %w", err)
		}
	}
	return d, nil
}

// CreateEmpty creates an empty database with the role grants but no schema
// preparation and no migrations (used as a restore target).
func CreateEmpty(ctx context.Context, suffix, adminDSN string) (Database, error) {
	if !nameRE.MatchString(suffix) {
		return Database{}, fmt.Errorf("localdb: suffix must match %s", nameRE)
	}
	u, err := parseAdmin(adminDSN)
	if err != nil {
		return Database{}, err
	}
	dbName := "controlplane_test_" + suffix
	if err := dropDatabase(ctx, u.String(), dbName); err != nil {
		return Database{}, err
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return Database{}, fmt.Errorf("localdb: connect admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, stmt := range []string{
		`CREATE DATABASE ` + quoteIdent(dbName) + ` OWNER cp_migrate`,
		`GRANT ALL PRIVILEGES ON DATABASE ` + quoteIdent(dbName) + ` TO cp_migrate`,
		`GRANT CONNECT ON DATABASE ` + quoteIdent(dbName) + ` TO cp_app, cp_readonly, cp_ops`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return Database{}, fmt.Errorf("localdb: %s: %w", stmt, err)
		}
	}
	return describe(u, dbName), nil
}

// PrepareSchema sets schema ownership and default privileges exactly as the
// compose init script does.
func PrepareSchema(ctx context.Context, adminDBDSN string) error {
	conn, err := pgx.Connect(ctx, adminDBDSN)
	if err != nil {
		return fmt.Errorf("localdb: connect new db: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	for _, stmt := range []string{
		`ALTER SCHEMA public OWNER TO cp_migrate`,
		`GRANT USAGE ON SCHEMA public TO cp_app, cp_readonly, cp_ops`,
		// No default table privileges for cp_app: each migration grants explicitly (D-016).
		`ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO cp_app`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE cp_migrate IN SCHEMA public GRANT SELECT ON TABLES TO cp_readonly, cp_ops`,
		`CREATE EXTENSION IF NOT EXISTS pg_stat_statements`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("localdb: prepare schema: %w", err)
		}
	}
	return nil
}

// Drop terminates sessions and drops controlplane_test_<suffix>.
func Drop(ctx context.Context, suffix, adminDSN string) error {
	if !nameRE.MatchString(suffix) {
		return fmt.Errorf("localdb: suffix must match %s", nameRE)
	}
	u, err := parseAdmin(adminDSN)
	if err != nil {
		return err
	}
	return dropDatabase(ctx, u.String(), "controlplane_test_"+suffix)
}

func dropDatabase(ctx context.Context, adminDSN, dbName string) error {
	conn, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		return fmt.Errorf("localdb: connect admin: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, dbName); err != nil {
		return fmt.Errorf("localdb: terminate sessions: %w", err)
	}
	if _, err := conn.Exec(ctx, `DROP DATABASE IF EXISTS `+quoteIdent(dbName)); err != nil {
		return fmt.Errorf("localdb: drop: %w", err)
	}
	return nil
}

func describe(u *url.URL, dbName string) Database {
	host, port := u.Hostname(), u.Port()
	if port == "" {
		port = "5432"
	}
	admin := *u
	admin.Path = "/" + dbName
	return Database{
		Name:       dbName,
		AdminDSN:   admin.String(),
		AppDSN:     fmt.Sprintf("postgres://cp_app:%s@%s:%s/%s?sslmode=disable", appPassword, host, port, dbName),
		MigrateDSN: fmt.Sprintf("postgres://cp_migrate:%s@%s:%s/%s?sslmode=disable", migratePassword, host, port, dbName),
	}
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// ErrNotLocal is returned for non-local admin DSNs.
var ErrNotLocal = errors.New("localdb: not a local database")
