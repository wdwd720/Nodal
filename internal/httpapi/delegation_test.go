package httpapi

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// The delegation routes over a fake port. The fake records every call, so a
// test can prove a refused request -- an agent's, a stranger's, an
// operator's on the owner's surface, a malformed one -- never reached the
// service, and that a replay was answered without asking it again.

// dgID decodes a fixed canonical id into a typed one: the delegation package
// mints its ids only when it persists a row.
func dgID[T any, PT interface {
	*T
	UnmarshalText([]byte) error
}](s string) T {
	var v T
	if err := PT(&v).UnmarshalText([]byte(s)); err != nil {
		panic(err)
	}
	return v
}

const (
	dgNilUUID    = "00000000-0000-0000-0000-000000000000"
	dgAbsentID   = "0193b2e0-0000-7000-8000-00000000d0ff"
	dgAgentID    = "0193b2e0-0000-7000-8000-00000000d006"
	dgStrategyID = "0193b2e0-0000-7000-8000-00000000d007"
	dgUserID     = "0193b2e0-0000-7000-8000-00000000d008"
)

var (
	dgCapabilityID   = dgID[delegation.CapabilityID]("0193b2e0-0000-7000-8000-00000000d001")
	dgSwapID         = dgID[delegation.SwapID]("0193b2e0-0000-7000-8000-00000000d002")
	dgWithdrawalID   = dgID[delegation.WithdrawalID]("0193b2e0-0000-7000-8000-00000000d003")
	dgDestinationID  = dgID[delegation.DestinationID]("0193b2e0-0000-7000-8000-00000000d004")
	dgVenueID        = dgID[delegation.VenueID]("0193b2e0-0000-7000-8000-00000000d005")
	dgPolicyID       = dgID[delegation.PolicyID]("0193b2e0-0000-7000-8000-00000000d009")
	dgHouseID        = dgID[econ.HouseAccountID]("0193b2e0-0000-7000-8000-00000000d00a")
	dgIntentID       = dgID[econ.IntentID]("0193b2e0-0000-7000-8000-00000000d00b")
	dgUsageID        = dgID[delegation.UsageID]("0193b2e0-0000-7000-8000-00000000d00c")
	dgEffectID       = dgID[econ.EffectID]("0193b2e0-0000-7000-8000-00000000d00d")
	dgReceiveAssetID = assets.NewAssetID()
	dgVenueKey       = devnet.NewKeypairFromSeed([32]byte{1}).PublicKey()
	dgWalletKey      = devnet.NewKeypairFromSeed([32]byte{2}).PublicKey()
	dgPayoutKey      = devnet.NewKeypairFromSeed([32]byte{3}).PublicKey()
	dgPayMint        = devnet.NewKeypairFromSeed([32]byte{4}).PublicKey()
	dgReceiveMint    = devnet.NewKeypairFromSeed([32]byte{5}).PublicKey()
)

// dgPrice is a plan price exactly as a plan carries it.
func dgPrice(mantissa, scale string, par bool) delegation.PlanPrice {
	return delegation.PlanPrice{Mantissa: mantissa, Scale: scale, Source: "test-oracle", ObservedAt: testNow, Par: par}
}

func dgCapabilityView() DelegationCapabilityView {
	c := delegation.Capability{
		ID: dgCapabilityID,
		Terms: delegation.CapabilityTerms{
			AccountID: testAccountID, AgentID: dgAgentID, StrategyID: dgStrategyID, Environment: config.EnvTest, Mode: gates.ModeSandbox,
			InstrumentClasses: []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken},
			ActionClasses:     []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionProposeWithdrawal},
			MaxNotional:       usd("1000.00"), MaxDailyNotional: usd("500.00"), MaxSingleTrade: usd("100.00"),
			DestinationAllowlist: []devnet.PublicKey{dgVenueKey, dgPayoutKey}, ChainAllowlist: []devnet.ChainID{devnethost.ChainSolana},
			ExpiresAt: testNow.Add(24 * time.Hour), RequiresHumanApproval: true,
		},
		GrantedAt: testNow.Add(-time.Hour), Status: delegation.CapabilityActive,
		GrantedBy: delegation.Actor{Type: security.ActorUser, ID: testUserID.String()},
		CreatedAt: testNow.Add(-time.Hour), UpdatedAt: testNow.Add(-time.Hour),
	}
	for i := range c.ContentHash {
		c.ContentHash[i] = byte(i)
	}
	return DelegationCapabilityView{
		Capability: c, AgentName: "momentum agent",
		Usage: DelegationUsage{Day: time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC), Uses: 2, UsedToday: usd("40.00"), UsedTotal: usd("140.00")},
		Uses: []DelegationUse{
			{ID: dgUsageID.String(), SubjectKind: delegation.SubjectSwap, SubjectID: dgSwapID.String(),
				Action: delegation.ActionAcquireExposure, Notional: usd("40.00"), UsedAt: testNow.Add(-time.Minute)},
		},
		AsOf: testNow,
	}
}

func dgSwapPlan() delegation.SwapPlan {
	return delegation.SwapPlan{
		Version: "nodal-sandbox/delegated-swap-plan/v1", SwapID: dgSwapID.String(), AccountID: testAccountID.String(),
		AgentID: dgAgentID, CapabilityID: dgCapabilityID.String(), CapabilityHash: strings.Repeat("ab", 32),
		IntentID: dgIntentID.String(), RequestNonce: "7", ActionClass: "ACQUIRE_EXPOSURE", Mode: "SANDBOX",
		Chain: string(devnethost.ChainSolana), Wallet: dgWalletKey.String(), VenueID: dgVenueID.String(),
		VenueAddress: dgVenueKey.String(), PayAssetID: testAssetID.String(), PayMint: dgPayMint.String(), PayDecimals: "6",
		ReceiveAssetID: dgReceiveAssetID.String(), ReceiveMint: dgReceiveMint.String(), ReceiveDecimals: "9",
		AmountIn: "100000000", AmountOut: "995000000", MinAmountOut: "985050000",
		PayPrice: dgPrice("1", "0", true), ReceivePrice: dgPrice("10000", "2", false), SpreadBPS: "50", MaxSlippageBPS: "100",
		NotionalUSD: "100.00", FeePerTx: "10", RequestedAt: testNow, QuoteExpiresAt: testNow.Add(2 * time.Minute),
	}
}

func dgSwapView(t *testing.T) DelegationSwapView {
	t.Helper()
	plan := dgSwapPlan()
	hash, err := plan.Hash()
	require.NoError(t, err)
	return DelegationSwapView{
		Swap: delegation.Swap{
			ID: dgSwapID, CapabilityID: dgCapabilityID, AccountID: testAccountID, AgentID: dgAgentID, RequestNonce: 7,
			IntentID: dgIntentID, UsageID: dgUsageID, VenueID: dgVenueID, Mode: gates.ModeSandbox,
			Action: delegation.ActionAcquireExposure, Chain: devnethost.ChainSolana, Wallet: dgWalletKey,
			PayAssetID: testAssetID, ReceiveAssetID: dgReceiveAssetID, AmountIn: qty("100000000"), AmountOut: qty("995000000"),
			MinAmountOut: qty("985050000"), NotionalUSD: usd("100.00"), RequestedAt: testNow, QuoteExpiresAt: testNow.Add(2 * time.Minute),
			RequiresApproval: true, Plan: plan, PlanHash: hash, Status: delegation.SwapAwaitingApproval,
			CreatedAt: testNow, UpdatedAt: testNow,
		},
		Venue:        DelegationVenueFacts{ID: dgVenueID, Name: "sandbox-solana", Address: dgVenueKey.String()},
		PayAsset:     DelegationAssetFacts{ID: testAssetID, Symbol: "SBXUSD", Decimals: 6},
		ReceiveAsset: DelegationAssetFacts{ID: dgReceiveAssetID, Symbol: "SBXSOL", Decimals: 9},
		Transitions: []DelegationTransitionView{
			{From: "NONE", To: "ADMITTED", Actor: delegation.Actor{Type: security.ActorAgent, ID: dgAgentID}, Reason: "admitted", At: testNow},
			{From: "ADMITTED", To: "AWAITING_APPROVAL", Actor: delegation.Actor{Type: security.ActorService, ID: "delegation"},
				Reason: "the capability requires the owner's approval", At: testNow},
		},
	}
}

