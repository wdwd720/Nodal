package capacity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/errs"
)

func at(t string) func() time.Time {
	ts, err := time.Parse(time.RFC3339, t)
	if err != nil {
		panic(err)
	}
	return func() time.Time { return ts }
}

// stubRow answers one Scan with one int64, or an error.
type stubRow struct {
	v   int64
	err error
}

func (r stubRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) != 1 {
		return errors.New("stub: expected one destination")
	}
	p, ok := dest[0].(*int64)
	if !ok {
		return errors.New("stub: expected *int64")
	}
	*p = r.v
	return nil
}

// stubQuerier answers the guard's queries by matching on the table each one
// names, rather than by call order. Order-based stubs pass when the guard
// silently stops asking a question, which is the failure most worth catching.
type stubQuerier struct {
	accounts, purchases, atRisk, dbBytes int64
	failOn                               string // substring of the query to fail
	asked                                []string
}

func (q *stubQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.asked = append(q.asked, sql)
	if q.failOn != "" && strings.Contains(sql, q.failOn) {
		return stubRow{err: errors.New("connection reset by peer")}
	}
	switch {
	case strings.Contains(sql, "FROM accounts"):
		return stubRow{v: q.accounts}
	case strings.Contains(sql, "interval '24 hours'"):
		return stubRow{v: q.purchases}
	case strings.Contains(sql, "paid_amount_minor"):
		return stubRow{v: q.atRisk}
	case strings.Contains(sql, "pg_database_size"):
		return stubRow{v: q.dbBytes}
	}
	return stubRow{err: errors.New("stub: unexpected query: " + sql)}
}

func (q *stubQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errors.New("stub: Exec not expected")
}

func (q *stubQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("stub: Query not expected")
}

// TestBudgetWithNoCeilingIsRefused. A guard that guards nothing is worse than
// no guard at all: the deployment reads as protected and is not.
func TestBudgetWithNoCeilingIsRefused(t *testing.T) {
	t.Parallel()
	_, err := NewGuard(Budget{}, at("2026-09-09T12:00:00Z"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "guards nothing")

	_, err = NewGuard(Budget{MaxAccounts: -1}, at("2026-09-09T12:00:00Z"))
	require.Error(t, err, "a negative ceiling is a typo, not a policy")

	_, err = NewGuard(LaunchTier(), nil)
	require.Error(t, err, "no clock")

	_, err = NewGuard(Budget{MaxAccounts: 1, DatabaseHeadroom: 1.5}, at("2026-09-09T12:00:00Z"))
	require.Error(t, err, "headroom above 1 would stop above the quota, which is never")
}

// TestUnmeasurableIsRefusal is the hard rule.
//
// The requirement is that the system fails closed before crossing a free-tier
// limit. A guard that admits an action because the database would not tell it
// how full the database is has failed open at the exact moment it mattered.
func TestUnmeasurableIsRefusal(t *testing.T) {
	t.Parallel()
	for _, q := range []string{"FROM accounts", "interval '24 hours'", "paid_amount_minor", "pg_database_size"} {
		t.Run(q, func(t *testing.T) {
			t.Parallel()
			g, err := NewGuard(LaunchTier(), at("2026-09-09T12:00:00Z"))
			require.NoError(t, err)

			_, err = g.Admit(context.Background(), &stubQuerier{failOn: q}, ActionCreditPurchase)
			require.Error(t, err, "a measurement that failed must refuse, not admit")
			assert.ErrorIs(t, err, ErrUnmeasurable)
			assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
		})
	}

	g, err := NewGuard(LaunchTier(), at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)
	_, err = g.Admit(context.Background(), nil, ActionCreditPurchase)
	require.Error(t, err, "no querier is also unmeasurable")
}

// TestEveryCeilingRefusesAtItsLimit walks each ceiling to the value that
// should stop it, and checks the one below it still passes. The pair matters:
// a ceiling that refuses everything looks identical to one that works.
func TestEveryCeilingRefusesAtItsLimit(t *testing.T) {
	t.Parallel()
	b := LaunchTier()

	cases := []struct {
		name    string
		action  Action
		atLimit stubQuerier
		below   stubQuerier
		ceiling string
	}{
		{
			"launch cohort", ActionOpenAccount,
			stubQuerier{accounts: b.MaxAccounts},
			stubQuerier{accounts: b.MaxAccounts - 1},
			"launch cohort",
		},
		{
			"daily purchase volume", ActionCreditPurchase,
			stubQuerier{purchases: b.MaxPurchasesPerDay},
			stubQuerier{purchases: b.MaxPurchasesPerDay - 1},
			"daily purchase volume",
		},
		{
			"money at risk", ActionCreditPurchase,
			stubQuerier{atRisk: b.MaxAtRiskMinor},
			stubQuerier{atRisk: b.MaxAtRiskMinor - 1},
			"money at risk",
		},
		{
			"database storage", ActionCreditPurchase,
			stubQuerier{dbBytes: int64(float64(b.MaxDatabaseBytes) * b.DatabaseHeadroom)},
			stubQuerier{dbBytes: int64(float64(b.MaxDatabaseBytes)*b.DatabaseHeadroom) - 1},
			"database storage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			g, err := NewGuard(b, at("2026-09-09T12:00:00Z"))
			require.NoError(t, err)

			atLimit := tc.atLimit
			_, err = g.Admit(context.Background(), &atLimit, tc.action)
			require.Error(t, err, "%s must refuse at its ceiling", tc.name)
			assert.ErrorIs(t, err, ErrAtCapacity)
			assert.Equal(t, errs.CodeAtCapacity, errs.CodeOf(err))
			assert.Contains(t, err.Error(), tc.ceiling, "the refusal names which ceiling")

			below := tc.below
			_, err = g.Admit(context.Background(), &below, tc.action)
			require.NoError(t, err, "%s must admit below its ceiling", tc.name)
		})
	}
}

