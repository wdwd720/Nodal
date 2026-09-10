//go:build integration

package capacity

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/errs"
)

// These run the guard's real SQL against a real PostgreSQL. The unit tests use
// a stub querier and prove the DECISIONS; this proves the QUERIES -- that they
// parse, that they name columns and states this schema actually has, and that
// the numbers they return are the ones the ceilings are compared against.
//
// Nothing here writes a row. Every threshold is tested by measuring what is
// already there and setting the ceiling relative to it, which means the same
// test is safe against a scratch database and against a live one.

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
		fmt.Fprintln(os.Stderr, "capacity integration: migrate up:", err)
		return 1
	}
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "capacity-integration", MaxConns: 4})
	if err != nil {
		fmt.Fprintln(os.Stderr, "capacity integration: open:", err)
		return 1
	}
	testDB = d
	defer d.Close()
	return m.Run()
}

func requireDB(t *testing.T) *db.DB {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
	return testDB
}

// bigBudget is a budget no real database will be anywhere near, so a test can
// measure without being refused for an unrelated reason.
func bigBudget() Budget {
	return Budget{
		MaxAccounts:        1 << 40,
		MaxPurchasesPerDay: 1 << 40,
		MaxAtRiskMinor:     1 << 40,
		MaxDatabaseBytes:   1 << 50,
	}
}

// TestIntegration_EveryMeasurementRunsAgainstTheRealSchema.
//
// The stub tests cannot catch a column that does not exist, a funding state
// spelled differently in SQL than in Go, or a function this Postgres does not
// have. This is the test that would have caught all three.
func TestIntegration_EveryMeasurementRunsAgainstTheRealSchema(t *testing.T) {
	d := requireDB(t)
	g, err := NewGuard(bigBudget(), time.Now)
	require.NoError(t, err)

	r, err := g.Measure(context.Background(), d)
	require.NoError(t, err, "every measurement must run against the real schema")

	assert.GreaterOrEqual(t, r.Accounts, int64(0))
	assert.GreaterOrEqual(t, r.PurchasesToday, int64(0))
	assert.GreaterOrEqual(t, r.AtRiskMinor, int64(0))
	assert.Positive(t, r.DatabaseBytes, "a live database occupies some bytes")
	assert.False(t, r.MeasuredAt.IsZero())
}

// decidedFundingStates are the funding states the money-at-risk sum leaves
// out: terminal ones, plus SETTLED. internal/credit is the authority on that
// classification and its own test enforces it against the state machine; the
// list is repeated here so this test can check the DATABASE knows exactly the
// same states and no others. internal/capacity cannot import internal/credit
// -- internal/credit imports it -- so the schema check has to state the set
// rather than derive it.
var decidedFundingStates = []string{"SETTLED", "REVERSED", "REFUNDED", "FAILED", "CANCELED"}

// TestIntegration_TheSchemaAndTheAtRiskSumKnowTheSameFundingStates.
//
// The money-at-risk sum filters on funding states by literal string. A state
// spelled differently in SQL than in the schema does not fail: the query runs
// and returns a smaller number, so the ceiling is measured against less
// exposure than really exists. That is the one direction that costs money, and
// it is invisible to every test that does not touch a real database.
//
// So both directions are checked against the constraint Postgres itself
// enforces: every state the sum counts must exist in the schema, and every
// state the schema allows must be either counted or deliberately decided.
func TestIntegration_TheSchemaAndTheAtRiskSumKnowTheSameFundingStates(t *testing.T) {
	d := requireDB(t)
	ctx := context.Background()

	// The state list is a table-level CHECK, not an enum type, so it has to be
	// read from the constraint definition rather than from pg_enum.
	var def string
	err := d.QueryRow(ctx, `
		SELECT pg_get_constraintdef(c.oid)
		  FROM pg_constraint c
		  JOIN pg_class t ON t.oid = c.conrelid
		 WHERE t.relname = 'credit_fundings'
		   AND c.contype = 'c'
		   AND pg_get_constraintdef(c.oid) LIKE '%(state%'
		 LIMIT 1`).Scan(&def)
	require.NoError(t, err, "credit_fundings must constrain its state column; without it nothing enforces the state machine at all")

	inSchema := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([A-Z_]+)'`).FindAllStringSubmatch(def, -1) {
		inSchema[m[1]] = true
	}
	require.NotEmpty(t, inSchema, "could not read any state out of %q", def)

	counted := map[string]bool{}
	for _, s := range AtRiskFundingStates() {
		counted[s] = true
		assert.True(t, inSchema[s],
			"the money-at-risk sum counts %q and the schema has no such state, so that money is silently excluded from the ceiling", s)
	}

	decided := map[string]bool{}
	for _, s := range decidedFundingStates {
		decided[s] = true
		assert.True(t, inSchema[s], "the schema has no state %q", s)
	}

	for s := range inSchema {
		assert.True(t, counted[s] || decided[s],
			"the schema allows funding state %q, which the money-at-risk ceiling neither counts nor treats as decided -- money in it is invisible to the cap", s)
	}
}

