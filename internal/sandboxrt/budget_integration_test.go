//go:build integration

package sandboxrt

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
)

// holdingEngine runs every module through inner, and keeps a run whose input
// says hold=yes in progress -- after its module ran -- until release is
// closed, so a test can do things while that run is in flight, whatever the
// machine's load.
type holdingEngine struct {
	inner   Engine
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func newHoldingEngine(inner Engine) *holdingEngine {
	return &holdingEngine{inner: inner, started: make(chan struct{}), release: make(chan struct{})}
}

func (h *holdingEngine) Run(ctx context.Context, req RunRequest) (RawRun, error) {
	if req.Input.Config["hold"] != "yes" {
		return h.inner.Run(ctx, req)
	}
	h.once.Do(func() { close(h.started) })
	raw, err := h.inner.Run(ctx, req)
	select {
	case <-h.release:
		return raw, err
	case <-ctx.Done():
		return RawRun{}, ctx.Err()
	}
}

func held(in Input) Input {
	in.Config["hold"] = "yes"
	return in
}

// executeIn runs one request in its own transaction, bounded so a test that
// waits on a lock it must never wait on fails instead of hanging.
func (f *svcFixture) executeIn(req ExecuteRequest) (Execution, error) {
	ctx, cancel := context.WithTimeout(f.ctx, 45*time.Second)
	defer cancel()
	var e Execution
	err := testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = f.svc.Execute(ctx, tx, req)
		return err
	})
	return e, err
}

func (f *svcFixture) usage() BudgetUsage {
	f.t.Helper()
	u, err := f.svc.UsageOf(f.ctx, testDB, f.account)
	require.NoError(f.t, err)
	return u
}

func requireReason(t *testing.T, err error, code errs.Code, reason string) {
	t.Helper()
	e, ok := errs.As(err)
	require.True(t, ok, "%v", err)
	assert.Equal(t, code, e.Code, "%v", err)
	assert.Equal(t, reason, e.Fields["reason"], "%v", err)
}

// TestIntegration_ARunHoldsNoBudgetLockWhileItRuns: a deliberately slow
// module -- one that never returns and is closed at its wall-time limit --
// runs for an account while another run of the same account is reserved,
// run, charged and committed from start to finish. Neither waits on the
// other, neither is an internal error, and the budget holds: with a window of
// two runs, a third caller is refused RATE_LIMITED for the window while the
// slow run's reservation and the fast run's charge fill it, and when the slow
// run ends it is charged and its reservation is gone.
func TestIntegration_ARunHoldsNoBudgetLockWhileItRuns(t *testing.T) {
	f := newSvcFixture(t)
	two := smallBudget()
	two.MaxRunsPerWindow = 2
	f.withBudget(two)
	hold := newHoldingEngine(f.engine)
	f.withEngine(hold)
	slow, fast := f.artifact("cpu_loop"), f.artifact("momentum")

	slowLimits := testLimits()
	slowLimits.WallTimeMS = 1500
	slowReq := f.request(slow, held(testInput()))
	slowReq.Limits = slowLimits
	type outcome struct {
		e   Execution
		err error
	}
	slowDone := make(chan outcome, 1)
	go func() {
		e, err := f.executeIn(slowReq)
		slowDone <- outcome{e, err}
	}()
	select {
	case <-hold.started:
	case o := <-slowDone:
		t.Fatalf("the slow run ended before it started: %v", o.err)
	case <-time.After(45 * time.Second):
		t.Fatal("the slow run never started")
	}
	u := f.usage()
	assert.Equal(t, 1, u.InFlightRuns, "the slow run is reserved while it runs")
	assert.Equal(t, slowLimits.WallTimeMS, u.InFlightCPUMS)

	// A whole run of the same account while the slow one runs: reserved,
	// run, charged and committed. Were any budget lock held by the slow run,
	// this would wait for it to end -- and the slow run cannot end before
	// this returns.
	fastExec, err := f.executeIn(f.request(fast, testInput()))
	require.NoError(t, err, "a run charged while another of its account runs")
	assert.Equal(t, TerminationOK, fastExec.Result.Termination)
	u = f.usage()
	assert.Equal(t, 1, u.Runs, "the fast run is charged")
	assert.Equal(t, 1, u.InFlightRuns, "and the slow one is still reserved")

	// The window -- two runs -- holds the slow run's reservation and the fast
	// run's charge: a third caller is refused for the window, a coded
	// refusal, and nothing of it runs.
	started := f.engine.runs.Load()
	_, err = f.executeIn(f.request(fast, testInput()))
	requireReason(t, err, errs.CodeRateLimited, BudgetWindowRuns)
	assert.Equal(t, started, f.engine.runs.Load())

	close(hold.release)
	var o outcome
	select {
	case o = <-slowDone:
	case <-time.After(45 * time.Second):
		t.Fatal("the slow run never ended")
	}
	require.NoError(t, o.err, "the slow run is charged when it ends")
	assert.Equal(t, TerminationTimeout, o.e.Result.Termination, "closed at its wall-time limit")
	u = f.usage()
	assert.Equal(t, 2, u.Runs, "both runs are charged, once each")
	assert.Equal(t, slowLimits.WallTimeMS+testLimits().WallTimeMS, u.CPUMS)
	assert.Zero(t, u.InFlightRuns, "no reservation outlives its run")
	_, err = f.executeIn(f.request(fast, testInput()))
	requireReason(t, err, errs.CodeRateLimited, BudgetWindowRuns)
}

