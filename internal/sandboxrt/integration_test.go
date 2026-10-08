//go:build integration

package sandboxrt

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/audit"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/db/migrate"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/security"
)

var (
	testAppURL     = os.Getenv("CP_TEST_DATABASE_URL")
	testMigrateURL = os.Getenv("CP_TEST_MIGRATE_DATABASE_URL")
	testDB         *db.DB
	// testBudget is the budget's own pool, as the binaries open it.
	testBudget *pgxpool.Pool
)

func setupDatabase() (func(), error) {
	if testAppURL == "" || testMigrateURL == "" {
		return func() {}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := migrate.Up(ctx, testMigrateURL); err != nil {
		return nil, err
	}
	d, err := db.Open(ctx, db.Config{URL: testAppURL, AppName: "sandboxrt-itest", MaxConns: 20})
	if err != nil {
		return nil, err
	}
	budget, err := OpenBudgetPool(ctx, d, MaxBudgetConns)
	if err != nil {
		d.Close()
		return nil, err
	}
	testDB, testBudget = d, budget
	return func() { budget.Close(); d.Close() }, nil
}

func requireDB(t *testing.T) {
	t.Helper()
	if testDB == nil {
		t.Skip("set CP_TEST_DATABASE_URL and CP_TEST_MIGRATE_DATABASE_URL")
	}
}

// countingEngine counts how many times a module was actually started.
type countingEngine struct {
	inner Engine
	runs  atomic.Int64
}

func (c *countingEngine) Run(ctx context.Context, req RunRequest) (RawRun, error) {
	c.runs.Add(1)
	return c.inner.Run(ctx, req)
}

type svcFixture struct {
	t      *testing.T
	ctx    context.Context
	clk    *clock.Fake
	auth   *econ.Authorizer
	aw     audit.Writer
	svc    *Service
	engine *countingEngine
	fx     fixtureSet
	// account is the account every run of the fixture is charged to.
	account accounts.AccountID
}

var operator = econ.Actor{Type: security.ActorOperator, ID: "0193b2e0-0000-7000-8000-00000000aaaa"}

func newSvcFixture(t *testing.T) *svcFixture {
	t.Helper()
	requireDB(t)
	fx := requireGoModules(t)
	clk := clock.NewFake(time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC))
	checker, err := gates.NewChecker("LOCAL", func(gates.Capability) bool { return true }, clk)
	require.NoError(t, err)
	auth := econ.NewAuthorizer(checker, killswitch.NewChecker(killswitch.Policy{}))
	engine := &countingEngine{inner: fx.runner}
	aw := audit.NewWriterWithBuildVersion("itest")
	f := &svcFixture{t: t, ctx: context.Background(), clk: clk, auth: auth, aw: aw, fx: fx, engine: engine,
		svc: NewService(clk, auth, engine, aw, testBudget)}
	f.account = f.newAccount()
	return f
}

// withEngine rebuilds the fixture's service around another engine, keeping
// its budget.
func (f *svcFixture) withEngine(e Engine) {
	b := f.svc.Budget()
	f.svc = NewService(f.clk, f.auth, e, f.aw, testBudget)
	f.withBudget(b)
}

// newAccount creates a customer account to charge runs to.
func (f *svcFixture) newAccount() accounts.AccountID {
	f.t.Helper()
	repo := accounts.NewRepository()
	u, err := repo.CreateUser(f.ctx, testDB, "sandboxrt-itest", uuid.NewString(), nil)
	require.NoError(f.t, err)
	a, err := repo.CreateAccount(f.ctx, testDB, u.ID, accounts.KindCustomer)
	require.NoError(f.t, err)
	return a.ID
}

func (f *svcFixture) request(digest []byte, in Input) ExecuteRequest {
	return ExecuteRequest{ArtifactDigest: digest, Input: in, Limits: testLimits(), Manifest: testManifest(),
		StrategyRef: "0193b2e0-0000-7000-8000-000000000123@1", Actor: operator, AccountID: f.account, Purpose: PurposeSimulation}
}

func (f *svcFixture) inTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return testDB.InTx(f.ctx, db.TxOptions{Isolation: pgx.ReadCommitted}, fn)
}

func (f *svcFixture) artifact(name string) []byte {
	f.t.Helper()
	b := f.fx.builds[name]
	require.NoError(f.t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.RecordArtifact(ctx, tx, b, operator)
		return err
	}))
	return b.ArtifactDigest
}

