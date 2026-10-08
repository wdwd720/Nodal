//go:build integration

package advancedjobs

import (
	"context"
	"crypto/sha256"
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
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxchain"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/stableasset"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// finality is enough blocks for anything submitted now to be FINALIZED on a
// default-configured devnet chain.
const finality = 1 + devnet.DefaultFinalityDepth

// chainFixture is one world's devnet network with the stable asset
// controller and the bridge over it.
type chainFixture struct {
	w      *world
	chain  sandboxchain.Chain
	stable *stableasset.Service
	bridge *bridge.Service
}

func (w *world) chainFixture() chainFixture {
	w.t.Helper()
	c := w.chain()
	return chainFixture{w: w, chain: c, stable: w.stable(c), bridge: w.bridge(c)}
}

func (f chainFixture) produce(chain devnet.ChainID) {
	f.w.t.Helper()
	_, err := f.chain.Host.ProduceBlocks(f.w.ctx, chain, finality)
	require.NoError(f.w.t, err)
}

// registerStable registers a fully reserved stable asset pegged to the
// sandbox dollar, with the given redemption mode.
func (f chainFixture) registerStable(mode stableasset.RedemptionMode) stableasset.PlatformStableAsset {
	f.w.t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	a, err := f.stable.Register(f.w.ctx, stableasset.Definition{
		Name: "ajobs-" + tag, Symbol: "AJPUSD" + strings.ToUpper(tag[:4]), PegCurrency: "USD", Decimals: 6,
		IssuanceCap: f.w.usdUnits(1_000_000), RedemptionMode: mode,
		PegQuoteAssetID: f.w.usd.ID, PegMantissa: money.QuantityFromInt64(1_000_000), PegScale: 6,
		OracleSource: "advancedjobs-itest", MaxPriceAge: time.Hour, PegDeviationThreshold: 50, Attestable: true,
		Reserves: []stableasset.ReserveSpec{
			{AssetID: f.w.usd.ID, Kind: stableasset.ReserveCashEquivalent, FactorBPS: 10_000, AcceptedForMint: true},
		},
	}, f.w.operator("stable"))
	require.NoError(f.w.t, err)
	f.w.mark(a.AssetID, "1.000000")
	return a
}

var stableFacts = stableasset.AccountFacts{Verification: valuedomain.VerificationNodalIdentity, IdentityVerified: true}

func (f chainFixture) mint(c customer, a stableasset.PlatformStableAsset, whole int64) (stableasset.MintResult, error) {
	return f.stable.Mint(security.WithPrincipal(f.w.ctx, c.p), stableasset.MintRequest{
		StableAssetID: a.ID, AccountID: c.account, Quantity: f.w.usdUnits(whole), ReserveAssetID: f.w.usd.ID,
		IdempotencyKey: "advancedjobs-mint-" + uuid.NewString(), Mode: gates.ModeSandbox,
		Actor: econ.Actor{Type: security.ActorUser, ID: c.user.String()}, Facts: stableFacts,
	})
}

// TestIntegration_AStableUnknownSubmissionResolvesThroughTheDevnetHost: a mint
// whose submission timed out but landed is SUBMISSION_UNKNOWN; the unknown-
// submission job waits while the chain has not finalized it, posts it once
// the devnet host says FINALIZED, never resubmits it, and does nothing more
// when it runs again.
func TestIntegration_AStableUnknownSubmissionResolvesThroughTheDevnetHost(t *testing.T) {
	w := newWorld(t)
	f := w.chainFixture()
	a := f.registerStable(stableasset.RedemptionInstant)
	c := w.customer("stable-unknown")
	w.fund(c.account, w.usd.ID, w.usdUnits(1_000))
	require.NoError(t, f.chain.Host.TimeoutButLand(w.ctx, a.Chain))
	res, err := f.mint(c, a, 100)
	require.Error(t, err)
	require.Equal(t, errs.CodeSubmissionStateUnknown, errs.CodeOf(err))
	require.Equal(t, stableasset.MintSubmissionUnknown, res.Request.Status)

	job, err := NewStableUnknown(testDB, f.stable, f.chain.Network, Settings{})
	require.NoError(t, err)
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.GreaterOrEqual(t, rep.Deferred, 1, "still in the mempool: wait")
	got, err := f.stable.GetMint(w.ctx, testDB, res.Request.ID)
	require.NoError(t, err)
	assert.Equal(t, stableasset.MintSubmissionUnknown, got.Status)

	f.produce(a.Chain)
	rep = run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	got, err = f.stable.GetMint(w.ctx, testDB, res.Request.ID)
	require.NoError(t, err)
	assert.Equal(t, stableasset.MintIssued, got.Status, "resolved from the chain's status by transaction id")
	assert.Equal(t, w.usdUnits(100).String(), w.balance(ledger.CustomerAccount(c.account, ledger.CodeWallet, a.AssetID)).String())

	run(t, w.ctx, job)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM devnet_events WHERE network = $1 AND kind = 'SUBMIT' AND tx_id = $2`,
		f.chain.Network, string(got.TxID)), "submitted exactly once, never again")
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM journal_transactions WHERE kind = 'STABLE_ISSUANCE' AND id = $1`, got.JournalTransactionID))

	// The monitor and the supply reconciliation see books, wallets, chain and
	// reserve agree, run after run.
	monitor, err := NewStableMonitor(testDB, f.stable, f.chain.Network, Settings{})
	require.NoError(t, err)
	supply, err := NewStableSupply(testDB, f.stable, f.chain.Network, Settings{})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		rep = run(t, w.ctx, monitor)
		assert.True(t, rep.OK(), "%+v", rep.Failures)
		assert.Zero(t, rep.Changed, "no alert and no breaker")
		rep = run(t, w.ctx, supply)
		assert.True(t, rep.OK(), "%+v", rep.Failures)
		assert.Zero(t, rep.Changed, "matched")
	}
	got2, err := f.stable.Get(w.ctx, testDB, a.ID)
	require.NoError(t, err)
	assert.Equal(t, stableasset.StatusActive, got2.Status)
}

