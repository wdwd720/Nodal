package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults applied when the corresponding Config field is zero.
const (
	DefaultStatementTimeout = 30 * time.Second
	DefaultLockTimeout      = 5 * time.Second
	DefaultAppName          = "controlplane"
)

// ErrTLSRequired is returned by Open when Config.RequireTLS is set and the
// connection URL does not request a verified TLS mode (verify-ca/verify-full).
var ErrTLSRequired = errors.New("db: RequireTLS is set but sslmode is not verify-ca or verify-full")

// Config describes a pool. StatementTimeout and LockTimeout are applied per
// connection via SET; zero means the package default, a negative value means
// "do not set" (server default).
type Config struct {
	URL              string
	MaxConns         int32
	MinConns         int32
	AppName          string
	RequireTLS       bool
	StatementTimeout time.Duration
	LockTimeout      time.Duration
}

// DB wraps a pgx pool. It satisfies Querier so read-only repositories can be
// given either a *DB or a pgx.Tx.
type DB struct {
	pool *pgxpool.Pool
}

// Querier is the minimal query interface satisfied by *pgxpool.Pool, *pgx.Conn,
// pgx.Tx and *DB. Repositories accept a Querier so the same code runs inside or
// outside a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (*pgx.Conn)(nil)
	_ Querier = pgx.Tx(nil)
	_ Querier = (*DB)(nil)
)

// Open builds a traced pgx pool and verifies connectivity with a Ping. It fails
// before dialing when RequireTLS is set and the URL's sslmode is not a
// verifying mode.
func Open(ctx context.Context, cfg Config) (*DB, error) {
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("db: Config.URL is empty")
	}
	if cfg.RequireTLS {
		mode, err := SSLMode(cfg.URL)
		if err != nil {
			return nil, fmt.Errorf("db: %w", err)
		}
		if !IsVerifiedSSLMode(mode) {
			return nil, fmt.Errorf("%w (sslmode=%q)", ErrTLSRequired, mode)
		}
	}

	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		// pgconn redacts the password in ParseConfigError; we still never echo the URL ourselves.
		return nil, fmt.Errorf("db: parse config: %w", err)
	}
	pc.ConnConfig.Tracer = otelpgx.NewTracer(otelpgx.WithTrimSQLInSpanName())

	appName := cfg.AppName
	if appName == "" && pc.ConnConfig.RuntimeParams["application_name"] == "" {
		appName = DefaultAppName
	}
	if appName != "" {
		pc.ConnConfig.RuntimeParams["application_name"] = appName
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}
	if pc.MinConns > pc.MaxConns {
		return nil, fmt.Errorf("db: MinConns (%d) exceeds MaxConns (%d)", pc.MinConns, pc.MaxConns)
	}

	sessionSQL := sessionSetup(cfg.StatementTimeout, cfg.LockTimeout)
	pc.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		if _, err := conn.Exec(ctx, sessionSQL); err != nil {
			return fmt.Errorf("db: session setup: %w", err)
		}
		return nil
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("db: new pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// sessionSetup renders the SET statements executed on every new connection.
// Only integers are interpolated, so the statement cannot be injected into.
func sessionSetup(statementTimeout, lockTimeout time.Duration) string {
	var b strings.Builder
	b.WriteString("SET TIME ZONE 'UTC'")
	if ms, ok := timeoutMillis(statementTimeout, DefaultStatementTimeout); ok {
		fmt.Fprintf(&b, "; SET statement_timeout = %d", ms)
	}
	if ms, ok := timeoutMillis(lockTimeout, DefaultLockTimeout); ok {
		fmt.Fprintf(&b, "; SET lock_timeout = %d", ms)
	}
	return b.String()
}

// timeoutMillis resolves a configured timeout: zero -> def, negative -> unset.
func timeoutMillis(d, def time.Duration) (int64, bool) {
	switch {
	case d < 0:
		return 0, false
	case d == 0:
		d = def
	}
	ms := d.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	return ms, true
}

// Pool exposes the underlying pool for packages that need pgx-specific
// features (CopyFrom, LISTEN/NOTIFY, batch). Prefer Querier/InTx elsewhere.
func (db *DB) Pool() *pgxpool.Pool { return db.pool }

// Close closes the pool, waiting for acquired connections to be released.
func (db *DB) Close() { db.pool.Close() }

// Ping verifies a connection can be acquired and the server answers.
func (db *DB) Ping(ctx context.Context) error { return db.pool.Ping(ctx) }

// Exec implements Querier against the pool.
func (db *DB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return db.pool.Exec(ctx, sql, args...)
}

// Query implements Querier against the pool.
func (db *DB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return db.pool.Query(ctx, sql, args...)
}

// QueryRow implements Querier against the pool.
func (db *DB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return db.pool.QueryRow(ctx, sql, args...)
}

// SSLMode extracts the sslmode from a URL (postgres://...?sslmode=x) or a
// keyword/value DSN (host=... sslmode=x). An absent sslmode yields "prefer",
// libpq's default. Errors never include the DSN.
func SSLMode(dsn string) (string, error) {
	dsn = strings.TrimSpace(dsn)
	lower := strings.ToLower(dsn)
	if strings.HasPrefix(lower, "postgres://") || strings.HasPrefix(lower, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("invalid connection URL")
		}
		if m := u.Query().Get("sslmode"); m != "" {
			return strings.ToLower(m), nil
		}
		return "prefer", nil
	}
	for _, kv := range strings.Fields(dsn) {
		k, v, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(k, "sslmode") {
			return strings.ToLower(strings.Trim(v, "'")), nil
		}
	}
	return "prefer", nil
}

// IsVerifiedSSLMode reports whether mode authenticates the server certificate.
func IsVerifiedSSLMode(mode string) bool {
	switch strings.ToLower(mode) {
	case "verify-ca", "verify-full":
		return true
	}
	return false
}