func dgWithdrawalView(t *testing.T) DelegationWithdrawalView {
	t.Helper()
	plan := delegation.WithdrawalPlan{
		Version: "nodal-sandbox/delegated-withdrawal-plan/v1", WithdrawalID: dgWithdrawalID.String(),
		AccountID: testAccountID.String(), AgentID: dgAgentID, CapabilityID: dgCapabilityID.String(),
		CapabilityHash: strings.Repeat("ab", 32), IntentID: dgIntentID.String(), ProposalNonce: "3", Mode: "SANDBOX",
		Chain: string(devnethost.ChainSolana), Wallet: dgWalletKey.String(), DestinationID: dgDestinationID.String(),
		DestinationAddress: dgPayoutKey.String(), AssetID: testAssetID.String(), Mint: dgPayMint.String(), Decimals: "6",
		Amount: "25000000", NotionalUSD: "25.00", Price: dgPrice("1", "0", true), PolicyID: dgPolicyID.String(),
		PolicyVersion: "2", PolicyHash: strings.Repeat("cd", 32), EvaluationOutcome: "HUMAN_REQUIRED", RiskScore: "30",
		FeePerTx: "10", ProposedAt: testNow, ExecutableAt: testNow, ApprovalExpiresAt: testNow.Add(24 * time.Hour),
	}
	hash, err := plan.Hash()
	require.NoError(t, err)
	return DelegationWithdrawalView{
		Withdrawal: delegation.Withdrawal{
			ID: dgWithdrawalID, CapabilityID: dgCapabilityID, AccountID: testAccountID, AgentID: dgAgentID, ProposalNonce: 3,
			IntentID: dgIntentID, UsageID: dgUsageID, PolicyID: dgPolicyID, DestinationID: dgDestinationID,
			Environment: config.EnvTest, Mode: gates.ModeSandbox, Chain: devnethost.ChainSolana, Wallet: dgWalletKey,
			DestinationAddress: dgPayoutKey, AssetID: testAssetID, Amount: qty("25000000"), NotionalUSD: usd("25.00"),
			ProposedAt: testNow, ApprovalExpiresAt: testNow.Add(24 * time.Hour), ExecutableAt: testNow,
			EvaluationOutcome: delegation.OutcomeHumanRequired, RiskScore: 30,
			Evaluation: delegation.Evaluation{Outcome: delegation.OutcomeHumanRequired, RiskScore: 30,
				ReasonCodes: []string{delegation.ReasonHumanAutoApprovalDisabled}, Anomalies: []string{},
				DestinationAge: "86400", AccountAge: "172800", DayTotalBefore: "1250", CountLast24h: "1", PolicyVersion: "2",
				PolicyHash: strings.Repeat("cd", 32), Environment: "TEST", ComplianceClear: true},
			Plan: plan, PlanHash: hash, Status: delegation.WithdrawalAwaitingApproval, CreatedAt: testNow, UpdatedAt: testNow,
		},
		Asset:         DelegationAssetFacts{ID: testAssetID, Symbol: "SBXUSD", Decimals: 6},
		Destination:   DelegationDestinationFacts{ID: dgDestinationID, Label: "cold wallet", Status: delegation.DestinationActive},
		PolicyVersion: 2,
		Transitions: []DelegationTransitionView{
			{From: "NONE", To: "PROPOSED", Actor: delegation.Actor{Type: security.ActorAgent, ID: dgAgentID}, Reason: "proposed", At: testNow},
		},
	}
}

func dgDestination() delegation.Destination {
	return delegation.Destination{ID: dgDestinationID, AccountID: testAccountID, Chain: devnethost.ChainSolana, Address: dgPayoutKey,
		Label: "cold wallet", Status: delegation.DestinationActive, AddedBy: delegation.Actor{Type: security.ActorUser, ID: testUserID.String()},
		AddedAt: testNow.Add(-48 * time.Hour)}
}

func dgPolicy() delegation.WithdrawalPolicy {
	p := delegation.WithdrawalPolicy{
		ID: dgPolicyID, AccountID: testAccountID, Version: 2, AutoApproveEnabled: false, AutoApproveMax: usd("0.00"),
		AutoApproveMaxRiskScore: 0, MaxRiskScore: 80, PerWithdrawalLimit: usd("250.00"), DailyLimit: usd("1000.00"),
		VelocityMaxCount: 10, MinDestinationAge: 0, MinAccountAge: 0, CoolingPeriod: 0, StrongAuthRequired: true,
		StrongAuthMaxAge: 15 * time.Minute, CreatedBy: delegation.Actor{Type: security.ActorUser, ID: testUserID.String()},
		EffectiveAt: testNow, CreatedAt: testNow,
	}
	for i := range p.PolicyHash {
		p.PolicyHash[i] = 0xcd
	}
	return p
}

// dgFake is the delegation port. Reads answer the fixed records; a record
// read by an id other than its own is NOT_FOUND. cmdErr fails every command,
// readErr every read.
type dgFake struct {
	mu      sync.Mutex
	calls   map[string]int
	cmdErr  error
	readErr error

	lastAccount          accounts.AccountID
	lastCursor           string
	lastLimit            int
	lastSwapStatus       delegation.SwapStatus
	lastWithdrawalStatus delegation.WithdrawalStatus
	lastCapabilityStatus delegation.CapabilityStatus
	lastGrant            delegation.GrantRequest
	lastRevoke           delegation.RevokeRequest
	lastDestination      delegation.DestinationRequest
	lastRemoved          delegation.DestinationID
	lastReason           string
	lastPolicy           delegation.WithdrawalPolicy
	lastSwapDecision     delegation.SwapDecision
	lastWithdrawDecision delegation.WithdrawalDecision
	lastVenue            DelegationVenueRegistration

	capability  DelegationCapabilityView
	destination delegation.Destination
	policies    []delegation.WithdrawalPolicy
	swap        DelegationSwapView
	withdrawal  DelegationWithdrawalView
	venues      DelegationVenuesView
	execution   DelegationExecution
	results     []DelegationExecutionResult
	next        string
}

func newDgFake(t *testing.T) *dgFake {
	t.Helper()
	cs := DelegationCoSignerView{Name: "sandbox-solana", Chain: devnethost.ChainSolana, Address: dgVenueKey, Registered: true}
	return &dgFake{
		calls: map[string]int{}, capability: dgCapabilityView(), destination: dgDestination(),
		policies: []delegation.WithdrawalPolicy{dgPolicy()}, swap: dgSwapView(t), withdrawal: dgWithdrawalView(t),
		venues: DelegationVenuesView{
			Venues: []DelegationVenueView{{Venue: delegation.Venue{ID: dgVenueID, Name: "sandbox-solana", Chain: devnethost.ChainSolana,
				Address: dgVenueKey, HouseAccountID: dgHouseID, SpreadBPS: 50,
				CreatedBy: delegation.Actor{Type: security.ActorService, ID: "delegation-venue-bootstrap"}, CreatedAt: testNow}, CoSigner: true}},
			CoSigners: []DelegationCoSignerView{cs,
				{Name: "sandbox-ethereum", Chain: devnethost.ChainEthereum, Address: dgPayoutKey, Registered: false}},
		},
		results: []DelegationExecutionResult{
			{Kind: delegation.SubjectSwap, ID: dgSwapID.String(), AccountID: testAccountID, StatusBefore: "ADMITTED", Status: "SETTLED"},
			{Kind: delegation.SubjectWithdrawal, ID: dgWithdrawalID.String(), AccountID: testAccountID, StatusBefore: "APPROVED",
				Status: "APPROVED", Err: errs.New(errs.CodeInvalidStateTransition, "delegation: the withdrawal's cooling period has not elapsed").
					WithField("reason", delegation.ReasonCoolingPeriod)},
		},
	}
}

func (f *dgFake) hit(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *dgFake) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

// commands counts every call that could change state.
func (f *dgFake) commands() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, name := range []string{"Grant", "Revoke", "AddDestination", "RemoveDestination", "SetWithdrawalPolicy", "DecideSwap",
		"DecideWithdrawal", "CancelWithdrawal", "RegisterVenue", "ExecutePending"} {
		n += f.calls[name]
	}
	return n
}

func (f *dgFake) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		n += c
	}
	return n
}

func (f *dgFake) scoped(acct accounts.AccountID, cursor string, limit int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastAccount, f.lastCursor, f.lastLimit = acct, cursor, limit
}