// TestIntegration_AnUnknownSubmissionOfAnotherNetworkIsNeverResolvedHere: the
// unknown-submission job refuses to resolve anything while a request waiting
// on an outcome belongs to another network -- reading it from this host
// would find nothing and could fail a mint that stands final on its own
// chain -- and that request's own network resolves it.
func TestIntegration_AnUnknownSubmissionOfAnotherNetworkIsNeverResolvedHere(t *testing.T) {
	w := newWorld(t)
	here := w.chainFixture()
	there := w.chainFixture()
	a := there.registerStable(stableasset.RedemptionInstant)
	c := w.customer("stable-elsewhere")
	w.fund(c.account, w.usd.ID, w.usdUnits(1_000))
	require.NoError(t, there.chain.Host.TimeoutButLand(w.ctx, a.Chain))
	res, err := there.mint(c, a, 50)
	require.Error(t, err)
	there.produce(a.Chain)

	job, err := NewStableUnknown(testDB, here.stable, here.chain.Network, Settings{})
	require.NoError(t, err)
	_, err = job.Run(w.ctx)
	require.Error(t, err)
	assert.Equal(t, errs.CodeReconciliationRequired, errs.CodeOf(err))
	got, gerr := there.stable.GetMint(w.ctx, testDB, res.Request.ID)
	require.NoError(t, gerr)
	assert.Equal(t, stableasset.MintSubmissionUnknown, got.Status, "untouched by a host that cannot see its chain")

	own, err := NewStableUnknown(testDB, there.stable, there.chain.Network, Settings{})
	require.NoError(t, err)
	rep := run(t, w.ctx, own)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	got, gerr = there.stable.GetMint(w.ctx, testDB, res.Request.ID)
	require.NoError(t, gerr)
	assert.Equal(t, stableasset.MintIssued, got.Status, "its own network resolves it")
	_, err = job.Run(w.ctx)
	require.NoError(t, err, "with nothing foreign left the job runs again")
}

// TestIntegration_AQueuedRedemptionIsPaidOnce: a burn of a QUEUED-mode stable
// asset waits in REDEMPTION_QUEUED after finality; the redemption job pays it
// and a second run pays nothing more.
func TestIntegration_AQueuedRedemptionIsPaidOnce(t *testing.T) {
	w := newWorld(t)
	f := w.chainFixture()
	a := f.registerStable(stableasset.RedemptionQueued)
	c := w.customer("stable-redeemer")
	w.fund(c.account, w.usd.ID, w.usdUnits(1_000))
	minted, err := f.mint(c, a, 300)
	require.NoError(t, err)
	require.Equal(t, stableasset.MintIssued, minted.Request.Status)
	burned, err := f.stable.Burn(security.WithPrincipal(w.ctx, c.p), stableasset.BurnRequest{
		StableAssetID: a.ID, AccountID: c.account, Quantity: w.usdUnits(100), IdempotencyKey: "advancedjobs-burn-" + uuid.NewString(),
		Mode: gates.ModeSandbox, Actor: econ.Actor{Type: security.ActorUser, ID: c.user.String()}, Facts: stableFacts,
	})
	require.NoError(t, err)
	require.Equal(t, stableasset.BurnRedemptionQueued, burned.Request.Status)
	cash := func() string {
		return w.balance(ledger.CustomerAccount(c.account, ledger.CodeWallet, w.usd.ID)).String()
	}
	before := cash()

	job, err := NewStableRedemptions(testDB, f.stable, f.chain.Network, Settings{})
	require.NoError(t, err)
	rep := run(t, w.ctx, job)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, 1, rep.Changed)
	got, err := f.stable.GetBurn(w.ctx, testDB, burned.Request.ID)
	require.NoError(t, err)
	assert.Equal(t, stableasset.BurnRedeemed, got.Status)
	paid := w.balance(ledger.CustomerAccount(c.account, ledger.CodeWallet, w.usd.ID))
	assert.NotEqual(t, before, paid.String(), "the redemption paid out the reserve")

	rep = run(t, w.ctx, job)
	assert.Zero(t, rep.Changed)
	assert.Equal(t, paid.String(), cash(), "paid once")
}

