//go:build integration

package advancedmetrics

import (
	"context"
	"crypto/sha256"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/marketplace"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/risk"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// registerStable registers a fully reserved stable asset pegged to the
// sandbox dollar on the fixture's network, its breaker tripping at a 50 bps
// deviation of its own oracle price.
func (f *fixture) registerStable() stableasset.PlatformStableAsset {
	f.t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	sa, err := f.stable.Register(f.ctx, stableasset.Definition{
		Name: "amet-" + tag, Symbol: "AMPUSD" + strings.ToUpper(tag[:4]), PegCurrency: "USD", Decimals: 6,
		IssuanceCap: units(1_000_000, 6), RedemptionMode: stableasset.RedemptionInstant,
		PegQuoteAssetID: f.usd.ID, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6, OracleSource: "advancedmetrics-itest",
		MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.ReserveSpec{{AssetID: f.usd.ID, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true}},
	}, f.operator())
	require.NoError(f.t, err)
	return sa
}

// bridgeService is the bridge over the fixture's network, as the worker
// builds it.
func (f *fixture) bridgeService() *bridge.Service {
	f.t.Helper()
	svc, err := bridge.New(bridge.Deps{
		DB: testDB, Clock: f.clk, Host: f.chain.Host, Intents: f.intents, Effects: f.effects, Authorizer: f.authz,
		Facilities: f.registry, Ledger: f.ledger, Capital: f.capital, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: f.router, Kills: f.kills, Reconciliation: f.recon, Audit: f.audit, Accounts: accounts.NewRepository(),
		Assets: f.assets, Holder: f.chain.Keys.Custody, Attester: f.chain.Keys.Attester,
	})
	require.NoError(f.t, err)
	return svc
}

// bridgeRoute creates a route from a fresh devnet token, minted by the
// returned key, to sbx-ethereum.
func (f *fixture) bridgeRoute(svc *bridge.Service) (bridge.Route, devnet.Keypair) {
	f.t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	minter := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("advancedmetrics-minter-" + tag)))
	mint := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("advancedmetrics-mint-" + tag))).PublicKey()
	require.NoError(f.t, f.chain.Host.EnsureMint(f.ctx, devnethost.ChainSolana, devnet.MintConfig{
		Address: mint, Decimals: 6, Authority: minter.PublicKey(),
	}))
	src, err := f.assets.Create(f.ctx, testDB, assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: "AMBS" + strings.ToUpper(tag[:4]), Name: "bridge source " + tag + " (simulated)",
		Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.t, err)
	price, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", f.clk.Now())
	require.NoError(f.t, err)
	r, err := svc.CreateRoute(f.ctx, bridge.RouteDefinition{
		Name: "am-" + tag, SourceAssetID: src.ID, DestinationChain: devnethost.ChainEthereum,
		DestinationSymbol: "AMBD" + strings.ToUpper(tag[:4]), UnitPrice: price, MaxReleaseAttempts: 3,
	}, f.operator())
	require.NoError(f.t, err)
	return r, minter
}

// mintToEscrow puts q units of a route's source token in the bridge escrow
// with no lock: units its books never recorded.
func (f *fixture) mintToEscrow(r bridge.Route, minter devnet.Keypair, q uint64) {
	f.t.Helper()
	host := f.chain.Host
	require.NoError(f.t, host.EnsureNativeBalance(f.ctx, r.SourceChain, minter.PublicKey(), 1_000_000, 100_000_000))
	tip, err := host.Tip(f.ctx, r.SourceChain)
	require.NoError(f.t, err)
	tx, err := devnet.Sign(devnet.Message{ChainID: r.SourceChain, RecentBlockhash: tip.Hash, FeePayer: minter.PublicKey(),
		Instructions: []devnet.Instruction{devnet.TokenMintTo{Mint: r.SourceMint, ToOwner: devnet.BridgeEscrowAddress(r.SourceChain),
			Amount: q, Authority: minter.PublicKey()}}}, minter)
	require.NoError(f.t, err)
	raw, err := tx.Encode()
	require.NoError(f.t, err)
	txID, err := host.Submit(f.ctx, r.SourceChain, raw)
	require.NoError(f.t, err)
	_, err = host.ProduceBlocks(f.ctx, r.SourceChain, 1+devnet.DefaultFinalityDepth)
	require.NoError(f.t, err)
	st, err := host.Status(f.ctx, r.SourceChain, txID)
	require.NoError(f.t, err)
	require.Equal(f.t, devnet.TxFinalized, st.State, "%s", st.Err)
}

