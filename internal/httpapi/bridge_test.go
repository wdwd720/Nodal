package httpapi

import (
	"context"
	"crypto/sha256"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/bridge"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

var (
	bridgeTestRouteID = stableTestID[*bridge.RouteID]()
	bridgeTestLegID   = stableTestID[*bridge.LegID]()
	bridgeTestTwin    = assets.NewAssetID()
	bridgeBase        = "/v1/advanced/bridge"
	bridgeRoutePath   = bridgeBase + "/routes/" + bridgeTestRouteID.String()
	bridgeLegPath     = bridgeBase + "/legs/" + bridgeTestLegID.String()
)

// fakeBridgePort is the bridge port. It records what each call was asked.
type fakeBridgePort struct {
	stubErr
	mu    sync.Mutex
	calls map[string]int

	route      BridgeRouteView
	leg        BridgeLegDetailView
	outcome    BridgeLegOutcome
	holdings   BridgeHoldingsView
	chains     BridgeChainsView
	recon      bridge.RouteReconciliation
	nextCursor string

	lastAccount accounts.AccountID
	lastCursor  string
	lastLimit   int
	lastLeg     BridgeLegCommand
	lastDef     bridge.RouteDefinition
	lastActor   econ.Actor
	lastStatus  bridge.RouteStatus
	lastReason  string
	lastChain   devnet.ChainID
	lastBlocks  uint64
	lastFault   BridgeFault

	lastReviewLeg bridge.LegID
}

func (f *fakeBridgePort) called(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeBridgePort) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeBridgePort) Routes(_ context.Context, limit int) ([]BridgeRouteView, error) {
	f.called("Routes")
	f.mu.Lock()
	f.lastLimit = limit
	f.mu.Unlock()
	return []BridgeRouteView{f.route}, f.fail()
}

func (f *fakeBridgePort) Route(context.Context, bridge.RouteID) (BridgeRouteView, error) {
	f.called("Route")
	return f.route, f.fail()
}

func (f *fakeBridgePort) Legs(_ context.Context, account accounts.AccountID, cursor string, limit int) (BridgeLegPage, error) {
	f.called("Legs")
	f.mu.Lock()
	f.lastAccount, f.lastCursor, f.lastLimit = account, cursor, limit
	f.mu.Unlock()
	return BridgeLegPage{Items: []BridgeLegView{f.leg.BridgeLegView}, NextCursor: f.nextCursor}, f.fail()
}

func (f *fakeBridgePort) Leg(context.Context, bridge.LegID) (BridgeLegDetailView, error) {
	f.called("Leg")
	return f.leg, f.fail()
}

func (f *fakeBridgePort) Holdings(_ context.Context, account accounts.AccountID) (BridgeHoldingsView, error) {
	f.called("Holdings")
	f.mu.Lock()
	f.lastAccount = account
	f.mu.Unlock()
	v := f.holdings
	v.AccountID = account
	return v, f.fail()
}

func (f *fakeBridgePort) Chains(context.Context) (BridgeChainsView, error) {
	f.called("Chains")
	return f.chains, f.fail()
}

func (f *fakeBridgePort) Bridge(_ context.Context, c BridgeLegCommand) (BridgeLegOutcome, error) {
	f.called("Bridge")
	f.mu.Lock()
	f.lastLeg = c
	f.mu.Unlock()
	return f.outcome, f.fail()
}

func (f *fakeBridgePort) CreateRoute(_ context.Context, def bridge.RouteDefinition, by econ.Actor) (BridgeRouteView, error) {
	f.called("CreateRoute")
	f.mu.Lock()
	f.lastDef, f.lastActor = def, by
	f.mu.Unlock()
	return f.route, f.fail()
}

func (f *fakeBridgePort) SetRouteStatus(_ context.Context, _ bridge.RouteID, to bridge.RouteStatus, by econ.Actor, reason string) (BridgeRouteView, error) {
	f.called("SetRouteStatus:" + string(to))
	f.mu.Lock()
	f.lastStatus, f.lastActor, f.lastReason = to, by, reason
	f.mu.Unlock()
	return f.route, f.fail()
}

func (f *fakeBridgePort) ReconcileRoute(_ context.Context, _ bridge.RouteID, by econ.Actor) (bridge.RouteReconciliation, error) {
	f.called("ReconcileRoute")
	f.mu.Lock()
	f.lastActor = by
	f.mu.Unlock()
	return f.recon, f.fail()
}

func (f *fakeBridgePort) ReconcileLeg(context.Context, bridge.LegID) (BridgeLegOutcome, error) {
	f.called("ReconcileLeg")
	return f.outcome, f.fail()
}