// TestIntegration_ACallerThatRollsBackLeavesNoReservation: a reservation
// lives in the caller's transaction and nowhere else, so a caller that rolls
// back -- after admitting a call and running none of it, or after its run was
// cut short -- leaves the account's budget exactly as its charges say. A run
// that did run stays charged after its caller rolls back: it ran, and
// counting it is the conservative choice. Nothing else lingers.
func TestIntegration_ACallerThatRollsBackLeavesNoReservation(t *testing.T) {
	f := newSvcFixture(t)
	f.withBudget(smallBudget()) // three runs per window, two per call
	digest := f.artifact("momentum")
	rollback := errors.New("rollback")

	// A call admitted whole -- two runs reserved -- and rolled back before
	// running any of them.
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		require.NoError(t, f.svc.Admit(ctx, tx, f.account, 2, testLimits()))
		u := f.usage()
		assert.Equal(t, 2, u.InFlightRuns, "every other connection sees the call's two runs reserved")
		assert.Zero(t, u.Runs)
		// While they are reserved, another caller cannot take their room.
		other := f.inTx(func(ctx context.Context, tx2 pgx.Tx) error {
			return f.svc.Admit(ctx, tx2, f.account, 2, testLimits())
		})
		requireReason(t, other, errs.CodeRateLimited, BudgetWindowRuns)
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	u := f.usage()
	assert.Zero(t, u.InFlightRuns, "the rollback took the reservations with it")
	assert.Zero(t, u.Runs, "and charged nothing")

	// A run that ran, whose caller then rolls back: the run is charged -- one
	// run, the CPU it was granted, the wall time it took -- and that is all.
	var ran Execution
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ran, err = f.svc.Execute(ctx, tx, f.request(digest, testInput()))
		require.NoError(t, err)
		return rollback
	})
	require.ErrorIs(t, err, rollback)
	u = f.usage()
	assert.Equal(t, 1, u.Runs, "the run ran, and stays on the budget")
	assert.Equal(t, testLimits().WallTimeMS, u.CPUMS)
	assert.Equal(t, ran.Result.Usage.WallTime.Milliseconds(), u.WallMS)
	assert.Zero(t, u.InFlightRuns)
	rec, err := f.svc.Run(f.ctx, testDB, ran.RunID)
	require.NoError(t, err, "with its record")
	assert.Equal(t, TerminationOK, rec.Termination)

	// A run cut short -- its caller's context ends while the module runs --
	// whose caller rolls back: nothing is charged, nothing is reserved.
	hold := newHoldingEngine(f.engine)
	f.withEngine(hold)
	ctx, cancel := context.WithCancel(f.ctx)
	cut := make(chan error, 1)
	go func() {
		cut <- testDB.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
			_, err := f.svc.Execute(ctx, tx, f.request(digest, held(testInput())))
			return err
		})
	}()
	<-hold.started
	assert.Equal(t, 1, f.usage().InFlightRuns, "reserved while it runs")
	cancel()
	require.Error(t, <-cut)
	u = f.usage()
	assert.Equal(t, 1, u.Runs, "the cut run charged nothing")
	assert.Zero(t, u.InFlightRuns, "and reserves nothing once its caller is gone")

	// The window is back to its normal accounting: one charge, room for two.
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		return f.svc.Admit(ctx, tx, f.account, 2, testLimits())
	}))
}