// TestIntegration_TheAtRiskSumActuallySeesCapturedMoney.
//
// The classification tests prove the LIST is right. This proves the QUERY uses
// it: the same sum is run with the shipped states and again with the four the
// guard used to name, against the real table. If the guard's number were still
// coming from the old set, the two would agree by construction.
func TestIntegration_TheAtRiskSumActuallySeesCapturedMoney(t *testing.T) {
	d := requireDB(t)
	ctx := context.Background()

	g, err := NewGuard(bigBudget(), time.Now)
	require.NoError(t, err)
	r, err := g.Measure(ctx, d)
	require.NoError(t, err)

	var direct int64
	require.NoError(t, d.QueryRow(ctx,
		`SELECT coalesce(sum(paid_amount_minor), 0)::bigint FROM credit_fundings WHERE state = ANY($1)`,
		AtRiskFundingStates()).Scan(&direct))
	assert.Equal(t, direct, r.AtRiskMinor, "the guard's money-at-risk number must be the sum over the states it publishes")

	// And the states omitted from the sum really are absent from it.
	var decidedSum int64
	require.NoError(t, d.QueryRow(ctx,
		`SELECT coalesce(sum(paid_amount_minor), 0)::bigint FROM credit_fundings WHERE state = ANY($1)`,
		decidedFundingStates).Scan(&decidedSum))
	var everything int64
	require.NoError(t, d.QueryRow(ctx,
		`SELECT coalesce(sum(paid_amount_minor), 0)::bigint FROM credit_fundings`).Scan(&everything))
	assert.Equal(t, everything, direct+decidedSum,
		"every row's money is either at risk or decided; a difference means a state neither list knows about")
}

// TestIntegration_EachCeilingRefusesAtTheMeasuredValue walks every ceiling to
// exactly what the database currently reports and checks the guard refuses --
// then one above it and checks the guard admits.
//
// Setting the ceiling from the measurement rather than inserting rows is what
// makes this safe to run against a live database: it writes nothing and still
// exercises the real number.
func TestIntegration_EachCeilingRefusesAtTheMeasuredValue(t *testing.T) {
	d := requireDB(t)
	ctx := context.Background()

	base, err := NewGuard(bigBudget(), time.Now)
	require.NoError(t, err)
	r, err := base.Measure(ctx, d)
	require.NoError(t, err)

	cases := []struct {
		name    string
		action  Action
		atLimit Budget
		below   Budget
		ceiling string
	}{
		{
			"launch cohort", ActionOpenAccount,
			Budget{MaxAccounts: max64(r.Accounts, 1)},
			Budget{MaxAccounts: r.Accounts + 1},
			"launch cohort",
		},
		{
			"daily purchase volume", ActionCreditPurchase,
			Budget{MaxPurchasesPerDay: max64(r.PurchasesToday, 1)},
			Budget{MaxPurchasesPerDay: r.PurchasesToday + 1},
			"daily purchase volume",
		},
		{
			"money at risk", ActionCreditPurchase,
			Budget{MaxAtRiskMinor: max64(r.AtRiskMinor, 1)},
			Budget{MaxAtRiskMinor: r.AtRiskMinor + 1},
			"money at risk",
		},
		{
			// Headroom is a fraction, so the ceiling that bites is
			// bytes/headroom rounded down.
			"database storage", ActionCreditPurchase,
			Budget{MaxDatabaseBytes: int64(float64(r.DatabaseBytes) / DefaultDatabaseHeadroom)},
			Budget{MaxDatabaseBytes: int64(float64(r.DatabaseBytes)/DefaultDatabaseHeadroom) * 4},
			"database storage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			atLimit, err := NewGuard(tc.atLimit, time.Now)
			require.NoError(t, err)
			_, err = atLimit.Admit(ctx, d, tc.action)
			require.Error(t, err, "%s must refuse at the measured value", tc.name)
			assert.ErrorIs(t, err, ErrAtCapacity)
			assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
			assert.Contains(t, err.Error(), tc.ceiling)

			below, err := NewGuard(tc.below, time.Now)
			require.NoError(t, err)
			_, err = below.Admit(ctx, d, tc.action)
			require.NoError(t, err, "%s must admit below the ceiling", tc.name)
		})
	}
}

