//go:build integration

package advancedjobs

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

// quietSnapshots builds a scheduled run's input in which the strategy sees
// too little history to propose anything: the follow runs the sandbox and
// records its run, and no intent reaches execution.
type quietSnapshots struct{ instrument string }

func (s quietSnapshots) Snapshot(_ context.Context, _ db.Querier, _ marketplace.Install, _ marketplace.Version, asOf time.Time) (sandboxrt.Input, error) {
	return sandboxrt.Input{
		AsOf: asOf,
		Instruments: []sandboxrt.InputInstrument{{InstrumentID: s.instrument, Kind: "SPOT_PAIR", BaseDecimals: 6, QuoteDecimals: 6,
			Mark: "150.25", Closes: []string{"150.00"}}},
		Portfolio: sandboxrt.InputPortfolio{Cash: "10000.00"},
	}, nil
}

// refusingSections and refusingExecutor stand where no proposal reaches:
// a quiet run never asks either, so a call is a test failure.
type refusingSections struct{}

func (refusingSections) RiskContext(context.Context, db.Querier, econ.Intent, sandboxrt.AcceptedProposal) (marketplace.RiskContext, error) {
	return marketplace.RiskContext{}, errors.New("advancedjobs itest: no proposal was expected")
}

type refusingExecutor struct{}

func (refusingExecutor) ExecutePaper(context.Context, pgx.Tx, marketplace.PaperExecution) (marketplace.ExecutionReceipt, error) {
	return marketplace.ExecutionReceipt{}, errors.New("advancedjobs itest: no execution was expected")
}

// strategyToolchain builds the sandbox builder and runner the marketplace
// validates and runs strategies with, or skips: building a strategy needs the
// go command.
func strategyToolchain(t *testing.T) (*sandboxrt.Builder, *sandboxrt.Runner) {
	t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skipf("building a strategy needs the go command: %v", err)
	}
	goBin, err = filepath.Abs(goBin)
	require.NoError(t, err)
	dir := t.TempDir()
	builder, err := sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: filepath.Join(dir, "gocache"),
		TempDir: dir, Timeout: 5 * time.Minute})
	if err != nil {
		t.Skipf("the go command found is unusable for strategy builds: %v", err)
	}
	runner, err := sandboxrt.NewRunner(context.Background(), sandboxrt.RunnerConfig{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = runner.Close(context.Background()) })
	return builder, runner
}

