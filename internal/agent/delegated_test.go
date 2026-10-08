package agent

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/strategy/ir"
)

// paperAuthority is a BACKTEST_ELIGIBLE agent running PAPER with the test
// strategy (which declares CREATE_TRADE_INTENT) and no envelope.
func paperAuthority(t *testing.T) Authority {
	t.Helper()
	return authorityAt(t, StageBacktestEligible, ModePaper, "", testIR())
}

func authorityAt(t *testing.T, stage Stage, mode Mode, envelopeID string, doc *ir.IR) Authority {
	t.Helper()
	a, err := NewAuthority(AuthorityInput{
		AgentID: NewAgentID(), AgentVersion: 1, AccountID: id.New[id.Any]().String(), StrategyVersionID: "sv",
		Stage: stage, Mode: mode, EnvelopeID: envelopeID, IR: doc,
	})
	require.NoError(t, err)
	return a
}

// grantTo is a valid, hashed capability granted to a's agent on a's account
// at nowForTest, for a day, with the given action classes.
func grantTo(t *testing.T, a Authority, actions ...delegation.ActionClass) delegation.Capability {
	t.Helper()
	account, err := accounts.ParseAccountID(a.AccountID())
	require.NoError(t, err)
	cid, err := delegation.ParseCapabilityID(id.New[id.Any]().String())
	require.NoError(t, err)
	c := delegation.Capability{
		ID: cid, GrantedAt: nowForTest(), Nonce: strings.Repeat("ab", 32), IdempotencyKey: "grant-" + cid.String(),
		Status:    delegation.CapabilityActive,
		GrantedBy: delegation.Actor{Type: security.ActorUser, ID: id.New[id.Any]().String()},
		Terms: delegation.CapabilityTerms{
			AccountID: account, AgentID: a.AgentID().String(), StrategyID: id.New[id.Any]().String(),
			// An agent tree never imports internal/gates, tests included
			// (test/security/authority_boundary_test.go): the mode is named
			// by its value.
			Environment: config.EnvTest, Mode: "SANDBOX",
			InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
			ActionClasses:     actions,
			MaxNotional:       money.USDFromMinor(1_000_000), MaxDailyNotional: money.USDFromMinor(500_000),
			MaxSingleTrade:       money.USDFromMinor(100_000),
			DestinationAllowlist: []devnet.PublicKey{devnet.NewKeypairFromSeed([32]byte{7}).PublicKey()},
			ChainAllowlist:       []devnet.ChainID{devnethost.ChainSolana}, ExpiresAt: nowForTest().Add(24 * time.Hour),
		},
	}
	c.Terms = c.Terms.Normalize()
	h, err := c.ComputeContentHash()
	require.NoError(t, err)
	c.ContentHash = h
	return c
}

var allDelegatedActions = []delegation.ActionClass{
	delegation.ActionAcquireExposure, delegation.ActionReduceExposure, delegation.ActionProposeWithdrawal,
}