// TestIntegration_AnAdmittedCallKeepsItsRoom: a call Admit admitted is
// reserved whole for its transaction. A concurrent caller takes only what is
// left; the call's own runs take up its reservations -- never refused for the
// room they were admitted with, never counted twice -- and the call ends
// holding nothing.
func TestIntegration_AnAdmittedCallKeepsItsRoom(t *testing.T) {
	f := newSvcFixture(t)
	f.withBudget(smallBudget()) // three runs per window, two per call
	digest := f.artifact("momentum")
	var calls []Execution
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		require.NoError(t, f.svc.Admit(ctx, tx, f.account, 2, testLimits()))
		// Another caller runs once in the room left, and then there is none.
		_, err := f.executeIn(f.request(digest, testInput()))
		require.NoError(t, err)
		_, err = f.executeIn(f.request(digest, testInput()))
		requireReason(t, err, errs.CodeRateLimited, BudgetWindowRuns)
		// The call's own runs still run: each takes up one reservation.
		for i := 0; i < 2; i++ {
			e, err := f.svc.Execute(ctx, tx, f.request(digest, testInput()))
			require.NoError(t, err, "run %d of the admitted call", i+1)
			calls = append(calls, e)
			u := f.usage()
			assert.Equal(t, 2+i, u.Runs)
			assert.Equal(t, 1-i, u.InFlightRuns, "a charged run is not counted again")
		}
		return nil
	})
	require.NoError(t, err)
	u := f.usage()
	assert.Equal(t, 3, u.Runs)
	assert.Zero(t, u.InFlightRuns)
	assert.NotEqual(t, calls[0].RunID, calls[1].RunID)
}

// TestIntegration_AContendedBudgetIsRateLimitedNeverInternal: a caller that
// cannot get the account's budget -- its lock held by another charge, or no
// budget connection free -- for longer than the budget waits is refused
// RATE_LIMITED (SANDBOX_BUDGET_CONTENDED, retry after a second) before
// anything runs, and is never an internal error.
func TestIntegration_AContendedBudgetIsRateLimitedNeverInternal(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	f.svc.wait = 300 * time.Millisecond

	// Another connection holds the account's budget lock, as a charge does.
	holder, err := testDB.Pool().Acquire(f.ctx)
	require.NoError(t, err)
	defer holder.Release()
	lockTx, err := holder.Begin(f.ctx)
	require.NoError(t, err)
	_, err = lockTx.Exec(f.ctx, `SELECT pg_advisory_xact_lock((('x' || substr(md5($1), 1, 16))::bit(64))::bigint)`, budgetLockKey(f.account))
	require.NoError(t, err)

	runsBefore := countRuns(t)
	_, err = f.executeIn(f.request(digest, testInput()))
	requireReason(t, err, errs.CodeRateLimited, BudgetContended)
	e, _ := errs.As(err)
	require.NotNil(t, e.RetryAfter)
	assert.Equal(t, time.Second, *e.RetryAfter)
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error { return f.svc.Admit(ctx, tx, f.account, 1, testLimits()) })
	requireReason(t, err, errs.CodeRateLimited, BudgetContended)
	assert.Zero(t, f.engine.runs.Load(), "a contended run never starts")
	assert.Equal(t, runsBefore, countRuns(t))

	require.NoError(t, lockTx.Rollback(f.ctx))
	_, err = f.executeIn(f.request(digest, testInput()))
	require.NoError(t, err, "the same run, once the budget is free")

	// No budget connection free within the wait: the same refusal.
	busy := NewService(f.clk, f.auth, f.engine, f.aw, exhaustedConns{})
	busy.wait = 200 * time.Millisecond
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := busy.Execute(ctx, tx, f.request(digest, testInput()))
		return err
	})
	requireReason(t, err, errs.CodeRateLimited, BudgetContended)
}