// TestIntegration_ScheduledFollowsRunEachDueInstallOnce: a due PAPER install
// of an approved strategy is followed by the job at the run's instant, once,
// and is not due again within the manifest's interval. Built as every
// deployment of this build builds the marketplace -- without a snapshot source
// -- the same install is deferred and nothing is written.
func TestIntegration_ScheduledFollowsRunEachDueInstallOnce(t *testing.T) {
	w := newWorld(t)
	builder, runner := strategyToolchain(t)
	usdc := w.simAsset("AJMQ", 6, assets.RiskSettlement)
	base := w.simAsset("AJMA", 6, assets.RiskMajor)
	var ins instruments.Instrument
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		ins, err = w.instr.CreateSpotPair(ctx, tx, instruments.SpotPairSpec{Base: base.ID, Quote: usdc.ID, Settlement: usdc.ID,
			CanonicalName: base.Symbol + "/" + usdc.Symbol, RiskClass: assets.RiskMajor, Status: assets.StatusActive,
			ActiveFrom: w.clk.Now().Add(-time.Hour)})
		return err
	}))
	build := func(snaps marketplace.SnapshotSource) *marketplace.Service {
		svc, err := marketplace.NewService(marketplace.Config{
			Clock: w.clk, Intents: w.intents, Effects: w.effects, Auth: w.authz, Risk: risk.NewStore(),
			RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.router,
			Sandbox: sandboxrt.NewService(w.clk, w.authz, runner, w.audit), Builder: builder,
			Sections: refusingSections{}, Executor: refusingExecutor{}, Audit: w.audit, Snapshots: snaps,
		})
		require.NoError(t, err)
		return svc
	}
	deployed := build(nil)
	withSnapshots := build(quietSnapshots{instrument: ins.ID.String()})

	creator, follower := w.customer("strategy-creator"), w.customer("strategy-follower")
	creatorActor := econ.Actor{Type: security.ActorUser, ID: creator.user.String()}
	operator := econ.Actor{Type: security.ActorOperator, ID: uuid.NewString()}
	source := sandboxrt.Source{}
	dir := filepath.Join("..", "marketplace", "testdata", "strategies", "momentum")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- the marketplace's own test strategy
		require.NoError(t, err)
		source[e.Name()] = b
	}
	manifest := sandboxrt.Manifest{AllowedIntentKinds: []econ.IntentKind{econ.KindAcquireExposure, econ.KindReduceExposure},
		AllowedInstrumentIDs: []string{ins.ID.String()}, MaxNotionalPerIntent: "1000.00", MaxIntentsPerRun: 2,
		MinIntervalSeconds: 3600}.Canonicalize()
	var v marketplace.Version
	step := func(fn func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error)) {
		t.Helper()
		require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			v, err = fn(ctx, tx)
			return err
		}))
	}
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		_, v, err := deployed.CreateListing(ctx, tx, marketplace.ListingRequest{CreatorAccountID: creator.account, Creator: creatorActor,
			Title: "Advanced worker momentum " + uuid.NewString()[:8], Version: marketplace.VersionRequest{
				Description: "the marketplace's momentum test strategy", RiskClass: marketplace.RiskModerate,
				SupportedInstrumentIDs: []string{ins.ID.String()}, Manifest: manifest, Source: source, SourceCommit: "test-momentum",
				BacktestEvidenceRef: "evidence://backtests/momentum", DataDependencies: []string{"closes", "marks"},
				Fees:   marketplace.FeeModel{PerformanceFeeBPS: 2000, ManagementFeeBPS: 100, HighWaterMark: true},
				Config: map[string]string{"order_units": "1000000"},
			}})
		return v, err
	})
	vid := v.ID
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return deployed.Submit(ctx, tx, vid, creatorActor)
	})
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		out, _, err := deployed.Validate(ctx, tx, vid, econ.SystemActor("marketplace.validator"))
		return out, err
	})
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return deployed.StartReview(ctx, tx, vid, operator)
	})
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return deployed.Approve(ctx, tx, vid, operator, "MEETS_POLICY", "advanced worker integration test")
	})
	require.Equal(t, marketplace.StatusApproved, v.Status)
	var in marketplace.Install
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, _, err = deployed.InstallVersion(ctx, tx, marketplace.InstallRequest{VersionID: vid, AccountID: follower.account,
			Mode: gates.ModePaper, Actor: econ.Actor{Type: security.ActorUser, ID: follower.user.String()}})
		return err
	}))
	runs := func() int {
		return w.count(`SELECT count(*) FROM marketplace_follow_runs WHERE install_id = $1`, in.ID)
	}

	rep := run(t, w.ctx, NewMarketplaceFollows(testDB, deployed, w.clk, Settings{}))
	assert.False(t, failedUnit(rep, in.ID.String()), "%+v", rep.Failures)
	assert.GreaterOrEqual(t, rep.Deferred, 1, "no snapshot source in this build: the follow is refused UNSUPPORTED and deferred")
	assert.Zero(t, runs(), "a refused follow writes nothing")

	job := NewMarketplaceFollows(testDB, withSnapshots, w.clk, Settings{})
	rep = run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, in.ID.String()), "%+v", rep.Failures)
	require.Equal(t, 1, runs(), "the due install was followed")
	var trigger string
	var asOf time.Time
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT trigger, as_of FROM marketplace_follow_runs WHERE install_id = $1`, in.ID).
		Scan(&trigger, &asOf))
	assert.Equal(t, string(marketplace.TriggerScheduled), trigger)
	assert.True(t, asOf.Equal(w.clk.Now().Truncate(time.Second)), "at the run's instant")

	w.clk.Advance(10 * time.Minute)
	run(t, w.ctx, job)
	assert.Equal(t, 1, runs(), "not due again within the manifest's interval")
	w.clk.Advance(time.Hour)
	run(t, w.ctx, job)
	assert.Equal(t, 2, runs(), "due again after it")
}