func (f *dgFake) Agents(_ context.Context, acct accounts.AccountID) ([]DelegationAgentView, error) {
	f.hit("Agents")
	f.scoped(acct, "", 0)
	return []DelegationAgentView{{ID: dgAgentID, Name: "momentum agent", Stage: "VALIDATED", State: "VALIDATED",
		StrategyID: dgStrategyID, CreatedAt: testNow}}, f.readErr
}

func (f *dgFake) Capabilities(_ context.Context, acct accounts.AccountID, cursor string, limit int) (DelegationCapabilityPage, error) {
	f.hit("Capabilities")
	f.scoped(acct, cursor, limit)
	return DelegationCapabilityPage{Items: []DelegationCapabilityView{f.capability}, NextCursor: f.next}, f.readErr
}

func (f *dgFake) Destinations(_ context.Context, acct accounts.AccountID) ([]delegation.Destination, error) {
	f.hit("Destinations")
	f.scoped(acct, "", 0)
	return []delegation.Destination{f.destination}, f.readErr
}

func (f *dgFake) WithdrawalPolicy(_ context.Context, acct accounts.AccountID) (DelegationPolicyView, error) {
	f.hit("WithdrawalPolicy")
	f.scoped(acct, "", 0)
	return DelegationPolicyView{AccountID: acct, Versions: f.policies}, f.readErr
}

func (f *dgFake) Swaps(_ context.Context, acct accounts.AccountID, status delegation.SwapStatus, cursor string, limit int) (DelegationSwapPage, error) {
	f.hit("Swaps")
	f.scoped(acct, cursor, limit)
	f.mu.Lock()
	f.lastSwapStatus = status
	f.mu.Unlock()
	return DelegationSwapPage{Items: []DelegationSwapView{f.swap}, NextCursor: f.next}, f.readErr
}

func (f *dgFake) Withdrawals(_ context.Context, acct accounts.AccountID, status delegation.WithdrawalStatus, cursor string, limit int) (DelegationWithdrawalPage, error) {
	f.hit("Withdrawals")
	f.scoped(acct, cursor, limit)
	f.mu.Lock()
	f.lastWithdrawalStatus = status
	f.mu.Unlock()
	return DelegationWithdrawalPage{Items: []DelegationWithdrawalView{f.withdrawal}, NextCursor: f.next}, f.readErr
}

func (f *dgFake) Capability(_ context.Context, cid delegation.CapabilityID) (DelegationCapabilityView, error) {
	f.hit("Capability")
	if cid != f.capability.Capability.ID {
		return DelegationCapabilityView{}, errs.New(errs.CodeNotFound, "delegation: capability not found")
	}
	return f.capability, f.readErr
}

func (f *dgFake) Destination(_ context.Context, did delegation.DestinationID) (delegation.Destination, error) {
	f.hit("Destination")
	if did != f.destination.ID {
		return delegation.Destination{}, errs.New(errs.CodeNotFound, "delegation: destination not found")
	}
	return f.destination, f.readErr
}

func (f *dgFake) Swap(_ context.Context, sid delegation.SwapID) (DelegationSwapView, error) {
	f.hit("Swap")
	if sid != f.swap.Swap.ID {
		return DelegationSwapView{}, errs.New(errs.CodeNotFound, "delegation: swap not found")
	}
	return f.swap, f.readErr
}

func (f *dgFake) Withdrawal(_ context.Context, wid delegation.WithdrawalID) (DelegationWithdrawalView, error) {
	f.hit("Withdrawal")
	if wid != f.withdrawal.Withdrawal.ID {
		return DelegationWithdrawalView{}, errs.New(errs.CodeNotFound, "delegation: withdrawal not found")
	}
	return f.withdrawal, f.readErr
}

func (f *dgFake) Venues(context.Context) (DelegationVenuesView, error) {
	f.hit("Venues")
	return f.venues, f.readErr
}

func (f *dgFake) Grant(_ context.Context, r delegation.GrantRequest) (DelegationCapabilityView, error) {
	f.hit("Grant")
	f.mu.Lock()
	f.lastGrant = r
	f.mu.Unlock()
	return f.capability, f.cmdErr
}

func (f *dgFake) Revoke(_ context.Context, r delegation.RevokeRequest) (DelegationCapabilityView, error) {
	f.hit("Revoke")
	f.mu.Lock()
	f.lastRevoke = r
	f.mu.Unlock()
	v := f.capability
	v.Capability.Status = delegation.CapabilityRevoked
	by := delegation.Actor{Type: security.ActorUser, ID: testUserID.String()}
	at := testNow
	v.Capability.RevokedBy, v.Capability.RevocationReason, v.Capability.RevokedAt = &by, r.Reason, &at
	return v, f.cmdErr
}

func (f *dgFake) AddDestination(_ context.Context, r delegation.DestinationRequest) (delegation.Destination, error) {
	f.hit("AddDestination")
	f.mu.Lock()
	f.lastDestination = r
	f.mu.Unlock()
	return f.destination, f.cmdErr
}

func (f *dgFake) RemoveDestination(_ context.Context, did delegation.DestinationID, reason string) (delegation.Destination, error) {
	f.hit("RemoveDestination")
	f.mu.Lock()
	f.lastRemoved, f.lastReason = did, reason
	f.mu.Unlock()
	d := f.destination
	d.Status = delegation.DestinationRemoved
	return d, f.cmdErr
}

func (f *dgFake) SetWithdrawalPolicy(_ context.Context, p delegation.WithdrawalPolicy) (delegation.WithdrawalPolicy, error) {
	f.hit("SetWithdrawalPolicy")
	f.mu.Lock()
	f.lastPolicy = p
	f.mu.Unlock()
	return f.policies[0], f.cmdErr
}

func (f *dgFake) DecideSwap(_ context.Context, d delegation.SwapDecision) (DelegationSwapOutcome, error) {
	f.hit("DecideSwap")
	f.mu.Lock()
	f.lastSwapDecision = d
	f.mu.Unlock()
	return DelegationSwapOutcome{Swap: f.swap, Execution: f.execution}, f.cmdErr
}

func (f *dgFake) DecideWithdrawal(_ context.Context, d delegation.WithdrawalDecision) (DelegationWithdrawalOutcome, error) {
	f.hit("DecideWithdrawal")
	f.mu.Lock()
	f.lastWithdrawDecision = d
	f.mu.Unlock()
	return DelegationWithdrawalOutcome{Withdrawal: f.withdrawal, Execution: f.execution}, f.cmdErr
}

func (f *dgFake) CancelWithdrawal(_ context.Context, wid delegation.WithdrawalID, reason string) (DelegationWithdrawalView, error) {
	f.hit("CancelWithdrawal")
	f.mu.Lock()
	f.lastReason = reason
	f.mu.Unlock()
	return f.withdrawal, f.cmdErr
}

func (f *dgFake) AllCapabilities(_ context.Context, status delegation.CapabilityStatus, cursor string, limit int) (DelegationCapabilityPage, error) {
	f.hit("AllCapabilities")
	f.scoped(accounts.AccountID{}, cursor, limit)
	f.mu.Lock()
	f.lastCapabilityStatus = status
	f.mu.Unlock()
	return DelegationCapabilityPage{Items: []DelegationCapabilityView{f.capability}, NextCursor: f.next}, f.readErr
}

func (f *dgFake) Pending(_ context.Context, limit int) (DelegationPendingView, error) {
	f.hit("Pending")
	f.scoped(accounts.AccountID{}, "", limit)
	return DelegationPendingView{Swaps: []DelegationSwapView{f.swap}, Withdrawals: []DelegationWithdrawalView{f.withdrawal}, AsOf: testNow},
		f.readErr
}

func (f *dgFake) RegisterVenue(_ context.Context, r DelegationVenueRegistration) (DelegationVenueView, error) {
	f.hit("RegisterVenue")
	f.mu.Lock()
	f.lastVenue = r
	f.mu.Unlock()
	return f.venues.Venues[0], f.cmdErr
}

func (f *dgFake) ExecutePending(_ context.Context, limit int) ([]DelegationExecutionResult, error) {
	f.hit("ExecutePending")
	f.scoped(accounts.AccountID{}, "", limit)
	return f.results, f.cmdErr
}

