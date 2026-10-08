package sandboxrt

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
)

// scripted is an engine that never runs anything.
type scripted struct{}

func (scripted) Run(context.Context, RunRequest) (RawRun, error) {
	return RawRun{}, errs.New(errs.CodeInternal, "scripted: nothing runs here")
}

// refusingConns is a budget pool that never has a connection free.
type refusingConns struct{}

func (refusingConns) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func unitDeps(t *testing.T) (clock.Clock, *econ.Authorizer, audit.Writer) {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	checker, err := gates.NewChecker("TEST", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	return clk, econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{})), audit.NewWriterWithBuildVersion("unit")
}

// TestReservationKeys_NameTheAccountAndTheRun: an account's hold lock is a
// function of the account alone; a run's tag is the low 32 bits of its id;
// and a run id made for a reserved tag is a fresh, well-formed version 7 id
// carrying exactly that tag, as its charge's run_id will show it.
func TestReservationKeys_NameTheAccountAndTheRun(t *testing.T) {
	a, b := accounts.AccountID(accounts.NewAccountID()), accounts.AccountID(accounts.NewAccountID())
	assert.Equal(t, holdKey(a), holdKey(a))
	assert.NotEqual(t, holdKey(a), holdKey(b))

	for _, tag := range []uint32{0, 1, 0x7fffffff, 0x80000000, 0xffffffff} {
		one, err := runIDWithTag(tag)
		require.NoError(t, err)
		two, err := runIDWithTag(tag)
		require.NoError(t, err)
		assert.Equal(t, tag, runTag(one))
		assert.NotEqual(t, one, two, "the same tag, two distinct runs")
		parsed, err := ParseRunID(one.String())
		require.NoError(t, err, "still a canonical version 7 id")
		assert.Equal(t, one, parsed)
		assert.True(t, strings.HasSuffix(one.String(), fmt.Sprintf("%08x", tag)),
			"the tag is the id's last eight hex digits, which is what the reservation query reads")
	}
	tags, err := distinctTags(100)
	require.NoError(t, err)
	seen := map[uint32]bool{}
	for _, tag := range tags {
		assert.False(t, seen[tag])
		seen[tag] = true
	}
	assert.Less(t, holdClass+maxWallTimeMS, int64(1)<<31, "every run lock's first key fits an int4")
}

// TestOpenBudgetPool_IsSmallAndOfItsOwn: the budget's pool opens on a
// database, with at most MaxBudgetConns connections.
func TestOpenBudgetPool_IsSmallAndOfItsOwn(t *testing.T) {
	ctx := context.Background()
	_, err := OpenBudgetPool(ctx, nil, 2)
	assert.Equal(t, errs.CodeInternal, errs.CodeOf(err))
	for _, n := range []int32{0, -1, MaxBudgetConns + 1} {
		_, err := OpenBudgetPool(ctx, new(db.DB), n)
		assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err), "%d connections", n)
	}
	assert.LessOrEqual(t, DefaultBudgetConns, MaxBudgetConns)
	assert.LessOrEqual(t, MaxBudgetConns, 4, "a few connections serve many runs: none is held while code runs")
}

// TestBudgetTx_AWaitThatRunsOutIsContendedNeverInternal: a budget connection
// that is not free within the wait is a RATE_LIMITED refusal to retry, and so
// is a lock wait that runs out; a caller that went away gets its own
// context's error, not a refusal.
func TestBudgetTx_AWaitThatRunsOutIsContendedNeverInternal(t *testing.T) {
	clk, auth, aw := unitDeps(t)
	s := NewService(clk, auth, scripted{}, aw, refusingConns{})
	ran := false
	err := s.budgetTx(context.Background(), 50*time.Millisecond, func(context.Context, pgx.Tx) error { ran = true; return nil })
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, errs.CodeRateLimited, e.Code)
	assert.Equal(t, BudgetContended, e.Fields["reason"])
	require.NotNil(t, e.RetryAfter)
	assert.False(t, ran, "nothing ran without a connection")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.budgetTx(ctx, time.Second, func(context.Context, pgx.Tx) error { return nil })
	assert.ErrorIs(t, err, context.Canceled)

	lockTimeout := errs.Wrap(&pgconn.PgError{Code: db.SQLStateLockNotAvailable}, errs.CodeInternal, "sandboxrt: lock sandbox budget")
	refusal, ok := errs.As(classifyBudget(lockTimeout, time.Second))
	require.True(t, ok)
	assert.Equal(t, errs.CodeRateLimited, refusal.Code)
	assert.Equal(t, BudgetContended, refusal.Fields["reason"])
	other := errs.New(errs.CodeNotFound, "x")
	assert.Same(t, other, classifyBudget(other, time.Second))
}