// bridgeRoute creates a route from a fresh source asset on sbx-solana, minted
// by a test authority, to sbx-ethereum.
func (f chainFixture) bridgeRoute() (bridge.Route, devnet.Keypair) {
	f.w.t.Helper()
	tag := strings.ReplaceAll(uuid.NewString(), "-", "")[:8]
	minter := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("advancedjobs-minter-" + tag)))
	mint := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("advancedjobs-mint-" + tag))).PublicKey()
	require.NoError(f.w.t, f.chain.Host.EnsureMint(f.w.ctx, devnethost.ChainSolana, devnet.MintConfig{
		Address: mint, Decimals: 6, Authority: minter.PublicKey(),
	}))
	src, err := f.w.assets.Create(f.w.ctx, testDB, assets.Asset{
		Chain: string(devnethost.ChainSolana), MintAddress: mint.String(), Kind: assets.KindSimulated,
		ValueDomain: valuedomain.Simulated, Symbol: "AJBS" + strings.ToUpper(tag[:4]), Name: "bridge source " + tag + " (simulated)",
		Decimals: 6, RiskClass: assets.RiskSettlement, Status: assets.StatusActive,
	})
	require.NoError(f.w.t, err)
	price, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", f.w.clk.Now())
	require.NoError(f.w.t, err)
	r, err := f.bridge.CreateRoute(f.w.ctx, bridge.RouteDefinition{
		Name: "ajobs-" + tag, SourceAssetID: src.ID, DestinationChain: devnethost.ChainEthereum,
		DestinationSymbol: "AJBD" + strings.ToUpper(tag[:4]), UnitPrice: price, MaxReleaseAttempts: 3,
	}, f.w.operator("bridge"))
	require.NoError(f.w.t, err)
	return r, minter
}

// fundSource gives a customer q units of the route's source asset on the
// ledger and in its custodied wallet on the source chain.
func (f chainFixture) fundSource(r bridge.Route, minter devnet.Keypair, acct accounts.AccountID, q uint64) {
	f.w.t.Helper()
	f.w.fund(acct, r.SourceAssetID, devnet.QuantityOf(q))
	host := f.chain.Host
	require.NoError(f.w.t, host.EnsureNativeBalance(f.w.ctx, r.SourceChain, minter.PublicKey(), 1_000_000, 100_000_000))
	tip, err := host.Tip(f.w.ctx, r.SourceChain)
	require.NoError(f.w.t, err)
	msg := devnet.Message{ChainID: r.SourceChain, RecentBlockhash: tip.Hash, FeePayer: minter.PublicKey(),
		Instructions: []devnet.Instruction{devnet.TokenMintTo{Mint: r.SourceMint, ToOwner: f.chain.Keys.Custody.HolderAddress(acct),
			Amount: q, Authority: minter.PublicKey()}}}
	tx, err := devnet.Sign(msg, minter)
	require.NoError(f.w.t, err)
	raw, err := tx.Encode()
	require.NoError(f.w.t, err)
	_, err = host.Submit(f.w.ctx, r.SourceChain, raw)
	require.NoError(f.w.t, err)
	f.produce(r.SourceChain)
}

var bridgeFacts = bridge.AccountFacts{Jurisdiction: "US-CA", Verification: valuedomain.VerificationEnhanced, IdentityVerified: true}

func (f chainFixture) devnetLeg(r bridge.Route, c customer, q uint64) bridge.Leg {
	f.w.t.Helper()
	res, err := f.bridge.Bridge(security.WithPrincipal(f.w.ctx, c.p), bridge.BridgeRequest{
		RouteID: r.ID, AccountID: c.account, Quantity: devnet.QuantityOf(q), IdempotencyKey: "advancedjobs-leg-" + uuid.NewString(),
		Mode: gates.ModeDevnet, Actor: econ.Actor{Type: security.ActorUser, ID: c.user.String()}, Facts: bridgeFacts,
	})
	require.NoError(f.w.t, err)
	require.Equal(f.w.t, bridge.StateBridgeRequested, res.Leg.Status, "a DEVNET leg is only observed after its lock")
	return res.Leg
}