// TestDatabaseCeilingStopsBeforeTheQuota: stopping AT the quota means the write
// that discovers it is the write that fails, and there is then no room to
// record the refusal, run reconciliation or export the ledger.
func TestDatabaseCeilingStopsBeforeTheQuota(t *testing.T) {
	t.Parallel()
	b := LaunchTier()
	g, err := NewGuard(b, at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)

	// Well inside the quota, but past the headroom fraction.
	used := int64(float64(b.MaxDatabaseBytes) * 0.90)
	assert.Less(t, used, b.MaxDatabaseBytes, "the fixture is inside the quota")

	_, err = g.Admit(context.Background(), &stubQuerier{dbBytes: used}, ActionCreditPurchase)
	require.Error(t, err, "the ceiling must bite before the quota does")
	assert.Contains(t, err.Error(), "database storage")
}

// TestDatabaseCeilingAppliesToEveryAction: running out of database is not
// specific to what was being written when it happened.
func TestDatabaseCeilingAppliesToEveryAction(t *testing.T) {
	t.Parallel()
	b := LaunchTier()
	g, err := NewGuard(b, at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)
	full := int64(float64(b.MaxDatabaseBytes) * 0.99)

	for _, a := range []Action{ActionOpenAccount, ActionCreditPurchase} {
		_, err := g.Admit(context.Background(), &stubQuerier{dbBytes: full}, a)
		require.Error(t, err, "%s", a)
		assert.Contains(t, err.Error(), "database storage")
	}
}