// TestDelegatedToolsAreOfferedOnlyUnderAUsableCapabilityInAPaperRun is the
// availability rule: a PAPER run with no envelope, whose strategy declares
// CREATE_TRADE_INTENT, holds a tool per kind its agent's own ACTIVE,
// unexpired, intact capability on its account grants -- and nothing else
// does.
func TestDelegatedToolsAreOfferedOnlyUnderAUsableCapabilityInAPaperRun(t *testing.T) {
	t.Parallel()
	a := paperAuthority(t)
	now := nowForTest().Add(time.Hour)
	c := grantTo(t, a, allDelegatedActions...)

	ts := DelegatedToolsFor(a, []delegation.Capability{c}, now)
	require.Len(t, ts.Tools, 2, "%+v", ts)
	assert.Empty(t, ts.Reason)
	kinds := []ir.ActionKind{ts.Tools[0].Kind, ts.Tools[1].Kind}
	assert.ElementsMatch(t, DelegatedActions(), kinds)
	swap, ok := ts.Lookup(ActionRequestDelegatedSwap, strings.ToUpper(c.ID.String()))
	require.True(t, ok, "a capability id is matched whatever its case")
	assert.Equal(t, []string{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"}, swap.Actions)
	assert.Equal(t, c.ID.String(), swap.CapabilityID)
	assert.Equal(t, "SANDBOX", swap.Mode)
	assert.Equal(t, c.Terms.MaxSingleTrade, swap.MaxSingleTradeUSD)
	assert.Equal(t, c.Terms.ExpiresAt, swap.ExpiresAt)
	w, ok := ts.Lookup(ActionProposeDelegatedWithdrawal, c.ID.String())
	require.True(t, ok)
	assert.Equal(t, []string{"PROPOSE_WITHDRAWAL"}, w.Actions)

	t.Run("each kind follows the grant", func(t *testing.T) {
		only := DelegatedToolsFor(a, []delegation.Capability{grantTo(t, a, delegation.ActionProposeWithdrawal)}, now)
		require.Len(t, only.Tools, 1)
		assert.Equal(t, ActionProposeDelegatedWithdrawal, only.Tools[0].Kind)
		reduce := DelegatedToolsFor(a, []delegation.Capability{grantTo(t, a, delegation.ActionReduceExposure)}, now)
		require.Len(t, reduce.Tools, 1)
		assert.Equal(t, ActionRequestDelegatedSwap, reduce.Tools[0].Kind)
	})

	none := func(t *testing.T, ts DelegatedToolset, reason string) {
		t.Helper()
		assert.Empty(t, ts.Tools)
		assert.Equal(t, reason, ts.Reason)
		assert.NotEmpty(t, ts.Detail)
	}
	t.Run("without a capability", func(t *testing.T) {
		none(t, DelegatedToolsFor(a, nil, now), DelegatedReasonNoCapability)
	})
	t.Run("revoked", func(t *testing.T) {
		r := c
		r.Status = delegation.CapabilityRevoked
		none(t, DelegatedToolsFor(a, []delegation.Capability{r}, now), DelegatedReasonNoCapability)
	})
	t.Run("expired", func(t *testing.T) {
		none(t, DelegatedToolsFor(a, []delegation.Capability{c}, c.Terms.ExpiresAt), DelegatedReasonNoCapability)
	})
	t.Run("not yet granted", func(t *testing.T) {
		none(t, DelegatedToolsFor(a, []delegation.Capability{c}, c.GrantedAt.Add(-time.Second)), DelegatedReasonNoCapability)
	})
	t.Run("tampered", func(t *testing.T) {
		x := c
		x.Terms.MaxSingleTrade = money.USDFromMinor(x.Terms.MaxSingleTrade.Minor() * 10)
		none(t, DelegatedToolsFor(a, []delegation.Capability{x}, now), DelegatedReasonNoCapability)
	})
	t.Run("another agent's", func(t *testing.T) {
		other := paperAuthority(t)
		none(t, DelegatedToolsFor(a, []delegation.Capability{grantTo(t, other, allDelegatedActions...)}, now), DelegatedReasonNoCapability)
	})
	t.Run("another account's", func(t *testing.T) {
		x := c
		acct, err := accounts.ParseAccountID(id.New[id.Any]().String())
		require.NoError(t, err)
		x.Terms.AccountID = acct
		h, err := x.ComputeContentHash()
		require.NoError(t, err)
		x.ContentHash = h
		none(t, DelegatedToolsFor(a, []delegation.Capability{x}, now), DelegatedReasonNoCapability)
	})
	for _, tc := range []struct {
		name  string
		stage Stage
		mode  Mode
	}{
		{"a BACKTEST replay", StageBacktestEligible, ModeBacktest},
		{"a SHADOW run", StageShadow, ModeShadow},
		{"a CANARY run", StageCanary, ModeCanary},
		{"a LIMITED run", StageLimited, ModeLimited},
		{"a LIVE run", StageLive, ModeLive},
	} {
		t.Run("in "+tc.name, func(t *testing.T) {
			envelope := ""
			if tc.stage.RealCapital() {
				envelope = "env-1"
			}
			b := authorityAt(t, tc.stage, tc.mode, envelope, testIR())
			none(t, DelegatedToolsFor(b, []delegation.Capability{grantTo(t, b, allDelegatedActions...)}, now), DelegatedReasonModeNotAdmitted)
		})
	}
	t.Run("bound to an envelope", func(t *testing.T) {
		b := authorityAt(t, StageBacktestEligible, ModePaper, "env-1", testIR())
		none(t, DelegatedToolsFor(b, []delegation.Capability{grantTo(t, b, allDelegatedActions...)}, now), DelegatedReasonEnvelopeBound)
	})
	t.Run("a strategy that may not propose", func(t *testing.T) {
		doc := testIR()
		doc.Effects = []ir.Effect{ir.EffectReadMarketData, ir.EffectCallModel, ir.EffectCommitPrediction}
		b := authorityAt(t, StageBacktestEligible, ModePaper, "", doc)
		none(t, DelegatedToolsFor(b, []delegation.Capability{grantTo(t, b, allDelegatedActions...)}, now), DelegatedReasonEffectNotDeclared)
	})
}

func swapTool(t *testing.T) DelegatedTool {
	t.Helper()
	return DelegatedTool{Kind: ActionRequestDelegatedSwap, CapabilityID: id.New[id.Any]().String(),
		Actions: []string{"ACQUIRE_EXPOSURE", "REDUCE_EXPOSURE"}}
}

func validSwap(tool DelegatedTool) DelegatedSwapRequest {
	return DelegatedSwapRequest{CapabilityID: tool.CapabilityID, Action: "ACQUIRE_EXPOSURE", VenueID: id.New[id.Any]().String(),
		PayAssetID: id.New[id.Any]().String(), ReceiveAssetID: id.New[id.Any]().String(),
		AmountIn: money.QuantityFromInt64(2_000_000), MaxSlippageBPS: 100, Jurisdiction: "US-CA"}
}

func u64Overflow() money.Quantity {
	q, err := money.ParseQuantity(new(big.Int).Lsh(big.NewInt(1), 64).String())
	if err != nil {
		panic(err)
	}
	return q
}

// TestDelegatedSwapArgumentsAreValidatedBeforeDelegationSeesThem: every
// malformed field is refused VALIDATION_FAILED and named, and a valid request
// becomes exactly delegation's request with the runtime's account, nonce and
// correlation -- and the swap's label passed through, never interpreted.
func TestDelegatedSwapArgumentsAreValidatedBeforeDelegationSeesThem(t *testing.T) {
	t.Parallel()
	tool := swapTool(t)
	account, err := accounts.ParseAccountID(id.New[id.Any]().String())
	require.NoError(t, err)

	r := validSwap(tool)
	got, err := swapRequestFor(r, tool, account, 42, "corr-1")
	require.NoError(t, err)
	assert.Equal(t, tool.CapabilityID, got.CapabilityID.String())
	assert.Equal(t, account, got.AccountID)
	assert.Equal(t, uint64(42), got.Nonce)
	assert.Equal(t, "corr-1", got.CorrelationID)
	assert.Equal(t, delegation.ActionAcquireExposure, got.Action)
	assert.Equal(t, r.VenueID, got.VenueID.String())
	assert.Equal(t, r.PayAssetID, got.PayAssetID.String())
	assert.Equal(t, r.ReceiveAssetID, got.ReceiveAssetID.String())
	assert.Equal(t, "2000000", got.AmountIn.String())
	assert.Equal(t, money.BPS(100), got.MaxSlippageBPS)

	reduce := r
	reduce.Action = "REDUCE_EXPOSURE"
	got, err = swapRequestFor(reduce, DelegatedTool{Kind: ActionRequestDelegatedSwap, CapabilityID: tool.CapabilityID}, account, 42, "corr-1")
	require.NoError(t, err, "the label is passed through whatever the tool lists; delegation's admission judges it")
	assert.Equal(t, delegation.ActionReduceExposure, got.Action)

	for _, tc := range []struct {
		name   string
		mutate func(*DelegatedSwapRequest, *DelegatedTool, *string)
		field  string
	}{
		{"not a swap label", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.Action = "PROPOSE_WITHDRAWAL" }, "action"},
		{"no label", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.Action = "" }, "action"},
		{"a withdrawal tool", func(_ *DelegatedSwapRequest, tl *DelegatedTool, _ *string) {
			tl.Kind = ActionProposeDelegatedWithdrawal
		}, "capability_id"},
		{"a venue that is not an id", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.VenueID = "venue" }, "venue_id"},
		{"a pay asset that is not an id", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.PayAssetID = "" }, "pay_asset_id"},
		{"a receive asset that is not an id", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.ReceiveAssetID = "x" }, "receive_asset_id"},
		{"one asset both ways", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.ReceiveAssetID = r.PayAssetID }, "receive_asset_id"},
		{"nothing to pay", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.AmountIn = money.QuantityFromInt64(0) }, "amount_in"},
		{"a negative amount", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.AmountIn = money.QuantityFromInt64(-1) }, "amount_in"},
		{"an amount past u64", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.AmountIn = u64Overflow() }, "amount_in"},
		{"a negative slippage bound", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.MaxSlippageBPS = -1 }, "max_slippage_bps"},
		{"a slippage bound past 100%", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) { r.MaxSlippageBPS = 10_001 }, "max_slippage_bps"},
		{"a jurisdiction past 32 bytes", func(r *DelegatedSwapRequest, _ *DelegatedTool, _ *string) {
			r.Jurisdiction = strings.Repeat("J", 33)
		}, "jurisdiction"},
		{"a correlation past 200 bytes", func(_ *DelegatedSwapRequest, _ *DelegatedTool, c *string) { *c = strings.Repeat("c", 201) }, "correlation_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, tl, corr := validSwap(tool), tool, "corr-1"
			tc.mutate(&req, &tl, &corr)
			_, err := swapRequestFor(req, tl, account, 42, corr)
			requireFieldRefusal(t, err, tc.field)
		})
	}
}