func newDelegationHarness(t *testing.T) (*harness, *dgFake) {
	t.Helper()
	h := newHarness(t)
	f := newDgFake(t)
	ports := h.ports.ports()
	ports.Delegation = f
	h.server.opts.Ports = ports
	return h, f
}

func dgStranger() security.Principal {
	p := customerPrincipal()
	p.AccountIDs = []string{testOtherAcct.String()}
	return p
}

func dgStale(p security.Principal) security.Principal {
	p.AuthTime = testNow.Add(-2 * stepUpMaxAge)
	return p
}

func dgPlanHash(t *testing.T) string {
	t.Helper()
	plan := dgSwapPlan()
	h, err := plan.Hash()
	require.NoError(t, err)
	return hex.EncodeToString(h[:])
}

func dgGrantBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "agent_id": dgAgentID, "mode": "SANDBOX",
		"instrument_classes": []string{"STABLE_VALUE", "SPOT_TOKEN"}, "action_classes": []string{"ACQUIRE_EXPOSURE", "PROPOSE_WITHDRAWAL"},
		"max_notional_usd": "1000.00", "max_daily_notional_usd": "500.00", "max_single_trade_usd": "100.00",
		"destination_allowlist": []string{dgVenueKey.String(), dgPayoutKey.String()}, "chain_allowlist": []string{"sbx-solana"},
		"expires_at": testNow.Add(24 * time.Hour).Format(time.RFC3339), "requires_human_approval": true,
	}
}

func dgPolicyBody() map[string]any {
	return map[string]any{
		"account_id": testAccountID.String(), "auto_approve_enabled": false, "auto_approve_max_usd": "0.00",
		"auto_approve_max_risk_score": 0, "max_risk_score": 80, "per_withdrawal_limit_usd": "250.00", "daily_limit_usd": "1000.00",
		"velocity_max_count": 10, "min_destination_age_seconds": 0, "min_account_age_seconds": 0, "cooling_period_seconds": 3600,
		"strong_auth_required": true, "strong_auth_max_age_seconds": 900,
	}
}

func dgWith(base map[string]any, key string, value any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	out[key] = value
	return out
}

// dgMutation is one state-changing route. byID routes are scoped by the
// record's own account, and anyone but its owner gets the NOT_FOUND an absent
// id gets.
type dgMutation struct {
	name    string
	path    string
	altPath string
	body    any
	altBody any
	counter string
	stepUp  bool
	byID    bool
}

func dgCustomerMutations(t *testing.T) []dgMutation {
	t.Helper()
	plan := dgPlanHash(t)
	acct := testAccountID.String()
	destination := map[string]any{"account_id": acct, "chain": "sbx-solana", "address": dgPayoutKey.String(), "label": "cold wallet"}
	approve := map[string]any{"decision": "APPROVE", "plan_hash": plan, "reason": "the plan is what I asked for"}
	return []dgMutation{
		{name: "grant", path: "/v1/advanced/delegation/capabilities", counter: "Grant", stepUp: true,
			body: dgGrantBody(), altBody: dgWith(dgGrantBody(), "max_single_trade_usd", "99.00")},
		{name: "revoke", path: "/v1/advanced/delegation/capabilities/" + dgCapabilityID.String() + "/revoke", counter: "Revoke",
			stepUp: true, byID: true, body: map[string]any{"reason": "no longer needed"}, altBody: map[string]any{"reason": "changed my mind"}},
		{name: "add-destination", path: "/v1/advanced/delegation/destinations", counter: "AddDestination", stepUp: true,
			body: destination, altBody: dgWith(destination, "label", "hot wallet")},
		{name: "remove-destination", path: "/v1/advanced/delegation/destinations/" + dgDestinationID.String() + "/remove",
			counter: "RemoveDestination", stepUp: true, byID: true, body: map[string]any{"reason": "retired"},
			altBody: map[string]any{"reason": "compromised"}},
		{name: "policy", path: "/v1/advanced/delegation/withdrawal-policy", counter: "SetWithdrawalPolicy", stepUp: true,
			body: dgPolicyBody(), altBody: dgWith(dgPolicyBody(), "velocity_max_count", 11)},
		{name: "swap-decision", path: "/v1/advanced/delegation/swaps/" + dgSwapID.String() + "/decision", counter: "DecideSwap",
			stepUp: true, byID: true, body: approve, altBody: dgWith(approve, "decision", "DECLINE")},
		{name: "withdrawal-decision", path: "/v1/advanced/delegation/withdrawals/" + dgWithdrawalID.String() + "/decision",
			counter: "DecideWithdrawal", stepUp: true, byID: true, body: approve, altBody: dgWith(approve, "decision", "DECLINE")},
		{name: "cancel", path: "/v1/advanced/delegation/withdrawals/" + dgWithdrawalID.String() + "/cancel", counter: "CancelWithdrawal",
			byID: true, body: map[string]any{"reason": "not now"}, altBody: map[string]any{"reason": "not ever"}},
	}
}

func dgOperatorMutations() []dgMutation {
	return []dgMutation{
		{name: "venue", path: "/v1/advanced/delegation/ops/venues", counter: "RegisterVenue", stepUp: true,
			body: map[string]any{"name": "sandbox-ethereum", "spread_bps": 50}, altBody: map[string]any{"name": "sandbox-ethereum", "spread_bps": 60}},
		{name: "execute-pending", path: "/v1/advanced/delegation/ops/execute-pending", counter: "ExecutePending", stepUp: true,
			body: map[string]any{"limit": 5}, altBody: map[string]any{"limit": 6}},
	}
}

// dgRoute is one delegation route a principal may try.
type dgRoute struct {
	method, path string
	body         any
}

func dgAllRoutes(t *testing.T) []dgRoute {
	t.Helper()
	acct := "?account_id=" + testAccountID.String()
	routes := []dgRoute{
		{http.MethodGet, "/v1/advanced/delegation/agents" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/capabilities" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/capabilities/" + dgCapabilityID.String(), nil},
		{http.MethodGet, "/v1/advanced/delegation/destinations" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/withdrawal-policy" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/swaps" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/swaps/" + dgSwapID.String(), nil},
		{http.MethodGet, "/v1/advanced/delegation/withdrawals" + acct, nil},
		{http.MethodGet, "/v1/advanced/delegation/withdrawals/" + dgWithdrawalID.String(), nil},
		{http.MethodGet, "/v1/advanced/delegation/venues", nil},
		{http.MethodGet, "/v1/advanced/delegation/ops/capabilities", nil},
		{http.MethodGet, "/v1/advanced/delegation/ops/pending", nil},
	}
	for _, m := range append(dgCustomerMutations(t), dgOperatorMutations()...) {
		routes = append(routes, dgRoute{http.MethodPost, m.path, m.body})
	}
	return routes
}

// --- tenant scoping ------------------------------------------------------------------

func TestDelegationAccountReads_AreScopedToTheCallersAccount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path        string
		valueDomain bool
	}{
		{"/v1/advanced/delegation/agents", false},
		{"/v1/advanced/delegation/capabilities", true},
		{"/v1/advanced/delegation/destinations", false},
		{"/v1/advanced/delegation/withdrawal-policy", true},
		{"/v1/advanced/delegation/swaps", true},
		{"/v1/advanced/delegation/withdrawals", true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			res := h.do(http.MethodGet, tc.path+"?account_id="+testAccountID.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, testAccountID, f.lastAccount)
			if tc.valueDomain {
				assert.Equal(t, "SIMULATED", res.raw()["value_domain"])
			}

			before := f.total()
			res = h.do(http.MethodGet, tc.path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeForbidden, res.problem().Code)
			assert.Equal(t, before, f.total(), "a foreign account is refused before the service is asked")

			op := operatorPrincipal()
			h.as(&op)
			res = h.do(http.MethodGet, tc.path+"?account_id="+testOtherAcct.String(), nil)
			require.Equal(t, http.StatusOK, res.Code, "an operator holding account:read_any reads any account; body=%s", res.Body.String())
			assert.Equal(t, testOtherAcct, f.lastAccount)
		})
	}
}

