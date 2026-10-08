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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	testDB = d
	return d.Close, nil
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
	svc    *Service
	engine *countingEngine
	fx     fixtureSet
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
	return &svcFixture{t: t, ctx: context.Background(), clk: clk, fx: fx, engine: engine,
		svc: NewService(clk, auth, engine, audit.NewWriterWithBuildVersion("itest"))}
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
		e, err = f.svc.Execute(ctx, tx, ExecuteRequest{ArtifactDigest: digest, Input: in, Limits: testLimits(), Manifest: testManifest(),
			StrategyRef: "0193b2e0-0000-7000-8000-000000000123@1", Actor: operator})
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
				e, err := f.svc.Execute(ctx, tx, ExecuteRequest{ArtifactDigest: digest, Input: testInput(), Limits: testLimits(),
					Manifest: testManifest(), StrategyRef: "0193b2e0-0000-7000-8000-000000000123@1", Actor: operator})
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
				return rollback
			})
			require.ErrorIs(t, err, rollback)
			assert.Equal(t, before, f.engine.runs.Load(), "the module was never started")
			_, err = f.svc.Run(f.ctx, testDB, runID)
			assert.Equal(t, errs.CodeNotFound, errs.CodeOf(err))
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

	err = exec(`DELETE FROM sandbox_runs WHERE id = $1`, e.RunID)
	require.Error(t, err)
	assert.Equal(t, "42501", db.SQLState(err))
	_, err = owner.Exec(f.ctx, `DELETE FROM sandbox_runs WHERE id = $1`, e.RunID)
	require.Error(t, err)
	assert.Equal(t, "P0001", db.SQLState(err), "a run is append-only even for the owner")
}