// TestDelegatedWithdrawalArgumentsAreValidatedBeforeDelegationSeesThem.
func TestDelegatedWithdrawalArgumentsAreValidatedBeforeDelegationSeesThem(t *testing.T) {
	t.Parallel()
	tool := DelegatedTool{Kind: ActionProposeDelegatedWithdrawal, CapabilityID: id.New[id.Any]().String(), Actions: []string{"PROPOSE_WITHDRAWAL"}}
	account, err := accounts.ParseAccountID(id.New[id.Any]().String())
	require.NoError(t, err)
	valid := func() DelegatedWithdrawalRequest {
		return DelegatedWithdrawalRequest{CapabilityID: tool.CapabilityID, DestinationID: id.New[id.Any]().String(),
			AssetID: id.New[id.Any]().String(), Amount: money.QuantityFromInt64(5_000_000), Jurisdiction: "US-CA"}
	}
	r := valid()
	got, err := withdrawalRequestFor(r, tool, account, 7, "corr-2")
	require.NoError(t, err)
	assert.Equal(t, tool.CapabilityID, got.CapabilityID.String())
	assert.Equal(t, account, got.AccountID)
	assert.Equal(t, uint64(7), got.Nonce)
	assert.Equal(t, r.DestinationID, got.DestinationID.String())
	assert.Equal(t, r.AssetID, got.AssetID.String())
	assert.Equal(t, "5000000", got.Amount.String())
	assert.Equal(t, "corr-2", got.CorrelationID)

	for _, tc := range []struct {
		name   string
		mutate func(*DelegatedWithdrawalRequest, *DelegatedTool)
		field  string
	}{
		{"a swap tool", func(_ *DelegatedWithdrawalRequest, tl *DelegatedTool) { tl.Kind = ActionRequestDelegatedSwap }, "capability_id"},
		{"a destination that is not an id", func(r *DelegatedWithdrawalRequest, _ *DelegatedTool) { r.DestinationID = "cold wallet" }, "destination_id"},
		{"an asset that is not an id", func(r *DelegatedWithdrawalRequest, _ *DelegatedTool) { r.AssetID = "" }, "asset_id"},
		{"nothing to withdraw", func(r *DelegatedWithdrawalRequest, _ *DelegatedTool) { r.Amount = money.QuantityFromInt64(0) }, "amount"},
		{"an amount past u64", func(r *DelegatedWithdrawalRequest, _ *DelegatedTool) { r.Amount = u64Overflow() }, "amount"},
		{"a jurisdiction past 32 bytes", func(r *DelegatedWithdrawalRequest, _ *DelegatedTool) { r.Jurisdiction = strings.Repeat("J", 33) }, "jurisdiction"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, tl := valid(), tool
			tc.mutate(&req, &tl)
			_, err := withdrawalRequestFor(req, tl, account, 7, "corr-2")
			requireFieldRefusal(t, err, tc.field)
		})
	}
}

