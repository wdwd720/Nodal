//go:build integration

package assets

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
)

// internal/assets had no tests of its own. `docs/build/REQUIREMENTS_TRACEABILITY.md`
// says so under R-032-1 and names the case it wanted: "same ticker, two mints".
// This harness is the first one here; it exists for the kind/domain parity
// check beside it, which makes true a claim `valueDomainProblems` had been
// making for a while.

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
)

func TestMain(m *testing.M) { os.Exit(testMain(m)) }

func testMain(m *testing.M) int {
	if testAppURL == "" || testMigrateURL == "" {
		return m.Run()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		fmt.Fprintln(os.Stderr, "assets integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "assets-itest", MaxConns: 4}); err != nil {
		fmt.Fprintln(os.Stderr, "assets integration: open pool:", err)
		return 1
	}
	defer testDB.Close()
	return m.Run()
}

func requireEnv(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("CP_TEST_DATABASE_URL / CP_TEST_MIGRATE_DATABASE_URL not set; skipping integration test")
	}
}