// TestAmountCannotVaultOverTheCeiling is the case a current-exposure check
// misses entirely: at one unit below the ceiling, any amount at all is
// admitted, and the tier ends up past a cap it was given.
func TestAmountCannotVaultOverTheCeiling(t *testing.T) {
	t.Parallel()
	b := LaunchTier()
	g, err := NewGuard(b, at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)

	// Almost full, and Admit alone is happy.
	near := stubQuerier{atRisk: b.MaxAtRiskMinor - 1_000}
	_, err = g.Admit(context.Background(), &near, ActionCreditPurchase)
	require.NoError(t, err, "current exposure is below the ceiling")

	// The same state, plus what this purchase would add.
	_, err = g.AdmitAmount(context.Background(), &near, ActionCreditPurchase, 50_000)
	require.Error(t, err, "a purchase may not carry the tier past its own ceiling")
	assert.ErrorIs(t, err, ErrAtCapacity)
	assert.Contains(t, err.Error(), "money at risk")

	// And an amount that fits exactly is admitted, or the ceiling would be
	// unusable rather than protective.
	_, err = g.AdmitAmount(context.Background(), &near, ActionCreditPurchase, 1_000)
	require.NoError(t, err)
}

// TestUnknownActionIsRefused: a new financial action that forgot to declare
// its ceilings must not inherit "no ceilings apply".
func TestUnknownActionIsRefused(t *testing.T) {
	t.Parallel()
	g, err := NewGuard(LaunchTier(), at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)

	_, err = g.Admit(context.Background(), &stubQuerier{}, Action("WITHDRAWAL"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "declares no ceilings")
}

// TestZeroCeilingIsNotMeasured: the paid tier turns off the infrastructure
// ceiling and keeps the financial ones. A zero ceiling must also skip its
// query, or the paid tier would pay for a measurement it does not use -- and,
// worse, would fail closed on a database that has no quota to report.
func TestZeroCeilingIsNotMeasured(t *testing.T) {
	t.Parallel()
	g, err := NewGuard(Budget{MaxAtRiskMinor: 1_000_000}, at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)

	q := &stubQuerier{atRisk: 10, failOn: "pg_database_size"}
	_, err = g.Admit(context.Background(), q, ActionCreditPurchase)
	require.NoError(t, err, "a disabled ceiling must not be measured, so its failure cannot matter")

	joined := strings.Join(q.asked, " ")
	assert.NotContains(t, joined, "pg_database_size")
	assert.NotContains(t, joined, "FROM accounts")
	assert.Contains(t, joined, "paid_amount_minor")
}

// TestReadingIsReturnedOnRefusal: the number that caused the refusal is the
// useful part, so it comes back with the error rather than only in the message.
func TestReadingIsReturnedOnRefusal(t *testing.T) {
	t.Parallel()
	g, err := NewGuard(LaunchTier(), at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)

	r, err := g.Admit(context.Background(), &stubQuerier{accounts: 50}, ActionOpenAccount)
	require.Error(t, err)
	assert.Equal(t, int64(50), r.Accounts)
	assert.Equal(t, "2026-09-09T12:00:00Z", r.MeasuredAt.Format(time.RFC3339))
}

// TestLaunchTierMatchesTheQuotasItProtects. The numbers are not decoration:
// MaxDatabaseBytes is the free Postgres quota, and the headroom leaves room to
// export a full tier when things are going badly.
func TestLaunchTierMatchesTheQuotasItProtects(t *testing.T) {
	t.Parallel()
	b := LaunchTier()
	assert.Equal(t, int64(500*1024*1024), b.MaxDatabaseBytes, "the free Postgres storage quota")
	assert.Positive(t, b.MaxAccounts)
	assert.Positive(t, b.MaxPurchasesPerDay)
	assert.Positive(t, b.MaxAtRiskMinor)
	assert.Less(t, b.DatabaseHeadroom, 1.0, "stopping at the quota is stopping too late")
	assert.Greater(t, b.DatabaseHeadroom, 0.5, "stopping this early would waste most of the tier")

	g, err := NewGuard(b, at("2026-09-09T12:00:00Z"))
	require.NoError(t, err)
	assert.Equal(t, b, g.Budget())
}