// A record read by its own id is scoped by the account stored on it, and a
// stranger's answer is exactly what an absent id gets.
func TestDelegationRecordsByID_AreScopedByTheirOwnAccountAndMaskedAsNotFound(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"capabilities", "swaps", "withdrawals"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			own := map[string]string{"capabilities": dgCapabilityID.String(), "swaps": dgSwapID.String(),
				"withdrawals": dgWithdrawalID.String()}[kind]
			path := "/v1/advanced/delegation/" + kind + "/" + own
			res := h.do(http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, res.Code, "the owner reads it; body=%s", res.Body.String())
			assert.Equal(t, "SIMULATED", res.raw()["value_domain"])

			stranger := dgStranger()
			h.as(&stranger)
			foreign := h.do(http.MethodGet, path, nil)
			require.Equal(t, http.StatusNotFound, foreign.Code, "body=%s", foreign.Body.String())
			fp := foreign.problem()
			absent := h.do(http.MethodGet, "/v1/advanced/delegation/"+kind+"/"+dgAbsentID, nil)
			require.Equal(t, http.StatusNotFound, absent.Code, "body=%s", absent.Body.String())
			ap := absent.problem()
			assert.Equal(t, errs.CodeNotFound, fp.Code)
			assert.Equal(t, fp.Code, ap.Code)
			assert.Equal(t, strings.Replace(fp.Detail, own, "", 1), strings.Replace(ap.Detail, dgAbsentID, "", 1))
			assert.Equal(t, len(fp.Fields), len(ap.Fields), "a foreign and an absent record must read alike")

			op := operatorPrincipal()
			h.as(&op)
			require.Equal(t, http.StatusOK, h.do(http.MethodGet, path, nil).Code)
			assert.Zero(t, f.commands())
		})
	}
}

// Every owner command is the owner's alone: an operator holding every
// customer permission and account:read_any still cannot act through the
// owner's surface, and neither can another customer. A command keyed by a
// record's id answers them NOT_FOUND, as for an absent id.
func TestDelegationOwnerCommands_UseOwnershipOnly(t *testing.T) {
	t.Parallel()
	for _, m := range dgCustomerMutations(t) {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			want, code := http.StatusForbidden, errs.CodeForbidden
			if m.byID {
				want, code = http.StatusNotFound, errs.CodeNotFound
			}
			h, f := newDelegationHarness(t)
			for _, p := range []security.Principal{operatorPrincipal(), dgStranger()} {
				p := p
				h.as(&p)
				res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "owner-only-"+m.name+"-"+string(p.ActorType))
				require.Equal(t, want, res.Code, "%s body=%s", p.ActorType, res.Body.String())
				assert.Equal(t, code, res.problem().Code)
			}
			assert.Zero(t, f.commands(), "nobody but the owner reaches a command")
		})
	}
}

// Every grant, revocation, destination change, policy version and decision
// needs a recent strong authentication; cancelling a withdrawal only reduces
// risk and needs none.
func TestDelegationOwnerCommands_RequireAStepUpWhereTheyMustAndOnlyThere(t *testing.T) {
	t.Parallel()
	for _, m := range dgCustomerMutations(t) {
		t.Run(m.name, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			stale := dgStale(customerPrincipal())
			h.as(&stale)
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stale-"+m.name+"-0001")
			if m.stepUp {
				require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
				assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code)
				assert.Zero(t, f.count(m.counter))
				return
			}
			require.Equal(t, http.StatusOK, res.Code, "%s needs no step-up; body=%s", m.name, res.Body.String())
			assert.Equal(t, 1, f.count(m.counter))
		})
	}
}

// No delegation route admits an agent: it cannot grant itself scope, decide
// its own withdrawal, read anything or run anything over HTTP. Its requests
// are admitted in-process by the agent runtime, never here.
func TestDelegationRoutes_RefuseAnAgentPrincipal(t *testing.T) {
	t.Parallel()
	for _, r := range dgAllRoutes(t) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			agent := agentPrincipal()
			h.as(&agent)
			res := h.do(r.method, r.path, r.body, "Idempotency-Key", "agent-probe-0001")
			require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeForbidden, p.Code)
			assert.Contains(t, p.Detail, "agents")
			assert.Zero(t, f.total(), "an agent reaches nothing behind the port")
		})
	}
	t.Run("an agent cannot grant itself scope", func(t *testing.T) {
		t.Parallel()
		h, f := newDelegationHarness(t)
		agent := agentPrincipal()
		h.as(&agent)
		body := dgWith(dgGrantBody(), "agent_id", agent.SubjectID)
		res := h.do(http.MethodPost, "/v1/advanced/delegation/capabilities", body, "Idempotency-Key", "agent-self-grant")
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Zero(t, f.count("Grant"))
	})
	t.Run("an agent cannot decide its own withdrawal", func(t *testing.T) {
		t.Parallel()
		h, f := newDelegationHarness(t)
		agent := security.AgentPrincipal(dgAgentID, testAccountID.String())
		h.as(&agent)
		res := h.do(http.MethodPost, "/v1/advanced/delegation/withdrawals/"+dgWithdrawalID.String()+"/decision",
			map[string]any{"decision": "APPROVE", "plan_hash": dgPlanHash(t), "reason": "self-approval"},
			"Idempotency-Key", "agent-self-approve")
		require.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Zero(t, f.count("DecideWithdrawal"))
		assert.Zero(t, f.count("Withdrawal"), "the record is not even read for an agent")
	})
}

// The operator surface is closed to customers: their role holds no
// sandbox:operate.
func TestDelegationOperatorRoutes_RefuseACustomer(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)
	for _, m := range dgOperatorMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "customer-op-"+m.name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
	}
	for _, path := range []string{"/v1/advanced/delegation/ops/capabilities", "/v1/advanced/delegation/ops/pending"} {
		res := h.do(http.MethodGet, path, nil)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", path, res.Body.String())
	}
	assert.Zero(t, f.total())
}

func TestDelegationOperatorActions_RequireAStepUp(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)
	stale := dgStale(operatorPrincipal())
	h.as(&stale)
	for _, m := range dgOperatorMutations() {
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "stale-op-"+m.name)
		require.Equal(t, http.StatusForbidden, res.Code, "%s body=%s", m.name, res.Body.String())
		assert.Equal(t, errs.CodeStepUpRequired, res.problem().Code, m.name)
	}
	assert.Zero(t, f.commands())
	// Reading moves nothing and needs no step-up.
	for _, path := range []string{"/v1/advanced/delegation/ops/capabilities", "/v1/advanced/delegation/ops/pending"} {
		require.Equal(t, http.StatusOK, h.do(http.MethodGet, path, nil).Code, path)
	}
}

// --- idempotency ---------------------------------------------------------------------------

// Every delegation command replays: the same key and body answer the same
// response without a second call to the service, and the same key with a
// different request is refused.
func TestDelegationCommands_ReplayAndRefuseKeyReuse(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, m dgMutation, p security.Principal) {
		h, f := newDelegationHarness(t)
		h.as(&p)
		key := "replay-" + m.name + "-0001"
		first := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, first.Code, "body=%s", first.Body.String())
		require.Equal(t, 1, f.count(m.counter))

		second := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", key)
		require.Equal(t, http.StatusOK, second.Code, "body=%s", second.Body.String())
		assert.JSONEq(t, first.Body.String(), second.Body.String(), "a replay answers the first response")
		assert.Equal(t, 1, f.count(m.counter), "a replay never reaches the service")

		path, body := m.path, m.altBody
		if m.altPath != "" {
			path, body = m.altPath, m.body
		}
		reuse := h.do(http.MethodPost, path, body, "Idempotency-Key", key)
		require.Equal(t, http.StatusConflict, reuse.Code, "body=%s", reuse.Body.String())
		assert.Equal(t, errs.CodeInvalidIdempotencyReuse, reuse.problem().Code)
		assert.Equal(t, 1, f.count(m.counter))
	}
	for _, m := range dgCustomerMutations(t) {
		t.Run("owner/"+m.name, func(t *testing.T) {
			t.Parallel()
			run(t, m, customerPrincipal())
		})
	}
	for _, m := range dgOperatorMutations() {
		t.Run("operator/"+m.name, func(t *testing.T) {
			t.Parallel()
			run(t, m, operatorPrincipal())
		})
	}
}