// TestIntegration_CircuitBreakerTripsAreCountedPerBreakerAndReason: a
// stable asset whose own price leaves its peg is paused by its breaker, and
// a bridge route whose escrow holds what no lock put there is paused by its
// breaker -- each counted once, by breaker and reason -- while an operator's
// pause is not a trip and a breaker that holds does not trip again.
func TestIntegration_CircuitBreakerTripsAreCountedPerBreakerAndReason(t *testing.T) {
	f := newFixture(t)
	sa := f.registerStable()
	routes := f.bridgeService()
	tripped, minter := f.bridgeRoute(routes)
	maintained, _ := f.bridgeRoute(routes)
	c := f.collector()
	require.Nil(t, f.collect(c).Counted)

	f.mark(sa.AssetID, "0.90")
	monitor := func() stableasset.MonitorReport {
		var rep stableasset.MonitorReport
		f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rep, err = f.stable.RunMonitor(ctx, tx, sa.ID, f.system)
			return err
		})
		return rep
	}
	assert.True(t, monitor().BreakerTripped, "a 1000 bps deviation against a 50 bps threshold")
	f.mintToEscrow(tripped, minter, 5)
	reconcile := func(r bridge.Route) bridge.RouteReconciliation {
		var rep bridge.RouteReconciliation
		f.inTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rep, err = routes.ReconcileRoute(ctx, tx, r.ID, reconciliation.ModePeriodic, f.system)
			return err
		})
		return rep
	}
	assert.True(t, reconcile(tripped).BreakerTripped, "the escrow holds 5 units no lock put there")
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := routes.TransitionRoute(ctx, tx, maintained.ID, bridge.RoutePaused, f.operator(), "maintenance")
		return err
	})

	s1 := f.collect(c)
	require.NotNil(t, s1.Counted)
	assert.Equal(t, map[Pair]int64{
		{BreakerStableAsset, string(stableasset.AlertPriceDeviation)}: 1,
		{BreakerBridgeRoute, ReasonReconciliationMismatch}:            1,
	}, s1.Counted.CircuitBreakerEvents, "the operator's pause is not a trip")

	assert.False(t, monitor().BreakerTripped, "a paused asset's breaker holds")
	assert.False(t, reconcile(tripped).BreakerTripped, "a paused route's breaker holds")
	s2 := f.collect(c)
	require.NotNil(t, s2.Counted)
	assert.Empty(t, s2.Counted.CircuitBreakerEvents, "no new trip, nothing counted again")
	assert.Equal(t, int64(1), f.counter("advanced_circuit_breaker_events", "kind", BreakerStableAsset))
	assert.Equal(t, int64(1), f.counter("advanced_circuit_breaker_events", "kind", BreakerBridgeRoute))
	assert.Equal(t, int64(2), f.counter("advanced_circuit_breaker_events", "", ""))
}

// unfollowed stands in for the marketplace's risk sections and paper
// executor, which a validation never reaches.
type unfollowed struct{}

func (unfollowed) RiskContext(context.Context, db.Querier, econ.Intent, sandboxrt.AcceptedProposal) (marketplace.RiskContext, error) {
	return marketplace.RiskContext{}, errs.New(errs.CodeUnsupported, "advancedmetrics itest: nothing is followed here")
}

func (unfollowed) ExecutePaper(context.Context, pgx.Tx, marketplace.PaperExecution) (marketplace.ExecutionReceipt, error) {
	return marketplace.ExecutionReceipt{}, errs.New(errs.CodeUnsupported, "advancedmetrics itest: nothing is followed here")
}