func requireFieldRefusal(t *testing.T, err error, field string) {
	t.Helper()
	require.Error(t, err)
	assert.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	e, ok := errs.As(err)
	require.True(t, ok)
	assert.Contains(t, e.Fields, field, "%v", e.Fields)
	assert.Equal(t, DelegatedReasonInvalidRequest, e.Fields["reason"])
}

// TestDelegatedNonceIsTheRunsAndTheActionsAlone: the nonce is a function of
// the run and the action -- so a re-run is the same request -- positive, and
// within int64 for every reader of the numeric column.
func TestDelegatedNonceIsTheRunsAndTheActionsAlone(t *testing.T) {
	t.Parallel()
	run := NewRunID()
	n := DelegatedNonce(run, "rebalance")
	assert.Equal(t, n, DelegatedNonce(run, "rebalance"))
	assert.NotEqual(t, n, DelegatedNonce(run, "rebalance-2"))
	assert.NotEqual(t, n, DelegatedNonce(NewRunID(), "rebalance"))
	seen := map[uint64]bool{}
	for i := 0; i < 1000; i++ {
		v := DelegatedNonce(NewRunID(), "a")
		assert.Positive(t, v)
		assert.LessOrEqual(t, v, uint64(math.MaxInt64))
		assert.False(t, seen[v])
		seen[v] = true
	}
}