// TestIntegration_AnAmountCannotCarryTheTierPastItsCeiling, measured against
// the real exposure rather than a stub.
func TestIntegration_AnAmountCannotCarryTheTierPastItsCeiling(t *testing.T) {
	d := requireDB(t)
	ctx := context.Background()

	base, err := NewGuard(bigBudget(), time.Now)
	require.NoError(t, err)
	r, err := base.Measure(ctx, d)
	require.NoError(t, err)

	// A ceiling 1000 minor units above whatever is currently at risk.
	g, err := NewGuard(Budget{MaxAtRiskMinor: r.AtRiskMinor + 1000}, time.Now)
	require.NoError(t, err)

	_, err = g.Admit(ctx, d, ActionCreditPurchase)
	require.NoError(t, err, "current exposure is below the ceiling")

	_, err = g.AdmitAmount(ctx, d, ActionCreditPurchase, 1000)
	require.NoError(t, err, "an amount that exactly fits is admitted")

	_, err = g.AdmitAmount(ctx, d, ActionCreditPurchase, 1001)
	require.Error(t, err, "one unit past the ceiling must be refused")
	assert.ErrorIs(t, err, ErrAtCapacity)
	assert.Contains(t, err.Error(), "money at risk")
}

// TestIntegration_AFailedMeasurementIsARefusal is the hard rule, against a real
// connection rather than a stub that was told to fail.
//
// A cancelled context is the honest way to produce a real database failure: the
// query is genuinely attempted and genuinely does not answer, which is what a
// network partition or an exhausted free-tier quota looks like from here.
func TestIntegration_AFailedMeasurementIsARefusal(t *testing.T) {
	d := requireDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g, err := NewGuard(LaunchTier(), time.Now)
	require.NoError(t, err)

	_, err = g.Admit(ctx, d, ActionCreditPurchase)
	require.Error(t, err, "a guard that cannot measure must refuse, not admit")
	assert.ErrorIs(t, err, ErrUnmeasurable)
	assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
}

// TestIntegration_TheLaunchTierBudgetIsSatisfiableToday: the shipped ceilings
// must actually admit an action against the database as it stands. A budget
// that refuses everything from the moment it is deployed is not a ceiling, it
// is an outage.
func TestIntegration_TheLaunchTierBudgetIsSatisfiableToday(t *testing.T) {
	d := requireDB(t)
	g, err := NewGuard(LaunchTier(), at("2026-09-10T00:00:00Z"))
	require.NoError(t, err)

	r, err := g.Admit(context.Background(), d, ActionCreditPurchase)
	require.NoError(t, err, "the launch tier ceilings must admit a purchase on a fresh deployment")

	// And there is real headroom rather than a value that happens to squeak in.
	b := LaunchTier()
	assert.Less(t, r.DatabaseBytes, int64(float64(b.MaxDatabaseBytes)*b.DatabaseHeadroom)/2,
		"the database is already past half the usable quota; the tier needs migrating, not testing")
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