func (f *fakeBridgePort) ProduceBlocks(_ context.Context, chain devnet.ChainID, n uint64) (BridgeChainView, error) {
	f.called("ProduceBlocks")
	f.mu.Lock()
	f.lastChain, f.lastBlocks = chain, n
	f.mu.Unlock()
	return f.chains.Chains[1], f.fail()
}

func (f *fakeBridgePort) InjectFault(_ context.Context, chain devnet.ChainID, fault BridgeFault) (BridgeChainView, error) {
	f.called("InjectFault")
	f.mu.Lock()
	f.lastChain, f.lastFault = chain, fault
	f.mu.Unlock()
	return f.chains.Chains[1], f.fail()
}

// The ways out of a leg's manual review record the leg, the operator and the
// reason, and answer the fake's leg.
func (f *fakeBridgePort) review(name string, lid bridge.LegID, by econ.Actor, reason string) error {
	f.called(name)
	f.mu.Lock()
	f.lastReviewLeg, f.lastActor, f.lastReason = lid, by, reason
	f.mu.Unlock()
	return f.fail()
}

func (f *fakeBridgePort) ResumeRelay(_ context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error) {
	if err := f.review("ResumeRelay", lid, by, reason); err != nil {
		return BridgeLegView{}, err
	}
	return f.outcome.View, nil
}

func (f *fakeBridgePort) ResolveReview(_ context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegView, error) {
	if err := f.review("ResolveReview", lid, by, reason); err != nil {
		return BridgeLegView{}, err
	}
	return f.outcome.View, nil
}

func (f *fakeBridgePort) Refund(_ context.Context, lid bridge.LegID, by econ.Actor, reason string) (BridgeLegOutcome, error) {
	if err := f.review("Refund", lid, by, reason); err != nil {
		return BridgeLegOutcome{}, err
	}
	return f.outcome, nil
}

