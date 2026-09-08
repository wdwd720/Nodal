//go:build integration

package valuedomain

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
)

// The finality transition table, compared across the two languages that hold it.
//
// Migration 00711 says, above `cp_credit_finality_can_transition`:
//
//	Mirrors internal/valuedomain.CanTransitionFinality. A test asserts the two
//	agree on every pair.
//
// No such test existed. The two tables happened to be identical, so the comment
// was a false claim about a control rather than a divergence — which is the
// F-14/F-18/F-25/F-40 shape, and it was sitting in the place a reader is most
// likely to take on trust: directly above the function it describes.
//
// It is exhaustive rather than sampled. There are five finality values, so the
// whole space is twenty-five pairs and there is no reason to guess at a subset.

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
		fmt.Fprintln(os.Stderr, "valuedomain integration: migrate up:", err)
		return 1
	}
	var err error
	if testDB, err = db.Open(ctx, db.Config{URL: testAppURL, AppName: "vd-itest", MaxConns: 4}); err != nil {
		fmt.Fprintln(os.Stderr, "valuedomain integration: open pool:", err)
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

func TestIntegration_GoAndSQLAgreeOnEveryFinalityTransition(t *testing.T) {
	requireEnv(t)
	all := AllFinalities()
	require.NotEmpty(t, all, "no finality values declared; this test would compare nothing")

	var disagreements []string
	permitted := 0
	for _, from := range all {
		for _, to := range all {
			var sqlSays bool
			require.NoError(t, testDB.QueryRow(t.Context(),
				`SELECT cp_credit_finality_can_transition($1, $2)`, string(from), string(to)).Scan(&sqlSays))
			goSays := CanTransitionFinality(from, to)
			if goSays {
				permitted++
			}
			if goSays != sqlSays {
				disagreements = append(disagreements,
					string(from)+" -> "+string(to)+": Go says "+yesNo(goSays)+", SQL says "+yesNo(sqlSays))
			}
		}
	}
	sort.Strings(disagreements)
	assert.Empty(t, disagreements,
		"%d finality transition(s) are judged differently by Go and by SQL; CR003 is raised from the "+
			"SQL copy, so a disagreement means the guard and the service refuse different things:\n  %v",
		len(disagreements), disagreements)

	// Negative controls. A table that permitted everything, or nothing, would
	// agree with any other such table and prove nothing about a state machine.
	assert.Positive(t, permitted, "no finality transition is permitted anywhere; the comparison is vacuous")
	assert.Less(t, permitted, len(all)*len(all),
		"every finality transition is permitted; there would be no state machine to mirror")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