func (f *svcFixture) execute(digest []byte, in Input) (Execution, error) {
	var e Execution
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		e, err = f.svc.Execute(ctx, tx, f.request(digest, in))
		return err
	})
	return e, err
}

func TestIntegration_ExecuteRecordsTheRunItsViolationsAndAnAudit(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("signing_attempt")
	in := testInput()
	in.Config["mode"] = "withdraw_kind"
	e, err := f.execute(digest, in)
	require.NoError(t, err)
	assert.Equal(t, TerminationOK, e.Result.Termination)
	assert.Empty(t, e.Accepted)
	require.Len(t, e.Violations, 1)
	assert.Equal(t, ViolationKindNotProposable, e.Violations[0].Code)

	rec, err := f.svc.Run(f.ctx, testDB, e.RunID)
	require.NoError(t, err)
	assert.Equal(t, TerminationOK, rec.Termination)
	assert.Equal(t, 1, rec.ViolationsCount)
	assert.True(t, rec.ArtifactVerified)
	assert.Equal(t, e.Result.RunDigest, rec.RunDigest)
	stored, err := f.svc.RunViolations(f.ctx, testDB, e.RunID)
	require.NoError(t, err)
	assert.Equal(t, e.Violations, stored)

	var auditRows int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM audit_events WHERE resource_type = 'sandbox_run' AND resource_id = $1
		AND action = 'sandbox.run.ok'`, e.RunID.String()).Scan(&auditRows))
	assert.Equal(t, 1, auditRows)

	// The well-behaved strategy's proposal survives the manifest.
	e, err = f.execute(f.artifact("momentum"), testInput())
	require.NoError(t, err)
	require.Len(t, e.Accepted, 1)
	assert.Equal(t, insA, e.Accepted[0].InstrumentID)
}

// TestIntegration_KillSwitchRefusesToRun: while CAPABILITY_DISABLE for
// USER_CODE_SANDBOX or the global kill is active -- as the caller's own
// transaction sees it -- the module is never started; the refusal is recorded
// as a KILLED_BY_SWITCH run, charged one run and no CPU, in a transaction of
// its own.
func TestIntegration_KillSwitchRefusesToRun(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	ctl, err := killswitch.NewController(f.clk, discardAudit{}, noApprovals{})
	require.NoError(t, err)
	op := security.Principal{SubjectID: "0193b2e0-0000-7000-8000-00000000bbbb", ActorType: security.ActorOperator,
		Roles: []security.Role{security.RoleOperations}, AuthTime: f.clk.Now(), AMR: []string{"mfa"}}

	for _, sw := range []struct {
		kind  killswitch.Kind
		scope string
	}{{killswitch.CapabilityDisable, string(gates.UserCodeSandbox)}, {killswitch.GlobalNewRiskKill, "*"}} {
		t.Run(string(sw.kind), func(t *testing.T) {
			before := f.engine.runs.Load()
			rollback := errors.New("rollback: the switch must not outlive the test")
			var runID RunID
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				if _, err := ctl.Activate(security.WithPrincipal(ctx, op), tx, sw.kind, sw.scope, "sandbox escape under investigation"); err != nil {
					return err
				}
				e, err := f.svc.Execute(ctx, tx, f.request(digest, testInput()))
				if err != nil {
					return err
				}
				runID = e.RunID
				assert.Equal(t, TerminationKilledBySwitch, e.Result.Termination)
				assert.Empty(t, e.Accepted)
				rec, err := f.svc.Run(ctx, tx, e.RunID)
				if err != nil {
					return err
				}
				assert.Equal(t, TerminationKilledBySwitch, rec.Termination, "the refusal is recorded")
				assert.Equal(t, int64(0), rec.WallMS)
				var cpu int64
				if err := tx.QueryRow(ctx, `SELECT cpu_ms FROM sandbox_usage WHERE run_id = $1`, e.RunID).Scan(&cpu); err != nil {
					return err
				}
				assert.Zero(t, cpu, "a run that never started is charged no CPU")
				return rollback
			})
			require.ErrorIs(t, err, rollback)
			assert.Equal(t, before, f.engine.runs.Load(), "the module was never started")
			// The refusal is the run's own record, committed with its charge
			// in a transaction of its own: the caller's rollback -- which takes
			// the switch back with it -- leaves the record, as one run of the
			// window that cost no CPU.
			rec, err := f.svc.Run(f.ctx, testDB, runID)
			require.NoError(t, err)
			assert.Equal(t, TerminationKilledBySwitch, rec.Termination)
		})
	}
	// With the switch released (rolled back) the same request runs.
	e, err := f.execute(digest, testInput())
	require.NoError(t, err)
	assert.Equal(t, TerminationOK, e.Result.Termination)
}

type discardAudit struct{}

func (discardAudit) Append(context.Context, pgx.Tx, killswitch.AuditEvent) error { return nil }

type noApprovals struct{}

func (noApprovals) VerifyApproved(context.Context, db.Querier, string, string, string) (killswitch.Approval, error) {
	return killswitch.Approval{}, errs.New(errs.CodeForbidden, "no approvals in this test")
}

func TestIntegration_ARecordedRunIsReproducible(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	e, err := f.execute(digest, testInput())
	require.NoError(t, err)
	var output, runDigest []byte
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT output, run_digest FROM sandbox_runs WHERE id = $1`, e.RunID).Scan(&output, &runDigest))
	// An auditor re-runs the stored artifact on the same input.
	var wasm []byte
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT wasm FROM strategy_artifacts WHERE artifact_digest = $1`, digest).Scan(&wasm))
	raw, err := f.fx.runner.Run(f.ctx, RunRequest{Artifact: Artifact{Digest: digest, Wasm: wasm}, Input: testInput(), Limits: testLimits()})
	require.NoError(t, err)
	assert.Equal(t, output, raw.Stdout)
	assert.Equal(t, runDigest, RunDigest(digest, raw.InputDigest, raw.Stdout))
}

func TestIntegration_RecordArtifactIsIdempotentUnderConcurrency(t *testing.T) {
	f := newSvcFixture(t)
	b := f.fx.builds["sleeper"]
	const n = 8
	var wg sync.WaitGroup
	existing := make(chan bool, n)
	errsCh := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
				_, ex, err := f.svc.RecordArtifact(ctx, tx, b, operator)
				existing <- ex
				return err
			})
			errsCh <- err
		}()
	}
	wg.Wait()
	close(existing)
	close(errsCh)
	for err := range errsCh {
		require.NoError(t, err)
	}
	var rows int
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM strategy_artifacts WHERE artifact_digest = $1`, b.ArtifactDigest).Scan(&rows))
	assert.Equal(t, 1, rows)

	// A second source that builds to the same bytes is recorded as such.
	other := b
	otherSum := sha256.Sum256([]byte("another tree"))
	other.SourceDigest = otherSum[:]
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, ex, err := f.svc.RecordArtifact(ctx, tx, other, operator)
		assert.True(t, ex)
		return err
	}))
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT count(*) FROM strategy_artifact_sources WHERE artifact_digest = $1`, b.ArtifactDigest).Scan(&rows))
	assert.Equal(t, 2, rows)

	// A failed or tampered build is never an artifact.
	tampered := b
	tampered.Wasm = append([]byte(nil), b.Wasm...)
	tampered.Wasm[100] ^= 1
	err := f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := f.svc.RecordArtifact(ctx, tx, tampered, operator)
		return err
	})
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
}

// TestIntegration_TheDatabaseRefusesATamperedArtifactOrRun goes around the
// service with raw SQL and shows the database holds the invariants itself.
func TestIntegration_TheDatabaseRefusesATamperedArtifactOrRun(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	sum := sha256.Sum256([]byte("not the wasm"))
	hash32 := sum[:]
	exec := func(sql string, args ...any) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}
	insertArtifact := `INSERT INTO strategy_artifacts (id, artifact_digest, source_digest, wasm, wasm_size, toolchain_version, sdk_version,
		build_log_digest, scan_report, scan_codes, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,'go1.27.1','nodalsdk/v1@sha256:` + "0000000000000000000000000000000000000000000000000000000000000000" + `',$3,'{}',$6,'SYSTEM','x')`

	err := exec(insertArtifact, uuid.New(), hash32, hash32, []byte("\x00asm\x01\x00\x00\x00wasm"), 12, []string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strategy_artifacts_digest_is_the_wasm")

	wasm := []byte("\x00asm\x01\x00\x00\x00ok")
	wsum := sha256.Sum256(wasm)
	err = exec(insertArtifact, uuid.New(), wsum[:], hash32, wasm, len(wasm), []string{"IMPORT_FORBIDDEN"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "strategy_artifacts_scan_codes_check", "an artifact exists only for a clean scan")

	err = exec(`UPDATE strategy_artifacts SET toolchain_version = 'go9' WHERE artifact_digest = $1`, digest)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err), "the application role cannot update an artifact at all")
	owner, err := pgx.Connect(f.ctx, testMigrateURL)
	require.NoError(t, err)
	defer func() { _ = owner.Close(f.ctx) }()
	_, err = owner.Exec(f.ctx, `UPDATE strategy_artifacts SET toolchain_version = 'go9' WHERE artifact_digest = $1`, digest)
	require.Error(t, err)
	assert.Equal(t, "P0001", db.SQLState(err), "and neither can the owner: the row is immutable")

	// A run whose digest does not follow from its artifact, input and output.
	e, err := f.execute(digest, testInput())
	require.NoError(t, err)
	insertRun := `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits, termination_reason,
		termination_detail, exit_code, artifact_verified, wall_ms, output, output_bytes, stderr_bytes, stderr_digest, peak_memory_bytes,
		output_digest, run_digest, proposals_count, accepted_count, violations_count, manifest_hash, strategy_ref, actor_type, actor_id,
		started_at) VALUES ($1,$2,$3,'v1','` + "0000000000000000000000000000000000000000000000000000000000000000" + `','{}',$4,'',0,true,1,$5,$6,0,$3,0,$7,$8,$9,0,$10,$3,'s@1','SYSTEM','x',now())`
	out := []byte("{}\n")
	outSum := sha256.Sum256(out)
	good := sha256.Sum256(append(append(append([]byte{}, digest...), hash32...), out...))
	err = exec(insertRun, uuid.New(), digest, hash32, "OK", out, len(out), outSum[:], hash32, 0, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox_runs_run_digest_is_reproducible")
	err = exec(insertRun, uuid.New(), digest, hash32, "TRAP", out, len(out), outSum[:], good[:], 2, 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox_runs_only_ok_runs_propose")
	err = exec(insertRun, uuid.New(), digest, hash32, "OK", out, len(out), outSum[:], good[:], 0, 3)
	require.Error(t, err)
	assert.Equal(t, "SX001", db.SQLState(err), "a run cannot declare violations it did not record")
	err = exec(insertRun, uuid.New(), digest, hash32, "OK", out, len(out), outSum[:], good[:], 0, 0)
	require.Error(t, err)
	assert.Equal(t, "SX002", db.SQLState(err), "a run nobody was charged for is refused at COMMIT")

	err = exec(`DELETE FROM sandbox_runs WHERE id = $1`, e.RunID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `DELETE FROM sandbox_runs WHERE id = $1`, e.RunID)
	require.Error(t, err)
	assert.Equal(t, "P0001", db.SQLState(err), "a run is append-only even for the owner")
}

// smallBudget allows three runs and 6 s of granted CPU per account per hour,
// two runs per call.
func smallBudget() Budget {
	return Budget{Version: "itest-budget/v1", Window: time.Hour, MaxRunsPerWindow: 3, MaxCPUMSPerWindow: 6000,
		MaxWallMSPerWindow: 60_000, MaxRunsPerCall: 2, MaxCPUMSPerCall: 4000}
}

func (f *svcFixture) withBudget(b Budget) {
	f.t.Helper()
	svc, err := f.svc.WithBudget(b)
	require.NoError(f.t, err)
	f.svc = svc
}

// TestIntegration_ARunBeyondTheAccountsWindowIsRefusedWithAStableReason is
// review-2 F13: an account's runs are charged against its window; the run the
// window has no room for is refused before anything starts, with a stable
// reason and a retry-after, and nothing of it is recorded. The window rolls.
func TestIntegration_ARunBeyondTheAccountsWindowIsRefusedWithAStableReason(t *testing.T) {
	f := newSvcFixture(t)
	f.withBudget(smallBudget())
	digest := f.artifact("momentum")
	for i := 0; i < 3; i++ {
		_, err := f.execute(digest, testInput())
		require.NoError(t, err, "run %d", i+1)
		f.clk.Advance(time.Minute)
	}
	u, err := f.svc.UsageOf(f.ctx, testDB, f.account)
	require.NoError(t, err)
	assert.Equal(t, 3, u.Runs)
	assert.Equal(t, int64(3*testLimits().WallTimeMS), u.CPUMS, "each run is charged the wall time it was granted")
	started := f.engine.runs.Load()
	runsBefore := countRuns(t)
	_, err = f.execute(digest, testInput())
	require.Error(t, err)
	assert.Equal(t, errs.CodeRateLimited, errs.CodeOf(err))
	e, _ := errs.As(err)
	assert.Equal(t, BudgetWindowRuns, e.Fields["reason"])
	require.NotNil(t, e.RetryAfter)
	assert.Equal(t, 57*time.Minute, *e.RetryAfter, "when the oldest charge leaves the window")
	assert.Equal(t, started, f.engine.runs.Load(), "a refused run never starts")
	assert.Equal(t, runsBefore, countRuns(t), "and nothing of it is recorded")

	// Another account has its own window.
	other := f.newAccount()
	req := f.request(digest, testInput())
	req.AccountID = other
	require.NoError(t, f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Execute(ctx, tx, req)
		return err
	}))
	// The window rolls: an hour after the first run, one run fits again.
	f.clk.Advance(58 * time.Minute)
	_, err = f.execute(digest, testInput())
	require.NoError(t, err)
}

func countRuns(t *testing.T) int {
	t.Helper()
	var n int
	require.NoError(t, testDB.QueryRow(context.Background(), `SELECT count(*) FROM sandbox_runs`).Scan(&n))
	return n
}

// TestIntegration_ACallBeyondItsBoundsIsRefusedBeforeAnythingRuns: a call is
// admitted whole or not at all -- more runs, or more granted CPU, than one
// call may have, or more than the account's window has left, is refused
// before the first run starts.
func TestIntegration_ACallBeyondItsBoundsIsRefusedBeforeAnythingRuns(t *testing.T) {
	f := newSvcFixture(t)
	f.withBudget(smallBudget())
	digest := f.artifact("momentum")
	admit := func(runs int, limits Limits) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			return f.svc.Admit(ctx, tx, f.account, runs, limits)
		})
	}
	reason := func(err error) any {
		e, ok := errs.As(err)
		require.True(t, ok, "%v", err)
		assert.Equal(t, errs.CodeRateLimited, e.Code)
		return e.Fields["reason"]
	}
	assert.Equal(t, BudgetCallRuns, reason(admit(3, testLimits())))
	slow := testLimits()
	slow.WallTimeMS = 2500
	assert.Equal(t, BudgetCallCPU, reason(admit(2, slow)), "2 x 2500 ms is more CPU than a call may be granted")
	require.NoError(t, admit(2, testLimits()))

	_, err := f.execute(digest, testInput())
	require.NoError(t, err)
	_, err = f.execute(digest, testInput())
	require.NoError(t, err)
	assert.Equal(t, BudgetWindowRuns, reason(admit(2, testLimits())), "two more do not fit in what the window has left")
	require.NoError(t, admit(1, testLimits()))
	assert.Equal(t, int64(2), f.engine.runs.Load(), "admission starts nothing")

	big := testLimits()
	big.WallTimeMS = 5000
	req := f.request(digest, testInput())
	req.Limits = big
	err = f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.svc.Execute(ctx, tx, req)
		return err
	})
	assert.Equal(t, BudgetCallCPU, reason(err), "one run granted more than a call may have is refused")
}

// TestIntegration_ConcurrentRunsCannotOverspendTheBudget: N concurrent
// callers, each in its own transaction, run code charged to one account whose
// window has room for three. Each run is admitted against the account's
// charges and every reservation not yet charged, in a short budget
// transaction, and no lock is held while any of them runs -- so exactly the
// window's three run and are charged, and every other caller is refused
// RATE_LIMITED for the window, never INTERNAL, however loaded the machine is.
// A caller refused only because the budget was contended (it waited longer
// than the budget allows) retries, as a client would after its Retry-After;
// that it can happen at all is the price of never waiting long.
func TestIntegration_ConcurrentRunsCannotOverspendTheBudget(t *testing.T) {
	f := newSvcFixture(t)
	f.withBudget(smallBudget())
	digest := f.artifact("momentum")
	const n = 8
	var wg sync.WaitGroup
	var ok, windowRefused, contendedRetries atomic.Int64
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; ; attempt++ {
				_, err := f.execute(digest, testInput())
				e, _ := errs.As(err)
				switch {
				case err == nil:
					ok.Add(1)
				case errs.CodeOf(err) == errs.CodeRateLimited && e.Fields["reason"] == BudgetContended && attempt < 20:
					contendedRetries.Add(1)
					continue
				case errs.CodeOf(err) == errs.CodeRateLimited && e.Fields["reason"] == BudgetWindowRuns:
					windowRefused.Add(1)
				default:
					t.Errorf("a concurrent run must run or be refused for the window, never this: %v", err)
				}
				return
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, int64(3), ok.Load(), "exactly the window's three runs")
	assert.Equal(t, int64(n-3), windowRefused.Load())
	assert.Equal(t, int64(3), f.engine.runs.Load(), "a refused run never starts")
	u, err := f.svc.UsageOf(f.ctx, testDB, f.account)
	require.NoError(t, err)
	assert.Equal(t, 3, u.Runs)
	assert.Zero(t, u.InFlightRuns, "every reservation is charged or gone")
	t.Logf("contended refusals retried: %d", contendedRetries.Load())
}

// TestIntegration_TheDatabaseRefusesAChargeThatIsNotTheRunsOrBeyondItsBudget
// goes around the service: a charge must be the run's own wall time and
// granted limit, and must fit the budget it states.
func TestIntegration_TheDatabaseRefusesAChargeThatIsNotTheRunsOrBeyondItsBudget(t *testing.T) {
	f := newSvcFixture(t)
	digest := f.artifact("momentum")
	e, err := f.execute(digest, testInput())
	require.NoError(t, err)
	var wall int64
	require.NoError(t, testDB.QueryRow(f.ctx, `SELECT wall_ms FROM sandbox_runs WHERE id = $1`, e.RunID).Scan(&wall))
	charge := func(run RunID, cpu, wallMS int64, maxRuns int) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
				window_ms, max_runs, max_cpu_ms, max_wall_ms) VALUES ($1,$2,$3,'SIMULATION',$4,$5,$6,'x',3600000,$7,600000,600000)`,
				uuid.New(), run, f.account, cpu, wallMS, f.clk.Now(), maxRuns)
			return err
		})
	}
	// A second charge for the same run is refused by its uniqueness ...
	err = charge(e.RunID, testLimits().WallTimeMS, wall, 100)
	require.Error(t, err)
	assert.True(t, db.IsUniqueViolation(err))
	// ... and a run recorded around the service needs a charge that is its own.
	insertRun := `INSERT INTO sandbox_runs (id, artifact_digest, input_digest, limits_version, limits_hash, limits, termination_reason,
		termination_detail, exit_code, artifact_verified, wall_ms, output, output_bytes, stderr_bytes, stderr_digest, peak_memory_bytes,
		output_digest, run_digest, proposals_count, accepted_count, violations_count, manifest_hash, strategy_ref, actor_type, actor_id,
		started_at) VALUES ($1,$2,$3,'v1','` + "0000000000000000000000000000000000000000000000000000000000000000" + `','{"wall_time_ms":2000}','TRAP','',1,true,7,
		''::bytea,0,0,$3,0,$4,$5,0,0,0,$3,'s@1','SYSTEM','x',now())`
	in := sha256.Sum256([]byte("an input"))
	empty := sha256.Sum256(nil)
	runDigest := sha256.Sum256(append(append([]byte{}, digest...), in[:]...))
	try := func(cpu, wallMS int64, maxRuns int) error {
		return f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			id := uuid.New()
			if _, err := tx.Exec(ctx, insertRun, id, digest, in[:], empty[:], runDigest[:]); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO sandbox_usage (id, run_id, account_id, purpose, cpu_ms, wall_ms, charged_at, budget_version,
				window_ms, max_runs, max_cpu_ms, max_wall_ms) VALUES ($1,$2,$3,'SIMULATION',$4,$5,$6,'x',3600000,$7,600000,600000)`,
				uuid.New(), id, f.account, cpu, wallMS, f.clk.Now(), maxRuns)
			return err
		})
	}
	err = try(0, 7, 100)
	require.Error(t, err)
	assert.Equal(t, "SX003", db.SQLState(err), "a run that started is charged the limit it was granted")
	err = try(2000, 1, 100)
	require.Error(t, err)
	assert.Equal(t, "SX003", db.SQLState(err), "and the wall time it took")
	err = try(2000, 7, 1)
	require.Error(t, err)
	assert.Equal(t, "SX003", db.SQLState(err), "a charge beyond the budget it states is refused")
	require.NoError(t, try(2000, 7, 100), "the run's own charge, within its budget")
}