// newFakeBridgePort holds a route from the stable asset on sbx-solana to its
// twin on sbx-ethereum and one reconciled leg of 100 units of the test
// account, plus an open leg in flight for the holdings.
func newFakeBridgePort(t *testing.T) *fakeBridgePort {
	t.Helper()
	unit, err := money.NewPrice(money.QuantityFromInt64(1), 0, "USD", "ROUTE_UNIT_PRICE", testNow)
	require.NoError(t, err)
	attester := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi attester"))).PublicKey()
	route := bridge.Route{
		ID: bridgeTestRouteID, Name: "pusd-solana-to-ethereum", FacilityID: econ.NewFacilityID(), Network: "httpapi-sbx",
		SourceChain: devnethost.ChainSolana, SourceAssetID: stableTestUnits, SourceMint: bridgeTestSourceMint(),
		DestinationChain: devnethost.ChainEthereum, DestinationAssetID: bridgeTestTwin,
		DestinationMint: bridge.DestinationMintAddress("httpapi-sbx", "pusd-solana-to-ethereum"), Decimals: 6,
		Attester: attester, UnitPrice: unit, MaxReleaseAttempts: 3, Status: bridge.RouteActive, CreatedAt: testNow,
	}
	rv := BridgeRouteView{Route: route, Source: StableAssetLabel{Symbol: "SBX-PUSD", Decimals: 6},
		Destination: StableAssetLabel{Symbol: "SBX-PUSD.E", Decimals: 6}}
	holder := devnet.NewKeypairFromSeed(sha256.Sum256([]byte("httpapi holder"))).PublicKey()
	srcEffect, dstEffect := econ.NewEffectID(), econ.NewEffectID()
	srcJ, dstJ := ledger.NewTransactionID(), ledger.NewTransactionID()
	leg := bridge.Leg{
		ID: bridgeTestLegID, RouteID: bridgeTestRouteID, AccountID: testAccountID, IntentID: econ.NewIntentID(), Mode: gates.ModeSandbox,
		Quantity: qty("100000000"), SourceOwner: holder, DestinationOwner: holder, Nonce: 18446744073709551615,
		PlanHash: sha256.Sum256([]byte("bridge plan")), Status: bridge.StateReconciled, SourceEffectID: &srcEffect,
		DestinationEffectID: &dstEffect, LockTxID: devnet.TxID("lock-tx-1"), SourceJournal: &srcJ, DestinationJournal: &dstJ,
		CreatedAt: testNow, UpdatedAt: testNow,
	}
	applied := econ.EffectApplied
	lv := BridgeLegView{Leg: leg, Route: rv, Effects: BridgeLegEffects{Source: &applied, Destination: &applied}}
	detail := BridgeLegDetailView{
		BridgeLegView: lv,
		Lock: &BridgeChainTxView{Chain: devnethost.ChainSolana, TxID: leg.LockTxID,
			Status: devnet.TxStatus{State: devnet.TxFinalized, Slot: 10, Height: 10, Final: true}},
		Attempts: []BridgeAttemptView{{Number: 1, Kind: bridge.AttemptRelease, Chain: devnethost.ChainEthereum, TxID: "release-tx-1",
			BlockhashHeight: 4, CreatedAt: testNow, OnChain: BridgeChainTxView{Chain: devnethost.ChainEthereum, TxID: "release-tx-1",
				Status: devnet.TxStatus{State: devnet.TxFinalized, Slot: 7, Height: 7, Final: true}}}},
		Transitions: []BridgeLegTransition{
			{From: "NONE", To: bridge.StateAvailable, ActorType: "USER", ActorID: testUserID.String(), Reason: "requested", OccurredAt: testNow},
			{From: "AVAILABLE", To: bridge.StateReserved, ActorType: "USER", ActorID: testUserID.String(), Reason: "units reserved", OccurredAt: testNow},
		},
	}
	openLeg := stableTestID[*bridge.LegID]()
	cc := bridge.CrossChainBuyingPower{
		Holdings: []buyingpower.V2Holding{{Component: buyingpower.ComponentBridgeInFlight, AssetID: stableTestUnits.String(), Symbol: "SBX-PUSD",
			Quantity: qty("50000000"), MarkedUSD: usd("50.00"), FactorBPS: 0, Reason: "bridge lock final on the source, release not yet observed"}},
		PendingLocks: usd("25.00"),
		Legs: []bridge.InFlightLeg{{LegID: openLeg, RouteID: bridgeTestRouteID, AssetID: stableTestUnits, Status: bridge.StateMessageInFlight,
			Holding: bridge.HoldingInFlight, Quantity: qty("50000000"), FactorBPS: 0, Reason: "bridge lock final on the source, release not yet observed"}},
		Policy: bridge.DefaultHaircutPolicy(),
	}
	window := uint64(150)
	return &fakeBridgePort{
		calls: map[string]int{}, route: rv, leg: detail, outcome: BridgeLegOutcome{View: lv, Settled: true},
		holdings: BridgeHoldingsView{
			AsOf: testNow,
			Chains: []BridgeChainHoldings{
				{Chain: "nodal-sandbox", Holdings: []BridgeHolding{{AssetID: econ.SandboxDollarID(), Label: StableAssetLabel{Symbol: econ.SandboxDollarSymbol, Decimals: 6}, Quantity: qty("400000000")}}},
				{Chain: "sbx-ethereum", Holdings: []BridgeHolding{{AssetID: bridgeTestTwin, Label: StableAssetLabel{Symbol: "SBX-PUSD.E", Decimals: 6}, Quantity: qty("100000000")}}},
			},
			CrossChain: cc,
			Labels:     map[assets.AssetID]StableAssetLabel{stableTestUnits: {Symbol: "SBX-PUSD", Decimals: 6}},
		},
		chains: BridgeChainsView{Network: "httpapi-sbx", FaultInjectionAllowed: true, Chains: []BridgeChainView{
			{Params: devnethost.ChainParams{ID: devnethost.ChainEthereum, BlockhashWindow: window, FinalityDepth: 32, FeePerTx: 5000, BridgeAuthority: attester},
				Tip: devnet.BlockInfo{Slot: 40, Height: 40}, FinalizedHeight: 8},
			{Params: devnethost.ChainParams{ID: devnethost.ChainSolana, BlockhashWindow: window, FinalityDepth: 32, FeePerTx: 5000, BridgeAuthority: attester},
				Tip: devnet.BlockInfo{Slot: 90, Height: 90}, FinalizedHeight: 58},
		}},
		recon: bridge.RouteReconciliation{RouteID: bridgeTestRouteID, Locked: qty("100000000"), Outstanding: qty("100000000"),
			InFlight: qty("0"), Refunded: qty("0"), Escrow: qty("100000000"), DestinationSupply: qty("100000000"),
			LandedUnpostedLocks: qty("0"), LandedUnpostedReleases: qty("0"), ExpectedEscrow: qty("100000000"),
			ExpectedSupply: qty("100000000"), BooksConserve: true, EscrowMatches: true, SupplyMatches: true},
	}
}

// bridgeTestSourceMint is the source mint the route names in these tests.
func bridgeTestSourceMint() devnet.PublicKey {
	return devnet.PublicKey(sha256.Sum256([]byte("httpapi source mint")))
}

func newBridgeHarness(t *testing.T) (*harness, *fakeBridgePort) {
	t.Helper()
	h := newHarness(t)
	fp := newFakeBridgePort(t)
	ports := h.ports.ports()
	ports.Bridge = fp
	h.server.opts.Ports = ports
	return h, fp
}