// fakeAuthority stands in for delegation's admission facade and counts every
// call: a refusal the runtime makes must leave it untouched.
type fakeAuthority struct {
	calls   int
	caps    []delegation.Capability
	capsErr error
}

func (f *fakeAuthority) RequestSwap(context.Context, pgx.Tx, delegation.SwapRequest) (delegation.SwapTicket, error) {
	f.calls++
	return delegation.SwapTicket{}, errs.New(errs.CodeInternal, "fake: not reached in a unit test")
}

func (f *fakeAuthority) ProposeWithdrawal(context.Context, pgx.Tx, delegation.ProposalRequest) (delegation.WithdrawalTicket, error) {
	f.calls++
	return delegation.WithdrawalTicket{}, errs.New(errs.CodeInternal, "fake: not reached in a unit test")
}

func (f *fakeAuthority) AgentCapabilities(context.Context, db.Querier, accounts.AccountID) ([]delegation.Capability, error) {
	f.calls++
	return f.caps, f.capsErr
}

func delegatedEmitterFor(t *testing.T, a Authority, fa *fakeAuthority) *DelegatedEmitter {
	t.Helper()
	e, err := NewDelegatedEmitter(DelegatedEmitterDeps{Clock: clock.NewFake(nowForTest().Add(time.Hour)), Authority: fa,
		Pauses: NewPauseChecker(), Budgets: NewBudgetReader()}, a)
	require.NoError(t, err)
	return e
}

// TestNewDelegatedEmitterRefusesEveryMissingDependency, as NewEmitter does.
func TestNewDelegatedEmitterRefusesEveryMissingDependency(t *testing.T) {
	t.Parallel()
	a := paperAuthority(t)
	full := func() DelegatedEmitterDeps {
		return DelegatedEmitterDeps{Clock: clock.NewFake(nowForTest()), Authority: &fakeAuthority{},
			Pauses: NewPauseChecker(), Budgets: NewBudgetReader()}
	}
	e, err := NewDelegatedEmitter(full(), a)
	require.NoError(t, err)
	require.NotNil(t, e)
	for name, blank := range map[string]func(*DelegatedEmitterDeps){
		"clock":               func(d *DelegatedEmitterDeps) { d.Clock = nil },
		"delegated authority": func(d *DelegatedEmitterDeps) { d.Authority = nil },
		"pause checker":       func(d *DelegatedEmitterDeps) { d.Pauses = nil },
		"budget reader":       func(d *DelegatedEmitterDeps) { d.Budgets = nil },
	} {
		t.Run(name, func(t *testing.T) {
			deps := full()
			blank(&deps)
			out, err := NewDelegatedEmitter(deps, a)
			require.Errorf(t, err, "a nil %s was accepted", name)
			assert.Nil(t, out)
			assert.Contains(t, err.Error(), name)
		})
	}
	_, err = NewDelegatedEmitter(full(), Authority{})
	require.Error(t, err, "an emitter is bound to a frozen authority")
}

