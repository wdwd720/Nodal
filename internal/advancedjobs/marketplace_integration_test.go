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
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuation"
)

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
// as the system scheduler, on the input the marketplace builds itself -- the
// install's paper book and the price store's fresh reference price, with too
// little history for the strategy to propose anything -- and is not due again
// within the manifest's interval.
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
	svc, err := marketplace.NewService(marketplace.Config{
		Clock: w.clk, Intents: w.intents, Effects: w.effects, Auth: w.authz, Risk: risk.NewStore(),
		RiskPolicy: risk.DefaultAdvancedPolicy(), Router: w.router,
		Sandbox: sandboxrt.NewService(w.clk, w.authz, runner, w.audit), Builder: builder,
		Sections: refusingSections{}, Executor: refusingExecutor{}, Audit: w.audit,
	})
	require.NoError(t, err)
	// One observation of the pair: the reference price, and a history of
	// one close, too little for the momentum strategy to propose anything.
	price, err := money.PriceFromDecimalString("150.25", usdc.ID.String(), "advancedjobs-itest", w.clk.Now())
	require.NoError(t, err)
	_, err = w.prices.RecordPrice(w.ctx, testDB, valuation.PriceObservation{AssetID: base.ID, QuoteAssetID: usdc.ID,
		Mantissa: price.Mantissa, Scale: price.Scale, Source: "advancedjobs-itest", ObservedAt: w.clk.Now()})
	require.NoError(t, err)

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
		_, v, err := svc.CreateListing(ctx, tx, marketplace.ListingRequest{CreatorAccountID: creator.account, Creator: creatorActor,
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
		return svc.Submit(ctx, tx, vid, creatorActor)
	})
	// A build runs between transactions, never inside one.
	_, _, err = svc.RunValidation(w.ctx, testDB, vid, econ.SystemActor("marketplace.validator"))
	require.NoError(t, err)
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return svc.StartReview(ctx, tx, vid, operator)
	})
	step(func(ctx context.Context, tx pgx.Tx) (marketplace.Version, error) {
		return svc.Approve(ctx, tx, vid, operator, "MEETS_POLICY", "advanced worker integration test")
	})
	require.Equal(t, marketplace.StatusApproved, v.Status)
	var in marketplace.Install
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		in, _, err = svc.InstallVersion(ctx, tx, marketplace.InstallRequest{VersionID: vid, AccountID: follower.account,
			Mode: gates.ModePaper, Actor: econ.Actor{Type: security.ActorUser, ID: follower.user.String()}})
		return err
	}))
	runs := func() int {
		return w.count(`SELECT count(*) FROM marketplace_follow_runs WHERE install_id = $1`, in.ID)
	}

	job := NewMarketplaceFollows(testDB, svc, w.clk, Settings{})
	rep := run(t, w.ctx, job)
	assert.False(t, failedUnit(rep, in.ID.String()), "%+v", rep.Failures)
	require.Equal(t, 1, runs(), "the due install was followed")
	var trigger, actorType string
	var asOf time.Time
	require.NoError(t, testDB.QueryRow(w.ctx, `SELECT trigger, actor_type, as_of FROM marketplace_follow_runs WHERE install_id = $1`, in.ID).
		Scan(&trigger, &actorType, &asOf))
	assert.Equal(t, string(marketplace.TriggerScheduled), trigger)
	assert.Equal(t, string(security.ActorSystem), actorType, "a scheduled follow is the system scheduler's")
	assert.True(t, asOf.Equal(w.clk.Now().Truncate(time.Second)), "at the run's instant")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM marketplace_follow_marks m JOIN marketplace_follow_runs f ON f.id = m.follow_run_id
		WHERE f.install_id = $1`, in.ID), "valued at the price store's reference price, which the run records")
	assert.Zero(t, w.count(`SELECT count(*) FROM marketplace_follow_intents fi JOIN marketplace_follow_runs f ON f.id = fi.follow_run_id
		WHERE f.install_id = $1`, in.ID), "a quiet run proposes nothing")

	w.clk.Advance(10 * time.Minute)
	run(t, w.ctx, job)
	assert.Equal(t, 1, runs(), "not due again within the manifest's interval")
	w.clk.Advance(time.Hour)
	run(t, w.ctx, job)
	assert.Equal(t, 2, runs(), "due again after it")
}