func (f chainFixture) legStatus(lid bridge.LegID) bridge.CrossChainCapitalState {
	f.w.t.Helper()
	l, err := f.bridge.GetLeg(f.w.ctx, testDB, lid)
	require.NoError(f.w.t, err)
	return l.Status
}

// TestIntegration_AnInFlightBridgeLegIsDrivenToReconciledByObservation: a
// DEVNET leg is only observed; the legs job advances it as the chains move --
// source final, release relayed once, destination final, reconciled -- never
// producing a block or resubmitting, and the routes job then finds the books
// and both chains agree.
func TestIntegration_AnInFlightBridgeLegIsDrivenToReconciledByObservation(t *testing.T) {
	w := newWorld(t)
	f := w.chainFixture()
	r, minter := f.bridgeRoute()
	c := w.customer("bridger")
	f.fundSource(r, minter, c.account, 1_000_000)
	leg := f.devnetLeg(r, c, 250_000)
	legs, err := NewBridgeLegs(testDB, f.bridge, f.chain.Network, Settings{})
	require.NoError(t, err)

	rep := run(t, w.ctx, legs)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, bridge.StateBridgeRequested, f.legStatus(leg.ID), "the lock is not final yet: nothing to do")

	f.produce(r.SourceChain)
	rep = run(t, w.ctx, legs)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, bridge.StateMessageInFlight, f.legStatus(leg.ID), "source final, release relayed")
	attempts := w.count(`SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1`, leg.ID)
	assert.Equal(t, 1, attempts)

	rep = run(t, w.ctx, legs)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, attempts, w.count(`SELECT count(*) FROM bridge_release_attempts WHERE leg_id = $1`, leg.ID),
		"a release that can still land is never raced by another")

	f.produce(r.DestinationChain)
	rep = run(t, w.ctx, legs)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, bridge.StateReconciled, f.legStatus(leg.ID))
	assert.Equal(t, devnet.QuantityOf(250_000).String(),
		w.balance(ledger.CustomerAccount(c.account, ledger.CodeWallet, r.DestinationAssetID)).String())

	routes, err := NewBridgeRoutes(testDB, f.bridge, f.chain.Network, Settings{})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		rep = run(t, w.ctx, routes)
		assert.True(t, rep.OK(), "%+v", rep.Failures)
		assert.Zero(t, rep.Changed, "books and both chains agree")
	}
}

// TestIntegration_ALegLeftDestinationFinalizedIsReconciledOnce: a leg whose
// driving process stopped right after its destination posting is reconciled
// by the legs job (bridge.Service.ReconcileLeg), once.
func TestIntegration_ALegLeftDestinationFinalizedIsReconciledOnce(t *testing.T) {
	w := newWorld(t)
	f := w.chainFixture()
	r, minter := f.bridgeRoute()
	c := w.customer("bridger-stopped")
	f.fundSource(r, minter, c.account, 1_000_000)
	leg := f.devnetLeg(r, c, 100_000)

	// The bridge's own stages, as a driving process takes them, stopping
	// after the destination posting.
	f.produce(r.SourceChain)
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.bridge.SettleSource(ctx, tx, leg.ID)
		return err
	}))
	var attempt *bridge.Attempt
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		attempt, err = f.bridge.PrepareRelease(ctx, tx, leg.ID)
		return err
	}))
	require.NotNil(t, attempt)
	require.NoError(t, f.chain.Host.EnsureNativeBalance(w.ctx, attempt.Chain, f.chain.Keys.Attester.PublicKey(), 1_000_000, 100_000_000))
	_, err := f.chain.Host.Submit(w.ctx, attempt.Chain, attempt.Raw)
	require.NoError(t, err)
	f.produce(r.DestinationChain)
	require.NoError(t, w.inTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := f.bridge.SettleDestination(ctx, tx, leg.ID)
		return err
	}))
	require.Equal(t, bridge.StateDestinationFinalized, f.legStatus(leg.ID))

	legs, err := NewBridgeLegs(testDB, f.bridge, f.chain.Network, Settings{})
	require.NoError(t, err)
	rep := run(t, w.ctx, legs)
	assert.True(t, rep.OK(), "%+v", rep.Failures)
	assert.Equal(t, bridge.StateReconciled, f.legStatus(leg.ID))
	run(t, w.ctx, legs)
	assert.Equal(t, 1, w.count(`SELECT count(*) FROM bridge_leg_transitions WHERE leg_id = $1 AND to_status = 'RECONCILED'`, leg.ID),
		"reconciled once")
}