// exhaustedConns is a budget pool with no free connection.
type exhaustedConns struct{}

func (exhaustedConns) Acquire(ctx context.Context) (*pgxpool.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// TestIntegration_AChargeTheWindowNoLongerHoldsIsContendedNeverInternal: a
// run whose window filled up under it while it ran -- here a charge written
// around the service, as a skewed clock or an overlong run could leave it --
// is refused by the database at its charge (SX003). That is a coded refusal
// (SANDBOX_BUDGET_CONTENDED), the budget is not overspent, and nothing of the
// run is recorded.
func TestIntegration_AChargeTheWindowNoLongerHoldsIsContendedNeverInternal(t *testing.T) {
	f := newSvcFixture(t)
	one := smallBudget()
	one.MaxRunsPerWindow, one.MaxRunsPerCall = 1, 1
	f.withBudget(one)
	hold := newHoldingEngine(f.engine)
	f.withEngine(hold)
	digest := f.artifact("momentum")
	done := make(chan error, 1)
	go func() {
		_, err := f.executeIn(f.request(digest, held(testInput())))
		done <- err
	}()
	<-hold.started

	// Around the service: a run and its charge that fill the one-run window.
	in := sha256.Sum256([]byte("an input " + uuid.NewString()))
	empty := sha256.Sum256(nil)
	runDigest := sha256.Sum256(append(append([]byte{}, digest...), in[:]...))
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits,
			termination_reason, termination_detail, exit_code, artifact_verified, wall_ms, output, output_bytes, stderr_bytes,
			stderr_digest, peak_memory_bytes, output_digest, run_digest, proposals_count, accepted_count, violations_count,
			manifest_hash, strategy_ref, actor_type, actor_id, started_at)
			VALUES ($1,$2,$3,'v1','`+"0000000000000000000000000000000000000000000000000000000000000000"+`','{"wall_time_ms":2000}','TRAP','',1,true,7,
			''::bytea,0,0,$3,0,$4,$5,0,0,0,$3,'s@1','SYSTEM','x',now())`, id, digest, in[:], empty[:], runDigest[:]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
			window_ms, max_runs, max_cpu_ms, max_wall_ms) VALUES ($1,$2,$3,'SIMULATION',2000,7,$4,'x',3600000,100,600000,600000)`,
			uuid.New(), id, f.account, f.clk.Now())
		return err
	}))
	runsBefore := countRuns(t)

	close(hold.release)
	err := <-done
	requireReason(t, err, errs.CodeRateLimited, BudgetContended)
	assert.Equal(t, runsBefore, countRuns(t), "nothing of the refused run is recorded")
	u := f.usage()
	assert.Equal(t, 1, u.Runs, "the window holds the one charge it has room for")
	assert.Zero(t, u.InFlightRuns)
}

// TestIntegration_TheBudgetNeverReachesForTheCallersPool is F-27's property:
// the caller's whole pool is the one connection its transaction holds, and a
// run is still reserved, run and charged -- on the budget's own connections.
func TestIntegration_TheBudgetNeverReachesForTheCallersPool(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	single, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "sandboxrt-itest-single", MaxConns: 1})
	require.NoError(t, err)
	defer single.Close()
	err = single.InTx(ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, func(ctx context.Context, tx pgx.Tx) error {
		require.NoError(t, f.svc.Admit(ctx, tx, f.account, 1, testLimits()))
		e, err := f.svc.Execute(ctx, tx, f.request(digest, testInput()))
		if err == nil {
			assert.Equal(t, TerminationOK, e.Result.Termination)
		}
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, 1, f.usage().Runs)
}