type bridgeMutation struct {
	name     string
	path     string
	body     any
	other    any
	call     string
	customer bool
	stepUp   bool
}

func bridgeLegBody(q string) map[string]any {
	return map[string]any{"account_id": testAccountID.String(), "route_id": bridgeTestRouteID.String(), "quantity": q, "mode": "SANDBOX"}
}

func bridgeRouteBody(name string) map[string]any {
	return map[string]any{"name": name, "source_asset_id": stableTestUnits.String(), "destination_chain": "sbx-ethereum",
		"destination_symbol": "SBX-PUSD.E", "unit_price": "1", "max_release_attempts": 3}
}

func bridgeMutations() []bridgeMutation {
	noBody := map[string]any{"different": "body"}
	reason := func(r string) map[string]any { return map[string]any{"reason": r} }
	return []bridgeMutation{
		{"bridge", bridgeBase + "/legs", bridgeLegBody("100000000"), bridgeLegBody("200000000"), "Bridge", true, false},
		{"create-route", bridgeBase + "/routes", bridgeRouteBody("pusd-solana-to-ethereum"), bridgeRouteBody("pusd-other"), "CreateRoute", false, true},
		{"pause", bridgeRoutePath + "/pause", reason("the destination is being investigated"), reason("another reason"), "SetRouteStatus:PAUSED", false, true},
		{"resume", bridgeRoutePath + "/resume", reason("the books reconcile again"), reason("another reason"), "SetRouteStatus:ACTIVE", false, true},
		{"reconcile-route", bridgeRoutePath + "/reconciliations", nil, noBody, "ReconcileRoute", false, false},
		{"reconcile-leg", bridgeLegPath + "/reconcile", nil, noBody, "ReconcileLeg", false, true},
		{"blocks", bridgeBase + "/chains/sbx-solana/blocks", map[string]any{"count": 40}, map[string]any{"count": 41}, "ProduceBlocks", false, true},
		{"fault", bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "TIMEOUT_BUT_LAND"}, map[string]any{"kind": "CLEAR_FAULTS"}, "InjectFault", false, true},
	}
}

func bridgeReads() []string {
	return []string{
		bridgeBase + "/routes",
		bridgeRoutePath,
		bridgeBase + "/legs?account_id=" + testAccountID.String(),
		bridgeLegPath,
		bridgeBase + "/holdings?account_id=" + testAccountID.String(),
		bridgeBase + "/chains",
	}
}

func TestBridge_UnwiredPortIsUnsupportedOnEveryRoute(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	op := stableOperator()
	h.as(&op)
	for _, path := range bridgeReads() {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", path, res.Body.String())
		p := res.problem()
		assert.Equal(t, errs.CodeUnsupported, p.Code, path)
		assert.Contains(t, p.Detail, "no sandbox chain is configured", path)
	}
	for i, m := range bridgeMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "bridge-unwired-"+itoa(i)+"-key")
		require.Equal(t, http.StatusUnprocessableEntity, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeUnsupported, res.problem().Code, m.name)
	}
}