// TestDelegatedRequestsTheRuntimeRefusesNeverReachDelegation: every refusal
// the runtime makes -- who is asking, what is linked, the run's mode, the
// tool the run does not hold, a malformed request -- is a typed error, and
// delegation is not called at all. (Pause, budget and delegation's own
// refusals need the database; the integration tests drive them.)
func TestDelegatedRequestsTheRuntimeRefusesNeverReachDelegation(t *testing.T) {
	t.Parallel()
	a := paperAuthority(t)
	c := grantTo(t, a, allDelegatedActions...)
	toolset := DelegatedToolsFor(a, []delegation.Capability{c}, nowForTest().Add(time.Hour))
	require.Len(t, toolset.Tools, 2)
	asAgent := security.WithPrincipal(context.Background(), AgentPrincipal(a))
	call := func() DelegatedCall {
		r := validSwap(DelegatedTool{CapabilityID: c.ID.String()})
		return DelegatedCall{RunID: NewRunID(), ActionName: "rebalance", PredictionID: id.New[id.Any]().String(),
			CorrelationID: "corr-3", Kind: ActionRequestDelegatedSwap, Swap: &r}
	}

	for _, tc := range []struct {
		name   string
		auth   Authority
		ctx    context.Context
		tools  DelegatedToolset
		mutate func(*DelegatedCall)
		code   errs.Code
		reason string
	}{
		{name: "no principal", auth: a, ctx: context.Background(), tools: toolset, code: errs.CodeUnauthenticated},
		{name: "a person", auth: a, tools: toolset, code: errs.CodeForbidden,
			ctx: security.WithPrincipal(context.Background(), security.Principal{SubjectID: "u-1", ActorType: security.ActorUser,
				Roles: []security.Role{security.RoleCustomer}, AccountIDs: []string{a.AccountID()}})},
		{name: "another agent", auth: a, tools: toolset, code: errs.CodeForbidden,
			ctx: security.WithPrincipal(context.Background(), security.AgentPrincipal(NewAgentID().String(), a.AccountID()))},
		{name: "this agent on another account", auth: a, tools: toolset, code: errs.CodeForbidden,
			ctx: security.WithPrincipal(context.Background(), security.AgentPrincipal(a.AgentID().String(), id.New[id.Any]().String()))},
		{name: "no run", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed, reason: DelegatedReasonInvalidRequest,
			mutate: func(c *DelegatedCall) { c.RunID = RunID{} }},
		{name: "no action", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed, reason: DelegatedReasonInvalidRequest,
			mutate: func(c *DelegatedCall) { c.ActionName = "" }},
		{name: "no prediction before it", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed,
			reason: DelegatedReasonPredictionRequired, mutate: func(c *DelegatedCall) { c.PredictionID = "" }},
		{name: "a tool the run does not hold", auth: a, ctx: asAgent, tools: DelegatedToolset{Reason: DelegatedReasonNoCapability},
			code: errs.CodeEffectForbidden, reason: DelegatedReasonToolUnavailable},
		{name: "a capability the run holds no swap tool under", auth: a, ctx: asAgent, tools: toolset,
			code: errs.CodeEffectForbidden, reason: DelegatedReasonToolUnavailable,
			mutate: func(c *DelegatedCall) { c.Swap.CapabilityID = id.New[id.Any]().String() }},
		{name: "a swap carrying a withdrawal", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed,
			reason: DelegatedReasonInvalidRequest, mutate: func(c *DelegatedCall) {
				c.Withdrawal = &DelegatedWithdrawalRequest{CapabilityID: c.Swap.CapabilityID}
			}},
		{name: "a withdrawal carrying a swap", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed,
			reason: DelegatedReasonInvalidRequest, mutate: func(c *DelegatedCall) { c.Kind = ActionProposeDelegatedWithdrawal }},
		{name: "a kind that is not delegated", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed,
			reason: DelegatedReasonInvalidRequest, mutate: func(c *DelegatedCall) { c.Kind = ir.ActionCreateTradeIntent }},
		{name: "a malformed request", auth: a, ctx: asAgent, tools: toolset, code: errs.CodeValidationFailed,
			reason: DelegatedReasonInvalidRequest, mutate: func(c *DelegatedCall) { c.Swap.AmountIn = money.QuantityFromInt64(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fa := &fakeAuthority{}
			e := delegatedEmitterFor(t, tc.auth, fa)
			cl := call()
			if tc.mutate != nil {
				tc.mutate(&cl)
			}
			_, err := e.Propose(tc.ctx, nil, tc.tools, cl)
			require.Error(t, err)
			assert.Equal(t, tc.code, errs.CodeOf(err), "%v", err)
			if tc.reason != "" {
				e, ok := errs.As(err)
				require.True(t, ok)
				assert.Equal(t, tc.reason, e.Fields["reason"], "%v", err)
			}
			assert.Zero(t, fa.calls, "delegation never saw a request the runtime refused")
		})
	}

	t.Run("a run delegation is not admitted to", func(t *testing.T) {
		for _, b := range []Authority{
			authorityAt(t, StageShadow, ModeShadow, "", testIR()),
			authorityAt(t, StageBacktestEligible, ModeBacktest, "", testIR()),
			authorityAt(t, StageBacktestEligible, ModePaper, "env-1", testIR()),
		} {
			fa := &fakeAuthority{}
			e := delegatedEmitterFor(t, b, fa)
			cl := call()
			_, err := e.Propose(security.WithPrincipal(context.Background(), AgentPrincipal(b)), nil, toolset, cl)
			require.Error(t, err)
			assert.Equal(t, errs.CodeEffectForbidden, errs.CodeOf(err))
			ts, terr := e.Tools(security.WithPrincipal(context.Background(), AgentPrincipal(b)), nil)
			require.NoError(t, terr)
			assert.Empty(t, ts.Tools)
			assert.Zero(t, fa.calls, "not even the capability read is made for a run delegation is not admitted to")
		}
	})

	t.Run("the toolset is read through delegation's own read, as the agent", func(t *testing.T) {
		fa := &fakeAuthority{caps: []delegation.Capability{c}}
		e := delegatedEmitterFor(t, a, fa)
		ts, err := e.Tools(asAgent, nil)
		require.NoError(t, err)
		assert.Len(t, ts.Tools, 2)
		assert.Equal(t, 1, fa.calls)
		_, err = e.Tools(context.Background(), nil)
		require.Error(t, err, "no capability is read without the agent's principal")
		fa.capsErr = errs.New(errs.CodeInternal, "read failed")
		_, err = e.Tools(asAgent, nil)
		require.Error(t, err, "a failed read is an error, never an empty toolset")
	})
}

// TestRunEvidenceCarriesDelegatedRecordsAndNothingElseChanges: with nothing
// delegated the evidence is exactly the freshness checks it always was; with
// delegation, the toolset and every request are appended, and a later
// attempt reads back exactly the requests.
func TestRunEvidenceCarriesDelegatedRecordsAndNothingElseChanges(t *testing.T) {
	t.Parallel()
	checks := []FreshnessCheck{{Dependency: "price", ToolCode: "oracle", ToolVersion: 1, Present: true, Fresh: true}}
	plain := runEvidence{checks: checks}
	assert.JSONEq(t, string(mustJSON(checks)), string(plain.render()))
	assert.Empty(t, delegatedRecordsOf(plain.render()))
	assert.JSONEq(t, `[]`, string(runEvidence{checks: []FreshnessCheck{}}.render()))

	at := nowForTest()
	admitted := DelegatedRecord{Kind: EvidenceKindDelegatedCall, Action: "rebalance", Tool: string(ActionRequestDelegatedSwap),
		CapabilityID: "c-1", Nonce: "9", SubjectKind: "SWAP", SubjectID: "s-1", Status: "AWAITING_APPROVAL", IntentID: "i-1", RecordedAt: at}
	refused := DelegatedRecord{Kind: EvidenceKindDelegatedCall, Action: "cash-out", Tool: string(ActionProposeDelegatedWithdrawal)}.
		refusedBy(errs.New(errs.CodeRiskRejected, "delegation: over the limit").WithField("reason", "RISK_REJECTED"))
	full := runEvidence{checks: checks, tools: toolsRecord(DelegatedToolset{Tools: []DelegatedTool{{Kind: ActionRequestDelegatedSwap,
		CapabilityID: "c-1"}}}, at)}.with(admitted).with(refused)
	var elems []map[string]any
	require.NoError(t, json.Unmarshal(full.render(), &elems))
	require.Len(t, elems, 4)
	assert.Equal(t, "price", elems[0]["dependency"])
	assert.Equal(t, EvidenceKindDelegatedTools, elems[1]["kind"])
	back := delegatedRecordsOf(full.render())
	require.Len(t, back, 2)
	assert.Equal(t, admitted, back[0])
	assert.Equal(t, refused, back[1])
	got, ok := full.call("cash-out")
	require.True(t, ok)
	assert.True(t, got.Refused())
	_, ok = full.call("unknown")
	assert.False(t, ok)
	assert.Empty(t, delegatedRecordsOf(json.RawMessage(`not json`)))
}

// TestARecordedRefusalEndsAResumedRunTheSameWay: a refusal read back from the
// run's evidence is the same code and reason, and so the same skip or
// failure, as the one that was recorded -- the request is not made again.
func TestARecordedRefusalEndsAResumedRunTheSameWay(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		errs.New(errs.CodeRiskRejected, "delegation: authority limit").WithField("reason", "RISK_REJECTED"),
		errs.New(errs.CodeKillSwitchActive, "agent: paused").WithField("reason", DelegatedReasonAgentPaused),
		errs.New(errs.CodeEffectForbidden, "agent: no tool").WithField("reason", DelegatedReasonToolUnavailable),
		errs.New(errs.CodeRateLimited, "agent: order rate").WithField("reason", DelegatedReasonOrderRate),
		errs.New(errs.CodeNotFound, "delegation: no such destination"),
	} {
		rec := DelegatedRecord{Kind: EvidenceKindDelegatedCall, Action: "a"}.refusedBy(err)
		again := rec.refusalError()
		assert.Equal(t, errs.CodeOf(err), errs.CodeOf(again))
		assert.Equal(t, delegatedSkipReason(err), delegatedSkipReason(again))
		e, _ := errs.As(err)
		g, _ := errs.As(again)
		if want, ok := e.Fields["reason"]; ok {
			assert.Equal(t, want, g.Fields["reason"])
		}
	}
	long := DelegatedRecord{}.refusedBy(errs.New(errs.CodeRiskRejected, strings.Repeat("x", 5000)))
	assert.Len(t, long.Detail, maxRecordedDetail, "a record keeps a bounded detail")
}