// A refusal is a recorded conclusion: its code and reason reach the wire
// unchanged, and replaying the key repeats it without asking again.
func TestDelegationRefusals_KeepTheirCodeAndReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		m      func(t *testing.T) dgMutation
		err    error
		status int
		code   errs.Code
		reason string
	}{
		{"a grant to another account's agent", func(t *testing.T) dgMutation { return dgCustomerMutations(t)[0] },
			errs.New(errs.CodeForbidden, "delegation: a capability is granted only to one of the account's own agents").
				WithField("reason", delegation.ReasonAgentMismatch), http.StatusForbidden, errs.CodeForbidden, delegation.ReasonAgentMismatch},
		{"limits out of order", func(t *testing.T) dgMutation { return dgCustomerMutations(t)[0] },
			errs.New(errs.CodeValidationFailed, "delegation: invalid capability terms"), http.StatusBadRequest, errs.CodeValidationFailed, ""},
		{"a decision on another plan", func(t *testing.T) dgMutation { return dgCustomerMutations(t)[5] },
			errs.New(errs.CodeConflict, "delegation: the decision names a plan other than the swap's").
				WithField("reason", delegation.ReasonApprovalPlanMismatch), http.StatusConflict, errs.CodeConflict, delegation.ReasonApprovalPlanMismatch},
		{"the policy's own stronger authentication", func(t *testing.T) dgMutation { return dgCustomerMutations(t)[6] },
			errs.New(errs.CodeStepUpRequired, "delegation: a recent strong authentication is required"), http.StatusForbidden,
			errs.CodeStepUpRequired, ""},
		{"a signed withdrawal cannot be cancelled", func(t *testing.T) dgMutation { return dgCustomerMutations(t)[7] },
			errs.New(errs.CodeInvalidStateTransition, "delegation: the withdrawal can no longer be cancelled"), http.StatusConflict,
			errs.CodeInvalidStateTransition, ""},
		{"a venue that already exists", func(*testing.T) dgMutation { return dgOperatorMutations()[0] },
			errs.New(errs.CodeConflict, "delegation: a venue with this name or address already exists"), http.StatusConflict,
			errs.CodeConflict, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := tc.m(t)
			h, f := newDelegationHarness(t)
			if strings.Contains(m.path, "/ops/") {
				op := operatorPrincipal()
				h.as(&op)
			}
			f.cmdErr = tc.err
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "refused-"+m.name+"-01")
			require.Equal(t, tc.status, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, tc.code, p.Code)
			if tc.reason != "" {
				assert.Equal(t, tc.reason, p.Fields["reason"])
			}
			f.cmdErr = nil
			again := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "refused-"+m.name+"-01")
			require.Equal(t, tc.status, again.Code, "body=%s", again.Body.String())
			assert.Equal(t, tc.code, again.problem().Code)
			assert.Equal(t, 1, f.count(m.counter), "the recorded refusal is replayed, not asked again")
		})
	}
}

// --- validation ------------------------------------------------------------------------------

func TestDelegationRequests_AreValidatedBeforeTheService(t *testing.T) {
	t.Parallel()
	acct := testAccountID.String()
	plan := dgPlanHash(t)
	decision := func(k string, v any) map[string]any {
		return dgWith(map[string]any{"decision": "APPROVE", "plan_hash": plan, "reason": "ok"}, k, v)
	}
	destination := map[string]any{"account_id": acct, "chain": "sbx-solana", "address": dgPayoutKey.String(), "label": "x"}
	cases := []struct {
		name, method, path string
		body               any
		field              string
		operator           bool
	}{
		{"nil agent", http.MethodPost, "/v1/advanced/delegation/capabilities", dgWith(dgGrantBody(), "agent_id", dgNilUUID), "agent_id", false},
		{"unknown mode", http.MethodPost, "/v1/advanced/delegation/capabilities", dgWith(dgGrantBody(), "mode", "ON_CHAIN"), "mode", false},
		{"unknown instrument class", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "instrument_classes", []string{"BONDS"}), "instrument_classes", false},
		{"unknown action class", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "action_classes", []string{"WITHDRAW"}), "action_classes", false},
		{"unknown chain", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "chain_allowlist", []string{"sbx-bitcoin"}), "chain_allowlist", false},
		{"not an address", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "destination_allowlist", []string{"not-an-address"}), "destination_allowlist", false},
		{"dollars without cents", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "max_notional_usd", "1000"), "max_notional_usd", false},
		{"an exponent", http.MethodPost, "/v1/advanced/delegation/capabilities",
			dgWith(dgGrantBody(), "max_single_trade_usd", "1e2"), "max_single_trade_usd", false},
		{"destination address", http.MethodPost, "/v1/advanced/delegation/destinations", dgWith(destination, "address", "0xabc"), "address", false},
		{"destination chain", http.MethodPost, "/v1/advanced/delegation/destinations", dgWith(destination, "chain", "solana"), "chain", false},
		{"policy limit precision", http.MethodPost, "/v1/advanced/delegation/withdrawal-policy",
			dgWith(dgPolicyBody(), "per_withdrawal_limit_usd", "10.5"), "per_withdrawal_limit_usd", false},
		{"negative cooling", http.MethodPost, "/v1/advanced/delegation/withdrawal-policy",
			dgWith(dgPolicyBody(), "cooling_period_seconds", -1), "cooling_period_seconds", false},
		{"no such decision", http.MethodPost, "/v1/advanced/delegation/swaps/" + dgSwapID.String() + "/decision",
			decision("decision", "MAYBE"), "decision", false},
		{"short plan hash", http.MethodPost, "/v1/advanced/delegation/swaps/" + dgSwapID.String() + "/decision",
			decision("plan_hash", "abc"), "plan_hash", false},
		{"upper-case plan hash", http.MethodPost, "/v1/advanced/delegation/withdrawals/" + dgWithdrawalID.String() + "/decision",
			decision("plan_hash", strings.ToUpper(plan)), "plan_hash", false},
		{"nil capability id", http.MethodGet, "/v1/advanced/delegation/capabilities/" + dgNilUUID, nil, "capabilityId", false},
		{"nil swap id", http.MethodGet, "/v1/advanced/delegation/swaps/" + dgNilUUID, nil, "swapId", false},
		{"nil withdrawal id", http.MethodPost, "/v1/advanced/delegation/withdrawals/" + dgNilUUID + "/cancel",
			map[string]any{"reason": "x"}, "withdrawalId", false},
		{"nil destination id", http.MethodPost, "/v1/advanced/delegation/destinations/" + dgNilUUID + "/remove",
			map[string]any{"reason": "x"}, "destinationId", false},
		{"unknown swap status", http.MethodGet, "/v1/advanced/delegation/swaps?account_id=" + acct + "&status=PENDING", nil, "status", false},
		{"unknown withdrawal status", http.MethodGet, "/v1/advanced/delegation/withdrawals?account_id=" + acct + "&status=PAID", nil, "status", false},
		{"unknown capability status", http.MethodGet, "/v1/advanced/delegation/ops/capabilities?status=EXPIRED", nil, "status", true},
		{"zero limit", http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 0}, "limit", true},
		{"too large a run", http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 51}, "limit", true},
		{"nil house book", http.MethodPost, "/v1/advanced/delegation/ops/venues",
			map[string]any{"name": "sandbox-ethereum", "spread_bps": 50, "house_account_id": dgNilUUID}, "house_account_id", true},
		{"no venue name", http.MethodPost, "/v1/advanced/delegation/ops/venues", map[string]any{"name": " ", "spread_bps": 50}, "name", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			if tc.operator {
				op := operatorPrincipal()
				h.as(&op)
			}
			res := h.do(tc.method, tc.path, tc.body, "Idempotency-Key", "validate-key-0001")
			require.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
			p := res.problem()
			assert.Equal(t, errs.CodeValidationFailed, p.Code)
			assert.Equal(t, tc.field, p.Fields["field"])
			assert.Zero(t, f.commands(), "a malformed request never reaches a command")
		})
	}
}

// --- what reaches the service ------------------------------------------------------------------