func TestBridge_Routes_RenderTheRouteExactly(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	res := h.do(http.MethodGet, bridgeBase+"/routes?limit=9", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 9, fp.lastLimit)
	var list api.BridgeRouteList
	res.json(&list)
	assert.Equal(t, api.SIMULATED, list.ValueDomain)
	require.Len(t, list.Items, 1)
	r := list.Items[0]
	assert.Equal(t, "sbx-solana", r.SourceChain)
	assert.Equal(t, "sbx-ethereum", r.DestinationChain)
	assert.Equal(t, "SBX-PUSD", r.SourceSymbol)
	assert.Equal(t, "SBX-PUSD.E", r.DestinationSymbol)
	assert.Equal(t, "1", r.UnitPrice.Mantissa)
	assert.Equal(t, "1", r.UnitPrice.Decimal)
	assert.Equal(t, api.BridgeRouteStatusActive, r.Status)

	res = h.do(http.MethodGet, bridgeRoutePath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	fp.err = errs.New(errs.CodeNotFound, "bridge route not found")
	res = h.do(http.MethodGet, bridgeRoutePath, nil)
	require.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
	res = h.do(http.MethodGet, bridgeBase+"/routes/not-a-uuid", nil)
	require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
}

func TestBridge_Legs_AreScopedAndPaged(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	fp.nextCursor = "older"
	res := h.do(http.MethodGet, bridgeBase+"/legs?account_id="+testAccountID.String()+"&cursor=c1&limit=3", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fp.lastAccount)
	assert.Equal(t, "c1", fp.lastCursor)
	assert.Equal(t, 3, fp.lastLimit)
	var page api.BridgeLegPage
	res.json(&page)
	assert.Equal(t, api.SIMULATED, page.ValueDomain)
	require.Len(t, page.Items, 1)
	l := page.Items[0]
	assert.Equal(t, "100000000", l.Quantity)
	assert.Equal(t, "18446744073709551615", l.Nonce, "a u64 nonce as an exact decimal string")
	assert.Equal(t, api.BridgeLegStatusReconciled, l.Status)
	assert.Equal(t, api.BridgeLegHoldingWalletDestination, l.Holding)
	require.NotNil(t, l.SourceEffectStatus)
	assert.Equal(t, api.BridgeEffectStatusApplied, *l.SourceEffectStatus)
	assert.Nil(t, l.RefundEffectStatus)
	assert.Equal(t, api.BridgeReconciliationNotRequired, l.Reconciliation.State)
	assert.Equal(t, "older", res.raw()["next_cursor"])

	calls := fp.count("Legs")
	res = h.do(http.MethodGet, bridgeBase+"/legs?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, calls, fp.count("Legs"), "a refused account never reaches the port")

	op := stableOperator()
	h.as(&op)
	res = h.do(http.MethodGet, bridgeBase+"/legs?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testOtherAcct, fp.lastAccount)
}

func TestBridge_LegByID_ShowsItsProgressAndMasksOtherAccounts(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	res := h.do(http.MethodGet, bridgeLegPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var d api.BridgeLegDetail
	res.json(&d)
	assert.Equal(t, api.SIMULATED, d.ValueDomain)
	require.NotNil(t, d.Lock)
	assert.Equal(t, api.BridgeTxStateFinalized, d.Lock.State)
	assert.True(t, d.Lock.Final)
	require.NotNil(t, d.Lock.Height)
	assert.Equal(t, int64(10), *d.Lock.Height)
	require.Len(t, d.Attempts, 1)
	assert.Equal(t, api.BridgeAttemptKindRelease, d.Attempts[0].Kind)
	assert.Equal(t, api.BridgeTxStateFinalized, d.Attempts[0].OnChain.State)
	require.Len(t, d.Transitions, 2)
	assert.Equal(t, "NONE", d.Transitions[0].FromStatus)
	assert.Equal(t, api.BridgeLegStatusReserved, d.Transitions[1].ToStatus)

	// A pending transaction reports no height and no error.
	fp.leg.Attempts[0].OnChain.Status = devnet.TxStatus{State: devnet.TxPending}
	res = h.do(http.MethodGet, bridgeLegPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var pending api.BridgeLegDetail
	res.json(&pending)
	assert.Equal(t, api.BridgeTxStatePending, pending.Attempts[0].OnChain.State)
	assert.Nil(t, pending.Attempts[0].OnChain.Height)
	assert.Nil(t, pending.Attempts[0].OnChain.Error)

	fp.leg.Leg.AccountID = testOtherAcct
	foreign := h.do(http.MethodGet, bridgeLegPath, nil)
	require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
	fp.err = errs.New(errs.CodeNotFound, "bridge leg not found")
	missing := h.do(http.MethodGet, bridgeLegPath, nil)
	require.Equal(t, http.StatusNotFound, missing.Code, "body=%s", missing.Body.String())
	assert.Equal(t, foreign.problem().Detail, missing.problem().Detail, "absent and not yours are one answer")
	fp.err = nil

	op := stableOperator()
	h.as(&op)
	res = h.do(http.MethodGet, bridgeLegPath, nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
}

func TestBridge_Bridge_ActsForTheOwnerAndShowsAnUnknownLockAsUnknown(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	res := h.do(http.MethodPost, bridgeBase+"/legs", bridgeLegBody("100000000"), "Idempotency-Key", "bridge-leg-key-0001")
	require.Equal(t, http.StatusOK, res.Code, "a final leg is 200; body=%s", res.Body.String())
	c := fp.lastLeg
	assert.Equal(t, bridgeTestRouteID, c.RouteID)
	assert.Equal(t, testAccountID, c.AccountID)
	assert.Equal(t, "100000000", c.Quantity.String())
	assert.Equal(t, gates.ModeSandbox, c.Mode)
	assert.Equal(t, "bridge-leg-key-0001", c.IdempotencyKey)
	assert.Equal(t, econ.Actor{Type: security.ActorUser, ID: testUserID.String()}, c.Actor)
	var out api.BridgeLegResult
	res.json(&out)
	assert.True(t, out.Settled)
	assert.Equal(t, api.SIMULATED, out.ValueDomain)

	// The lock's submission outcome is unknown: 202, the leg still
	// BRIDGE_REQUESTED, its source effect UNKNOWN, awaiting reconciliation.
	unknown := econ.EffectUnknown
	since := testNow.Add(time.Second)
	fp.outcome = BridgeLegOutcome{View: fp.outcome.View}
	fp.outcome.View.Leg.Status, fp.outcome.View.Leg.SourceJournal, fp.outcome.View.Leg.DestinationJournal = bridge.StateBridgeRequested, nil, nil
	fp.outcome.View.Effects = BridgeLegEffects{Source: &unknown}
	fp.outcome.View.Reconciliation = BridgeLegReconciliation{Awaiting: true, UnknownSince: &since}
	res = h.do(http.MethodPost, bridgeBase+"/legs", bridgeLegBody("100000000"), "Idempotency-Key", "bridge-leg-key-0002")
	require.Equal(t, http.StatusAccepted, res.Code, "body=%s", res.Body.String())
	res.json(&out)
	assert.False(t, out.Settled)
	assert.Equal(t, api.BridgeLegStatusBridgeRequested, out.Leg.Status)
	assert.Equal(t, api.BridgeLegHoldingWalletSourceReserved, out.Leg.Holding)
	require.NotNil(t, out.Leg.SourceEffectStatus)
	assert.Equal(t, api.BridgeEffectStatusUnknown, *out.Leg.SourceEffectStatus)
	assert.Equal(t, api.BridgeReconciliationAwaiting, out.Leg.Reconciliation.State)
	require.NotNil(t, out.Leg.Reconciliation.UnknownSince)

	foreign := bridgeLegBody("1")
	foreign["account_id"] = testOtherAcct.String()
	res = h.do(http.MethodPost, bridgeBase+"/legs", foreign, "Idempotency-Key", "bridge-leg-foreign-1")
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
	for i, bad := range []map[string]any{
		bridgeLegBody("1.5"), bridgeLegBody(""),
		func() map[string]any { b := bridgeLegBody("1"); b["mode"] = "PAPER"; return b }(),
		func() map[string]any { b := bridgeLegBody("1"); b["route_id"] = "nope"; return b }(),
	} {
		res = h.do(http.MethodPost, bridgeBase+"/legs", bad, "Idempotency-Key", "bridge-leg-bad-"+itoa(i))
		require.Equal(t, http.StatusBadRequest, res.Code, "%v body=%s", bad, res.Body.String())
	}
	assert.Equal(t, 2, fp.count("Bridge"))

	fp.err = errs.New(errs.CodeAssetRestricted, "bridge: the route is PAUSED").WithField("reason", bridge.RefusalRouteNotActive)
	res = h.do(http.MethodPost, bridgeBase+"/legs", bridgeLegBody("100000000"), "Idempotency-Key", "bridge-leg-key-0003")
	require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
	p := res.problem()
	assert.Equal(t, errs.CodeAssetRestricted, p.Code)
	assert.Equal(t, bridge.RefusalRouteNotActive, p.Fields["reason"])
}

func TestBridge_Holdings_GroupByChainAndCountInFlightAtTheHaircut(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	res := h.do(http.MethodGet, bridgeBase+"/holdings?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, testAccountID, fp.lastAccount)
	var hv api.BridgeHoldings
	res.json(&hv)
	assert.Equal(t, api.SIMULATED, hv.ValueDomain)
	require.Len(t, hv.Chains, 2)
	assert.Equal(t, "nodal-sandbox", hv.Chains[0].Chain)
	assert.Equal(t, "400000000", hv.Chains[0].Holdings[0].Quantity)
	assert.Equal(t, "sbx-ethereum", hv.Chains[1].Chain)
	assert.Equal(t, "SBX-PUSD.E", hv.Chains[1].Holdings[0].Symbol)
	require.Len(t, hv.InFlight, 1)
	assert.Equal(t, "50.00", hv.InFlight[0].MarkedUsd, "USD as an exact two-decimal string")
	assert.Equal(t, 0, hv.InFlight[0].FactorBps, "in flight counts nothing under the default policy")
	assert.Equal(t, 6, hv.InFlight[0].Decimals)
	require.Len(t, hv.Legs, 1)
	assert.Equal(t, api.BridgeLegStatusMessageInFlight, hv.Legs[0].Status)
	assert.Equal(t, api.BridgeLegHoldingBridgeInFlight, hv.Legs[0].Holding)
	assert.Equal(t, "SBX-PUSD", hv.Legs[0].Symbol)
	assert.Equal(t, "25.00", hv.PendingLocksUsd)
	assert.Equal(t, "bridge-haircut-v1", hv.Policy.Version)

	res = h.do(http.MethodGet, bridgeBase+"/holdings?account_id="+testOtherAcct.String(), nil)
	require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())

	// The bridge withholds its figures when the legs and the ledger disagree.
	fp.err = errs.New(errs.CodeReconciliationRequired, "bridge: legs in flight disagree with the ledger; buying power is withheld")
	res = h.do(http.MethodGet, bridgeBase+"/holdings?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeReconciliationRequired, res.problem().Code)
}

func TestBridge_Chains_ReportBothChains(t *testing.T) {
	t.Parallel()
	h, _ := newBridgeHarness(t)
	res := h.do(http.MethodGet, bridgeBase+"/chains", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var c api.BridgeChains
	res.json(&c)
	assert.Equal(t, api.SIMULATED, c.ValueDomain)
	assert.Equal(t, "httpapi-sbx", c.Network)
	assert.True(t, c.FaultInjectionAllowed)
	require.Len(t, c.Chains, 2)
	assert.Equal(t, api.BridgeChainIdSbxEthereum, c.Chains[0].Id)
	assert.Equal(t, int64(90), c.Chains[1].TipHeight)
	assert.Equal(t, int64(58), c.Chains[1].FinalizedHeight)
	assert.Equal(t, "5000", c.Chains[1].FeePerTx)
	require.NotNil(t, c.Chains[1].BridgeAuthority)
}

// Fault injection exists only where the environment allows fake providers:
// anywhere else the route is refused before the host is reached.
func TestBridge_FaultInjection_IsRefusedOutsideLocalTestAndDev(t *testing.T) {
	t.Parallel()
	for _, env := range []config.Environment{config.EnvStaging, config.EnvProd} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			h, fp := newBridgeHarness(t)
			op := stableOperator()
			h.as(&op)
			srv, err := New(Options{Env: env, Clock: h.server.clk, Authenticator: h.server.opts.Authenticator, Ports: h.server.opts.Ports})
			require.NoError(t, err)
			h.server = srv
			res := h.do(http.MethodPost, bridgeBase+"/chains/sbx-solana/faults", map[string]any{"kind": "TIMEOUT_BUT_LAND"},
				"Idempotency-Key", "bridge-fault-"+string(env))
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeForbidden, p.Code)
			assert.Equal(t, devnethost.ReasonFaultInjectionForbidden, p.Fields["reason"])
			assert.Zero(t, fp.count("InjectFault"), "the host is never asked")
		})
	}
	for _, env := range []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev} {
		t.Run(string(env), func(t *testing.T) {
			t.Parallel()
			h, fp := newBridgeHarness(t)
			op := stableOperator()
			h.as(&op)
			srv, err := New(Options{Env: env, Clock: h.server.clk, Authenticator: h.server.opts.Authenticator, Ports: h.server.opts.Ports})
			require.NoError(t, err)
			h.server = srv
			res := h.do(http.MethodPost, bridgeBase+"/chains/sbx-ethereum/faults", map[string]any{"kind": "DROP_NEXT", "count": 2},
				"Idempotency-Key", "bridge-fault-"+string(env))
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, devnethost.ChainEthereum, fp.lastChain)
			assert.Equal(t, BridgeFault{Kind: BridgeFaultDropNext, Count: 2}, fp.lastFault)
			var out api.BridgeFaultResult
			res.json(&out)
			assert.Equal(t, api.BridgeFaultKindDropNext, out.Kind)
			assert.Equal(t, api.SIMULATED, out.ValueDomain)
		})
	}
}

func TestBridge_ChainCommands_ValidateTheirInput(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	op := stableOperator()
	h.as(&op)
	for i, tc := range []struct {
		path string
		body any
	}{
		{bridgeBase + "/chains/sbx-bitcoin/blocks", map[string]any{"count": 1}},
		{bridgeBase + "/chains/sbx-solana/blocks", map[string]any{"count": 0}},
		{bridgeBase + "/chains/sbx-solana/blocks", map[string]any{"count": 1001}},
		{bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "MELT"}},
		{bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "DROP_NEXT"}},
		{bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "DROP_NEXT", "count": 0}},
		{bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "DELAY_INCLUSION", "count": 1001}},
		{bridgeBase + "/chains/sbx-solana/faults", map[string]any{"kind": "TIMEOUT_BUT_LAND", "count": 1}},
	} {
		res := h.do(http.MethodPost, tc.path, tc.body, "Idempotency-Key", "bridge-chain-bad-"+itoa(i))
		require.Equal(t, http.StatusBadRequest, res.Code, "%s %v body=%s", tc.path, tc.body, res.Body.String())
		assert.Equal(t, errs.CodeValidationFailed, res.problem().Code)
	}
	assert.Zero(t, fp.count("ProduceBlocks")+fp.count("InjectFault"))

	res := h.do(http.MethodPost, bridgeBase+"/chains/sbx-solana/blocks", map[string]any{"count": 40}, "Idempotency-Key", "bridge-blocks-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, devnethost.ChainSolana, fp.lastChain)
	assert.Equal(t, uint64(40), fp.lastBlocks)
	res = h.do(http.MethodPost, bridgeBase+"/chains/sbx-solana/faults", map[string]any{"kind": "DELAY_INCLUSION", "count": 0},
		"Idempotency-Key", "bridge-delay-0001")
	require.Equal(t, http.StatusOK, res.Code, "zero restores immediate inclusion; body=%s", res.Body.String())
	assert.Equal(t, BridgeFault{Kind: BridgeFaultDelayInclusion}, fp.lastFault)
}