// marketplace is the strategy marketplace with the real builder and runner;
// it skips the test where no go toolchain can be found.
func (f *fixture) marketplace() *marketplace.Service {
	f.t.Helper()
	goBin, err := exec.LookPath("go")
	if err != nil {
		f.t.Skipf("the scanner runs inside the builder, which needs the go toolchain: %v", err)
	}
	goBin, err = filepath.Abs(goBin)
	require.NoError(f.t, err)
	dir := f.t.TempDir()
	builder, err := sandboxrt.NewBuilder(sandboxrt.BuilderConfig{GoBinary: goBin, CacheDir: filepath.Join(dir, "gocache"), TempDir: dir})
	require.NoError(f.t, err)
	runner, err := sandboxrt.NewRunner(f.ctx, sandboxrt.RunnerConfig{})
	require.NoError(f.t, err)
	f.t.Cleanup(func() { _ = runner.Close(context.Background()) })
	svc, err := marketplace.NewService(marketplace.Config{
		Clock: f.clk, Intents: f.intents, Effects: f.effects, Auth: f.authz, Risk: risk.NewStore(), RiskPolicy: risk.DefaultAdvancedPolicy(),
		Router: f.router, Sandbox: sandboxrt.NewService(f.clk, f.authz, runner, f.audit), Builder: builder, Sections: unfollowed{},
		Executor: unfollowed{}, Audit: f.audit, ServiceID: "advancedmetrics-itest",
	})
	require.NoError(f.t, err)
	return svc
}

// refusedVersion publishes a one-file strategy, submits it and runs its
// validation, which the scanner refuses.
func (f *fixture) refusedVersion(market *marketplace.Service, source string) marketplace.Validation {
	f.t.Helper()
	creator := f.customer()
	actor := econ.Actor{Type: security.ActorUser, ID: creator.user.String()}
	inst := uuid.NewString()
	var v marketplace.Version
	f.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, v, err = market.CreateListing(ctx, tx, marketplace.ListingRequest{CreatorAccountID: creator.account, Creator: actor,
			Title: "advancedmetrics " + inst[:8], Version: marketplace.VersionRequest{
				Description: "a strategy the scanner refuses", RiskClass: marketplace.RiskModerate, SupportedInstrumentIDs: []string{inst},
				Manifest: sandboxrt.Manifest{AllowedIntentKinds: []econ.IntentKind{econ.KindAcquireExposure}, AllowedInstrumentIDs: []string{inst},
					MaxNotionalPerIntent: "100.00", MaxIntentsPerRun: 1, MinIntervalSeconds: 3600}.Canonicalize(),
				Source: sandboxrt.Source{"main.go": []byte(source)}, SourceCommit: "advancedmetrics-itest",
				DataDependencies: []string{"closes"}, Config: map[string]string{"order_units": "1"},
			}})
		if err != nil {
			return err
		}
		v, err = market.Submit(ctx, tx, v.ID, actor)
		return err
	})
	got, val, err := market.RunValidation(f.ctx, testDB, v.ID, econ.SystemActor("marketplace.validator"))
	require.NoError(f.t, err)
	require.Equal(f.t, marketplace.StatusRejected, got.Status)
	require.Equal(f.t, marketplace.ValidationFailed, val.Outcome)
	require.Equal(f.t, sandboxrt.BuildFailureScan, val.FailureReason)
	require.Contains(f.t, val.ScanCodes, string(sandboxrt.ScanImportForbidden))
	return val
}

// TestIntegration_ScanRefusalsOfNetworkImportsAreNetworkViolations: the
// sandbox gives a strategy no network import, so the attempt is the scan:
// a version the scanner refused for importing net/http is one network
// violation, and one it refused for importing reflect -- forbidden, but
// neither the network nor another process -- is none.
func TestIntegration_ScanRefusalsOfNetworkImportsAreNetworkViolations(t *testing.T) {
	f := newFixture(t)
	market := f.marketplace()
	c := f.collector()
	require.Nil(t, f.collect(c).Counted)

	f.refusedVersion(market, "package main\n\nimport \"net/http\"\n\nvar _ = http.Get\n\nfunc main() {}\n")
	f.refusedVersion(market, "package main\n\nimport \"reflect\"\n\nvar _ = reflect.TypeOf\n\nfunc main() {}\n")

	s1 := f.collect(c)
	require.NotNil(t, s1.Counted)
	assert.Equal(t, int64(1), s1.Counted.SandboxNetworkViolations, "net/http is the network; reflect is not")
	s2 := f.collect(c)
	require.NotNil(t, s2.Counted)
	assert.Zero(t, s2.Counted.SandboxNetworkViolations, "counted once")
	assert.Equal(t, int64(1), f.counter("advanced_sandbox_network_violations", "", ""))
}