// The handler passes the service the owner's scoped account, the terms
// exactly as typed (exact cents, parsed addresses, the Idempotency-Key as
// the grant's own key), the plan hash the owner was shown, and the operator
// as the venue's registrant. The environment and the agent's strategy are not
// the caller's to choose: the service takes them from the deployment and the
// agent.
func TestDelegationCommands_PassTheScopedRequestToTheService(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)
	ms := dgCustomerMutations(t)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[0].path, ms[0].body, "Idempotency-Key", "grant-key-00001").Code)
	g := f.lastGrant
	assert.Equal(t, testAccountID, g.AccountID)
	assert.Equal(t, dgAgentID, g.AgentID)
	assert.Equal(t, gates.ModeSandbox, g.Mode)
	assert.Equal(t, []delegation.InstrumentClass{delegation.ClassStableValue, delegation.ClassSpotToken}, g.InstrumentClasses)
	assert.Equal(t, []delegation.ActionClass{delegation.ActionAcquireExposure, delegation.ActionProposeWithdrawal}, g.ActionClasses)
	assert.Equal(t, int64(100_000), g.MaxNotional.Minor())
	assert.Equal(t, int64(50_000), g.MaxDailyNotional.Minor())
	assert.Equal(t, int64(10_000), g.MaxSingleTrade.Minor())
	assert.Equal(t, []devnet.PublicKey{dgVenueKey, dgPayoutKey}, g.DestinationAllowlist)
	assert.Equal(t, []devnet.ChainID{devnethost.ChainSolana}, g.ChainAllowlist)
	assert.True(t, g.ExpiresAt.Equal(testNow.Add(24*time.Hour)))
	assert.True(t, g.RequiresHumanApproval)
	assert.Equal(t, "grant-key-00001", g.IdempotencyKey, "the Idempotency-Key is the grant's own key")

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[1].path, ms[1].body, "Idempotency-Key", "revoke-key-0001").Code)
	assert.Equal(t, delegation.RevokeRequest{CapabilityID: dgCapabilityID, Reason: "no longer needed"}, f.lastRevoke)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[2].path, ms[2].body, "Idempotency-Key", "dest-key-00001").Code)
	assert.Equal(t, delegation.DestinationRequest{AccountID: testAccountID, Chain: devnethost.ChainSolana, Address: dgPayoutKey,
		Label: "cold wallet"}, f.lastDestination)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[4].path, ms[4].body, "Idempotency-Key", "policy-key-0001").Code)
	p := f.lastPolicy
	assert.Equal(t, testAccountID, p.AccountID)
	assert.Equal(t, int64(25_000), p.PerWithdrawalLimit.Minor())
	assert.Equal(t, int64(100_000), p.DailyLimit.Minor())
	assert.Equal(t, time.Hour, p.CoolingPeriod)
	assert.Equal(t, 15*time.Minute, p.StrongAuthMaxAge)
	assert.True(t, p.StrongAuthRequired)
	assert.Equal(t, 80, p.MaxRiskScore)
	assert.Equal(t, 10, p.VelocityMaxCount)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[5].path, ms[5].altBody, "Idempotency-Key", "decide-key-0001").Code)
	d := f.lastSwapDecision
	assert.Equal(t, dgSwapID, d.SwapID)
	assert.False(t, d.Approve, "DECLINE is not an approval")
	assert.Equal(t, dgPlanHash(t), hex.EncodeToString(d.PlanHash[:]), "the decision is bound to the plan hash shown")

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ms[6].path, ms[6].body, "Idempotency-Key", "decide-key-0002").Code)
	assert.True(t, f.lastWithdrawDecision.Approve)
	assert.Equal(t, dgWithdrawalID, f.lastWithdrawDecision.WithdrawalID)

	op := operatorPrincipal()
	h.as(&op)
	ops := dgOperatorMutations()
	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ops[0].path, map[string]any{"name": "sandbox-ethereum", "spread_bps": 25,
		"house_account_id": dgHouseID.String()}, "Idempotency-Key", "venue-key-00001").Code)
	v := f.lastVenue
	assert.Equal(t, "sandbox-ethereum", v.Name)
	assert.Equal(t, money.BPS(25), v.SpreadBPS)
	require.NotNil(t, v.HouseAccountID)
	assert.Equal(t, dgHouseID, *v.HouseAccountID)
	assert.Equal(t, delegation.Actor{Type: security.ActorOperator, ID: op.SubjectID}, v.Actor)

	require.Equal(t, http.StatusOK, h.do(http.MethodPost, ops[1].path, map[string]any{}, "Idempotency-Key", "run-key-0000001").Code)
	assert.Equal(t, delegationExecuteDefault, f.lastLimit, "a run without a limit takes the default")
}

// --- what comes back -----------------------------------------------------------------------------