func TestBridge_EveryMutationReplaysItsFirstAnswer(t *testing.T) {
	t.Parallel()
	for _, m := range bridgeMutations() {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newBridgeHarness(t)
			if !m.customer {
				op := stableOperator()
				h.as(&op)
			}
			key := "bridge-replay-" + m.name
			first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Less(t, first.Code, 300, "body=%s", first.Body.String())
			second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
			require.Equal(t, first.Code, second.Code, "body=%s", second.Body.String())
			assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers exactly what the first call did")
			assert.Equal(t, 1, fp.count(m.call), "a replay never reaches the service")
			assert.Equal(t, "SIMULATED", first.raw()["value_domain"], "every answer states its value domain")

			reuse := h.do(http.MethodPost, m.path, m.other, "Idempotency-Key", key)
			require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
			assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
			assert.Equal(t, 1, fp.count(m.call))
		})
	}
}

func TestBridge_OperatorRoutes_RefuseCustomersAndStaleSessions(t *testing.T) {
	t.Parallel()
	for _, m := range bridgeMutations() {
		if m.customer {
			continue
		}
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, fp := newBridgeHarness(t)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "bridge-customer-"+m.name)
			require.Equal(t, http.StatusForbidden, res.Code, "a customer reached %s: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)

			stale := stableOperator()
			stale.AuthTime = testNow.Add(-2 * stepUpMaxAge)
			h.as(&stale)
			res = h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "bridge-stale-"+m.name)
			if !m.stepUp {
				require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
				return
			}
			require.Equal(t, http.StatusForbidden, res.Code, "%s without a recent step-up: %s", m.name, res.Body.String())
			assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
			assert.Zero(t, fp.count(m.call))
		})
	}
}