// TestDelegatedRefusalsMapOntoTheRunsOwnSkipReasons: the bounds the runtime
// names skip the run; delegation's scope, limit and policy refusals fail it,
// as a refused trade intent does -- a delegated destination that is not
// found is a refusal, not a missing data dependency.
func TestDelegatedRefusalsMapOntoTheRunsOwnSkipReasons(t *testing.T) {
	t.Parallel()
	for code, want := range map[errs.Code]SkipReason{
		errs.CodeEffectForbidden:         SkipEffectMismatch,
		errs.CodeKillSwitchActive:        SkipKillSwitch,
		errs.CodeRateLimited:             SkipRateLimited,
		errs.CodeStaleMarketData:         SkipStaleData,
		errs.CodeRiskRejected:            "",
		errs.CodeForbidden:               "",
		errs.CodeNotFound:                "",
		errs.CodeValidationFailed:        "",
		errs.CodeConflict:                "",
		errs.CodeInsufficientBuyingPower: "",
	} {
		assert.Equal(t, want, delegatedSkipReason(errs.New(code, "x")), string(code))
	}
}

// TestTheRuntimeHoldsNoDelegationServiceOrSigner proves at the source level
// what DelegatedAuthority promises: the runtime's own files name delegation's
// request and answer types, its capability, its identifiers and the facade --
// and never the Service, a signer, a venue signer, a constructor, or any
// method that signs, executes, decides, grants or revokes.
func TestTheRuntimeHoldsNoDelegationServiceOrSigner(t *testing.T) {
	t.Parallel()
	used := delegationSelectorsIn(t, ".")
	allowed := map[string]bool{
		"AgentAdmission": true, "SwapRequest": true, "SwapTicket": true, "ProposalRequest": true, "WithdrawalTicket": true,
		"Capability": true, "ParseCapabilityID": true, "ParseVenueID": true, "ParseDestinationID": true, "ActionClass": true,
		"ActionProposeWithdrawal": true, "SubjectSwap": true, "SubjectWithdrawal": true,
	}
	var outside []string
	for name := range used {
		if !allowed[name] {
			outside = append(outside, name)
		}
	}
	sort.Strings(outside)
	assert.Empty(t, outside, "internal/agent names delegation identifiers beyond the admission surface")
	assert.True(t, used["AgentAdmission"], "the runtime is bound to the facade")
}

// delegationSelectorsIn returns every delegation.X a directory's non-test
// files name.
func delegationSelectorsIn(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		require.NoError(t, err)
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) == "github.com/nodal/controlplane/internal/delegation" {
				local = "delegation"
				if imp.Name != nil {
					local = imp.Name.Name
				}
			}
		}
		if local == "" {
			continue
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == local {
					out[sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	return out
}
