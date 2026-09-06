//go:build integration && chaos

package chaos

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
)

// The suite runs against an isolated database provisioned by
//
//	go run ./scripts/testdb -name chaos -export
//
// never the shared controlplane_test: it terminates backends and pauses the
// Postgres container, which would be hostile to any other suite sharing it.
var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	testMigrate    *pgxpool.Pool
	testDBName     string
	adminDSN       string
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run() // every test skips with a reason
	}
	name, err := databaseName(testAppURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos: parse CP_TEST_DATABASE_URL:", err)
		return 1
	}
	testDBName = name
	if strings.EqualFold(name, "controlplane_test") || strings.EqualFold(name, "controlplane") {
		fmt.Fprintf(os.Stderr, "chaos: refusing to run against the shared database %q; "+
			"provision one with `go run ./scripts/testdb -name chaos -export`\n", name)
		return 1
	}
	adminDSN = adminDSNFor(testMigrateURL)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "chaos: migrate up:", err)
		return 1
	}
	testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "chaos-itest", MaxConns: 20})
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos: open app pool:", err)
		return 1
	}
	testMigrate, err = pgxpool.New(ctx, testMigrateURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos: open migrate pool:", err)
		return 1
	}
	code := m.Run()
	testMigrate.Close()
	testDB.Close()
	return code
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; " +
			"provision with `go run ./scripts/testdb -name chaos -export`")
	}
}

func databaseName(dsn string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	return strings.TrimPrefix(u.Path, "/"), nil
}

// adminDSNFor rewrites a DSN onto the `postgres` maintenance database with the
// admin role, so a test can terminate every backend on its own database from
// a connection that is not itself one of them.
func adminDSNFor(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	u.User = url.UserPassword("cp_admin", "cp_admin_local")
	u.Path = "/postgres"
	return u.String()
}

func pgxConnect(ctx context.Context, dsn string) (*pgx.Conn, error) { return pgx.Connect(ctx, dsn) }

// --- fixtures ---------------------------------------------------------------

// newAccount creates a user and a customer account. Chaos tests must be
// re-runnable against one database — posted ledger rows are never deletable —
// so every test owns fresh identifiers and asserts only about its own.
func newAccount(t *testing.T) (accounts.UserID, accounts.AccountID) {
	t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(context.Background(), testDB, "chaos-itest", "sub-"+uuid.NewString(), nil)
	require.NoError(t, err)
	a, err := repo.CreateAccount(context.Background(), testDB, u.ID, accounts.KindCustomer)
	require.NoError(t, err)
	return u.ID, a.ID
}

func newAsset(t *testing.T, symbol string, decimals uint8, stable bool) assets.AssetID {
	t.Helper()
	a := assets.Asset{
		Chain: "solana-chaos-" + uuid.NewString(), MintAddress: uuid.NewString(), Kind: assets.KindSPLToken,
		Symbol: symbol, Name: symbol, Decimals: decimals, IsStablecoin: stable,
		RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	}
	if stable {
		a.PegCurrency = "USD"
	}
	created, err := assets.NewRepository().Create(context.Background(), testDB, a)
	require.NoError(t, err)
	return created.ID
}

// chaosToken is a short unique suffix so fixtures never collide across runs.
func chaosToken() string { return strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }

// waitDB blocks until the pool can answer a trivial query again, which is how
// a client learns the database came back.
func waitDB(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var one int
		last = testDB.Pool().QueryRow(ctx, "SELECT 1").Scan(&one)
		cancel()
		if last == nil && one == 1 {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	require.NoErrorf(t, last, "database never answered again within %s", within)
}