// Every figure is rendered exactly as the service reported it: USD as cents
// with two digits, amounts as exact base units, the plan as it was hashed;
// what the read derives (the remaining limits, the standing) follows from
// those figures alone.
func TestDelegationReads_RenderTheServicesFiguresExactly(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)

	res := h.do(http.MethodGet, "/v1/advanced/delegation/capabilities/"+dgCapabilityID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	c := res.raw()
	assert.Equal(t, "1000.00", c["max_notional_usd"])
	assert.Equal(t, "500.00", c["max_daily_notional_usd"])
	assert.Equal(t, "100.00", c["max_single_trade_usd"])
	assert.Equal(t, "ACTIVE", c["standing"])
	assert.Equal(t, "SANDBOX", c["mode"])
	assert.Equal(t, "momentum agent", c["agent_name"])
	assert.Equal(t, hex.EncodeToString(f.capability.Capability.ContentHash[:]), c["content_hash"])
	assert.Equal(t, []any{dgVenueKey.String(), dgPayoutKey.String()}, c["destination_allowlist"])
	assert.Equal(t, []any{"sbx-solana"}, c["chain_allowlist"])
	usage := c["usage"].(map[string]any)
	assert.Equal(t, map[string]any{"day": "2026-09-06", "uses": float64(2), "used_today_usd": "40.00", "used_total_usd": "140.00",
		"remaining_today_usd": "460.00", "remaining_total_usd": "860.00"}, usage)
	uses := c["uses"].([]any)
	require.Len(t, uses, 1)
	assert.Equal(t, "40.00", uses[0].(map[string]any)["notional_usd"])
	assert.Equal(t, "SWAP", uses[0].(map[string]any)["subject_kind"])

	// Over a limit, nothing remains -- never a negative figure.
	f.capability.Usage.UsedToday = usd("600.00")
	f.capability.Capability.Terms.ExpiresAt = testNow
	res = h.do(http.MethodGet, "/v1/advanced/delegation/capabilities?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code)
	item := res.raw()["items"].([]any)[0].(map[string]any)
	assert.Equal(t, "0.00", item["usage"].(map[string]any)["remaining_today_usd"])
	assert.Equal(t, "EXPIRED", item["standing"], "a capability is expired from its expiry on, whatever its stored status")
	assert.Equal(t, "ACTIVE", item["status"])

	res = h.do(http.MethodPost, "/v1/advanced/delegation/capabilities/"+dgCapabilityID.String()+"/revoke",
		map[string]any{"reason": "done"}, "Idempotency-Key", "revoke-render-01")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	revoked := res.raw()
	assert.Equal(t, "REVOKED", revoked["standing"])
	assert.Equal(t, "done", revoked["revocation_reason"])
	assert.Equal(t, map[string]any{"actor_type": "USER", "actor_id": testUserID.String()}, revoked["revoked_by"])

	res = h.do(http.MethodGet, "/v1/advanced/delegation/swaps/"+dgSwapID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	sw := res.raw()
	assert.Equal(t, "100000000", sw["amount_in"])
	assert.Equal(t, "995000000", sw["amount_out"])
	assert.Equal(t, "985050000", sw["min_amount_out"])
	assert.Equal(t, "100.00", sw["notional_usd"])
	assert.Equal(t, "7", sw["request_nonce"])
	assert.Equal(t, "AWAITING_APPROVAL", sw["status"])
	assert.Equal(t, dgPlanHash(t), sw["plan_hash"])
	plan := sw["plan"].(map[string]any)
	assert.Equal(t, "100000000", plan["amount_in"])
	assert.Equal(t, "100.00", plan["notional_usd"])
	assert.Equal(t, "10", plan["fee_per_tx"])
	assert.Equal(t, map[string]any{"mantissa": "10000", "scale": "2", "source": "test-oracle", "observed_at": "2026-09-06T12:00:00Z",
		"par": false}, plan["receive_price"])
	assert.Equal(t, map[string]any{"id": dgVenueID.String(), "name": "sandbox-solana", "address": dgVenueKey.String()}, sw["venue"])
	assert.Equal(t, float64(9), sw["receive_asset"].(map[string]any)["decimals"])
	assert.Len(t, sw["transitions"], 2)
	assert.Nil(t, sw["inspection"], "nothing was inspected before signing")
	assert.Equal(t, "SIMULATED", sw["value_domain"])

	f.withdrawal.Inspection = &DelegationInspectionView{ID: dgUsageID.String(), Verdict: delegation.VerdictRejected,
		ReasonCodes: []string{delegation.InspectDestinationMismatch}, SignerPublicKey: dgWalletKey.String(),
		CapabilityHash: []byte{1, 2}, PlanHash: []byte{3}, MessageSHA256: []byte{4}, PolicyVersion: "delegated-signing-v1",
		PolicyHash: "ph", InspectorVersion: delegation.InspectorVersion, DecidedAt: testNow}
	res = h.do(http.MethodGet, "/v1/advanced/delegation/withdrawals/"+dgWithdrawalID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	w := res.raw()
	assert.Equal(t, "25000000", w["amount"])
	assert.Equal(t, "25.00", w["notional_usd"])
	ev := w["evaluation"].(map[string]any)
	assert.Equal(t, "12.50", ev["day_total_before_usd"], "the evaluation's day total is cents, rendered as the sandbox dollar")
	assert.Equal(t, float64(86400), ev["destination_age_seconds"])
	assert.Equal(t, "HUMAN_REQUIRED", ev["outcome"])
	assert.Equal(t, []any{delegation.ReasonHumanAutoApprovalDisabled}, ev["reason_codes"])
	assert.Equal(t, map[string]any{"id": dgDestinationID.String(), "label": "cold wallet", "status": "ACTIVE"}, w["destination"])
	inspection := w["inspection"].(map[string]any)
	assert.Equal(t, "REJECTED", inspection["verdict"])
	assert.Equal(t, []any{delegation.InspectDestinationMismatch}, inspection["reason_codes"])
	assert.Equal(t, "0102", inspection["capability_hash"])

	res = h.do(http.MethodGet, "/v1/advanced/delegation/withdrawal-policy?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	pol := res.raw()
	current := pol["current"].(map[string]any)
	assert.Equal(t, "250.00", current["per_withdrawal_limit_usd"])
	assert.Equal(t, float64(900), current["strong_auth_max_age_seconds"])
	assert.Equal(t, float64(2), current["version"])
	assert.Equal(t, strings.Repeat("cd", 32), current["policy_hash"])
	assert.Len(t, pol["versions"], 1)

	f.policies = nil
	res = h.do(http.MethodGet, "/v1/advanced/delegation/withdrawal-policy?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusOK, res.Code)
	empty := res.raw()
	assert.NotContains(t, empty, "current", "an account with no policy has none in force; nothing is withdrawn without one")
	assert.Equal(t, []any{}, empty["versions"])

	op := operatorPrincipal()
	h.as(&op)
	res = h.do(http.MethodGet, "/v1/advanced/delegation/venues", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	venues := res.raw()
	assert.Equal(t, true, venues["items"].([]any)[0].(map[string]any)["co_signer"])
	assert.Equal(t, float64(50), venues["items"].([]any)[0].(map[string]any)["spread_bps"])
	assert.Len(t, venues["co_signers"], 2)
	assert.Equal(t, false, venues["co_signers"].([]any)[1].(map[string]any)["registered"])

	res = h.do(http.MethodGet, "/v1/advanced/delegation/ops/pending?limit=7", nil)
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 7, f.lastLimit)
	pending := res.raw()
	assert.Len(t, pending["swaps"], 1)
	assert.Len(t, pending["withdrawals"], 1)
	assert.Equal(t, "SIMULATED", pending["value_domain"])
}

// An approval is followed by the service's execution; the decision stands
// whatever the execution then did, and the response says how it ended
// without ever leaking an internal error's text.
func TestDelegationDecisions_ReportHowTheExecutionEnded(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		execution DelegationExecution
		want      map[string]any
	}{
		{"declined, nothing executed", DelegationExecution{}, map[string]any{"attempted": false}},
		{"approved and settled", DelegationExecution{Attempted: true}, map[string]any{"attempted": true}},
		{"approved, refused at signing", DelegationExecution{Attempted: true,
			Err: errs.New(errs.CodeSigningRejected, "delegation: the venue's quote expired before the swap was signed").
				WithField("reason", delegation.ReasonQuoteExpired)},
			map[string]any{"attempted": true, "error": map[string]any{"code": "SIGNING_REJECTED", "reason": delegation.ReasonQuoteExpired,
				"detail": "delegation: the venue's quote expired before the swap was signed"}}},
		{"approved, outcome unknown", DelegationExecution{Attempted: true,
			Err: errs.New(errs.CodeSubmissionStateUnknown, "delegation: the submission's outcome is unknown")},
			map[string]any{"attempted": true, "error": map[string]any{"code": "SUBMISSION_STATE_UNKNOWN",
				"detail": "delegation: the submission's outcome is unknown"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, f := newDelegationHarness(t)
			f.execution = tc.execution
			m := dgCustomerMutations(t)[5]
			res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "execution-report-1")
			require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
			out := res.raw()
			assert.Equal(t, tc.want, out["execution"])
			assert.Equal(t, "SIMULATED", out["value_domain"])
			assert.Equal(t, dgSwapID.String(), out["swap"].(map[string]any)["id"])
		})
	}
	t.Run("an internal failure is reported without its text", func(t *testing.T) {
		t.Parallel()
		h, f := newDelegationHarness(t)
		f.execution = DelegationExecution{Attempted: true, Err: errors.New("dial tcp 10.0.0.7:5432: password=hunter2 refused")}
		m := dgCustomerMutations(t)[6]
		res := h.do(http.MethodPost, m.path, m.body, "Idempotency-Key", "execution-report-2")
		require.Equal(t, http.StatusOK, res.Code, "the decision stands; body=%s", res.Body.String())
		assert.NotContains(t, res.Body.String(), "hunter2")
		assert.NotContains(t, res.Body.String(), "10.0.0.7")
		e := res.raw()["execution"].(map[string]any)["error"].(map[string]any)
		assert.Equal(t, "INTERNAL", e["code"])
	})
}

func TestDelegationExecutionRun_ReportsEverySubject(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)
	op := operatorPrincipal()
	h.as(&op)
	res := h.do(http.MethodPost, "/v1/advanced/delegation/ops/execute-pending", map[string]any{"limit": 2}, "Idempotency-Key", "execute-run-0001")
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 2, f.lastLimit)
	out := res.raw()
	assert.Equal(t, "SIMULATED", out["value_domain"])
	items := out["items"].([]any)
	require.Len(t, items, 2)
	settled := items[0].(map[string]any)
	assert.Equal(t, map[string]any{"kind": "SWAP", "id": dgSwapID.String(), "account_id": testAccountID.String(),
		"status_before": "ADMITTED", "status": "SETTLED"}, settled)
	cooling := items[1].(map[string]any)
	assert.Equal(t, "APPROVED", cooling["status"])
	assert.Equal(t, map[string]any{"code": "INVALID_STATE_TRANSITION", "reason": delegation.ReasonCoolingPeriod,
		"detail": "delegation: the withdrawal's cooling period has not elapsed"}, cooling["error"])
}

func TestDelegationRoutes_UnwiredPortIsUnsupported(t *testing.T) {
	t.Parallel()
	for _, r := range dgAllRoutes(t) {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if strings.Contains(r.path, "/ops/") {
				op := operatorPrincipal()
				h.as(&op)
			}
			res := h.do(r.method, r.path, r.body, "Idempotency-Key", "unwired-key-0001")
			require.Equal(t, http.StatusUnprocessableEntity, res.Code, "body=%s", res.Body.String())
			assert.Equal(t, errs.CodeUnsupported, res.problem().Code)
		})
	}
}

// A read the service fails keeps its code; nothing is invented in its place.
func TestDelegationReads_KeepTheServicesErrorCodes(t *testing.T) {
	t.Parallel()
	h, f := newDelegationHarness(t)
	f.readErr = errs.New(errs.CodeProviderUnavailable, "the simulated chain is not reachable")
	res := h.do(http.MethodGet, "/v1/advanced/delegation/swaps?account_id="+testAccountID.String(), nil)
	require.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, errs.CodeProviderUnavailable, res.problem().Code)
}

func TestDelegationRemaining_IsNeverNegative(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "0.00", delegationRemaining(usd("10.00"), usd("10.01")).String())
	assert.Equal(t, "0.01", delegationRemaining(usd("10.00"), usd("9.99")).String())
	assert.Equal(t, "10.00", delegationRemaining(usd("10.00"), usd("0.00")).String())
}