func TestBridge_CreateRoute_TranslatesTheDefinition(t *testing.T) {
	t.Parallel()
	h, fp := newBridgeHarness(t)
	op := stableOperator()
	h.as(&op)
	res := h.do(http.MethodPost, bridgeBase+"/routes", bridgeRouteBody("pusd-solana-to-ethereum"), "Idempotency-Key", "bridge-route-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	def := fp.lastDef
	assert.Equal(t, "pusd-solana-to-ethereum", def.Name)
	assert.Equal(t, stableTestUnits, def.SourceAssetID)
	assert.Equal(t, devnethost.ChainEthereum, def.DestinationChain)
	assert.Equal(t, "1", def.UnitPrice.Mantissa.String())
	assert.Equal(t, int32(0), def.UnitPrice.Scale)
	assert.Equal(t, 3, def.MaxReleaseAttempts)
	assert.Equal(t, econ.Actor{Type: security.ActorOperator, ID: stableTestOperator.String()}, fp.lastActor)

	for i, change := range []func(map[string]any){
		func(b map[string]any) { b["unit_price"] = "a dollar" },
		func(b map[string]any) { b["destination_chain"] = "sbx-bitcoin" },
		func(b map[string]any) { b["source_asset_id"] = "nope" },
	} {
		b := bridgeRouteBody("pusd-solana-to-ethereum")
		change(b)
		res := h.do(http.MethodPost, bridgeBase+"/routes", b, "Idempotency-Key", "bridge-route-bad-"+itoa(i))
		require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	}
	assert.Equal(t, 1, fp.count("CreateRoute"))

	res = h.do(http.MethodPost, bridgeRoutePath+"/reconciliations", nil, "Idempotency-Key", "bridge-recon-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	var rec api.BridgeRouteReconciliation
	res.json(&rec)
	assert.True(t, rec.Matched)
	assert.True(t, rec.BooksConserve)
	assert.Equal(t, "100000000", rec.Locked)
	assert.Equal(t, "100000000", rec.Escrow)
	assert.Equal(t, api.SIMULATED, rec.ValueDomain)

	res = h.do(http.MethodPost, bridgeRoutePath+"/pause", map[string]any{"reason": "the destination is being investigated"}, "Idempotency-Key", "bridge-pause-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, bridge.RoutePaused, fp.lastStatus)
	assert.Equal(t, "the destination is being investigated", fp.lastReason)
}
