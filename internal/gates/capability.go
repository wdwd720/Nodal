package gates

import (
	"github.com/nodal/controlplane/internal/id"
)

// Capability is a production capability whose exercise is gated (PART 54).
type Capability string

// Capabilities. The set is closed and mirrors the CHECK constraint on
// capability_gates.capability.
const (
	LiveFunding           Capability = "LIVE_FUNDING"
	LiveManualTrading     Capability = "LIVE_MANUAL_TRADING"
	LiveAgentTrading      Capability = "LIVE_AGENT_TRADING"
	Withdrawals           Capability = "WITHDRAWALS"
	SocialDataPersistence Capability = "SOCIAL_DATA_PERSISTENCE"
	Marketplace           Capability = "MARKETPLACE"
	CrossChain            Capability = "CROSS_CHAIN"
	PredictionMarkets     Capability = "PREDICTION_MARKETS"
	Securities            Capability = "SECURITIES"
	CEXTrading            Capability = "CEX_TRADING"

	// --- Nodal-native economy (gola.md PARTS XII-XXI) ---------------------
	//
	// These are gates rather than feature flags because PART LXXXIX forbids an
	// ordinary flag being the sole protection for a legal financial
	// capability. The machinery here -- dual control, evidence references,
	// database-held state authority -- is what that requirement is asking for.

	// CreditPurchase lets a user buy Credits with real money.
	CreditPurchase Capability = "CREDIT_PURCHASE"
	// NativeAssetCreation lets a user create a Nodal-native asset.
	NativeAssetCreation Capability = "NATIVE_ASSET_CREATION"
	// NativeMarketTrading lets Credits and native assets move against each
	// other on the internal market.
	NativeMarketTrading Capability = "NATIVE_MARKET_TRADING"
	// PayoutReserve lets eligible Credits be committed to a payout.
	PayoutReserve Capability = "PAYOUT_RESERVE"
	// PayoutSettle lets reserved value leave the system.
	PayoutSettle Capability = "PAYOUT_SETTLE"
	// HostedTrading lets fiat and crypto move inside a partner account.
	HostedTrading Capability = "HOSTED_TRADING"
	// HostedFunding lets value enter or leave a partner account.
	HostedFunding Capability = "HOSTED_FUNDING"

	// --- agent authority above level 3 (PART XXVIII) ----------------------
	//
	// Each level has its own gate: approving "choose among the options I
	// picked" is not approving "choose my investments".

	// AgentBoundedDiscretion is authority level 4.
	AgentBoundedDiscretion Capability = "AGENT_BOUNDED_DISCRETION"
	// AgentAutonomousSelection is authority level 5.
	AgentAutonomousSelection Capability = "AGENT_AUTONOMOUS_SELECTION"
	// AgentAutonomousPortfolio is authority level 6.
	AgentAutonomousPortfolio Capability = "AGENT_AUTONOMOUS_PORTFOLIO"
)

var allCapabilities = []Capability{
	LiveFunding, LiveManualTrading, LiveAgentTrading, Withdrawals,
	SocialDataPersistence, Marketplace, CrossChain, PredictionMarkets,
	Securities, CEXTrading,

	CreditPurchase, NativeAssetCreation, NativeMarketTrading,
	PayoutReserve, PayoutSettle, HostedTrading, HostedFunding,
	AgentBoundedDiscretion, AgentAutonomousSelection, AgentAutonomousPortfolio,
}

// AllCapabilities returns every capability in declaration order.
func AllCapabilities() []Capability { return append([]Capability(nil), allCapabilities...) }

// Valid reports whether c is a declared capability.
func (c Capability) Valid() bool {
	for _, k := range allCapabilities {
		if k == c {
			return true
		}
	}
	return false
}

// IsHighRisk reports whether activating c moves live money or opens
// exposure to an external venue, and therefore requires the full evidence
// set (legal review, provider contract, risk approval, security approval)
// and dual authorization (PART 55; POLICY_AUTHORITY §1 condition 4).
func IsHighRisk(c Capability) bool {
	switch c {
	case LiveFunding, LiveManualTrading, LiveAgentTrading, Withdrawals,
		Securities, CEXTrading, CrossChain, PredictionMarkets:
		return true

	// The internal economy. A capability is high risk when exercising it moves
	// value a user could reasonably believe is theirs, or changes who decides
	// what happens to that value.
	case CreditPurchase, NativeMarketTrading, PayoutReserve, PayoutSettle,
		HostedTrading, HostedFunding,
		AgentBoundedDiscretion, AgentAutonomousSelection, AgentAutonomousPortfolio:
		return true

		// NativeAssetCreation is deliberately NOT high risk. Creating a draft
		// asset moves nothing; it is gated because publication is a content and
		// jurisdiction question, and it needs a legal review reference. Demanding
		// a provider contract before somebody may name a token would be theatre,
		// and a control that is theatre teaches operators to route around controls.
	}
	return false
}

// GateState is the persisted state of a gate (PART 54).
type GateState string

// Gate states.
const (
	StateDisabled        GateState = "DISABLED"
	StatePendingApproval GateState = "PENDING_APPROVAL"
	StateApproved        GateState = "APPROVED"
	StateActive          GateState = "ACTIVE"
	StateSuspended       GateState = "SUSPENDED"
	StateRevoked         GateState = "REVOKED"
	StateExpired         GateState = "EXPIRED"
)

var allStates = []GateState{
	StateDisabled, StatePendingApproval, StateApproved, StateActive,
	StateSuspended, StateRevoked, StateExpired,
}

// AllStates returns every state in declaration order.
func AllStates() []GateState { return append([]GateState(nil), allStates...) }

// Valid reports whether s is a declared state.
func (s GateState) Valid() bool {
	for _, k := range allStates {
		if k == s {
			return true
		}
	}
	return false
}

// transitions is the explicit legal-transition table (POLICY_AUTHORITY §1).
//
//	DISABLED|REVOKED|EXPIRED → PENDING_APPROVAL   (Propose: new approval version)
//	PENDING_APPROVAL         → APPROVED           (Approve: first distinct approver)
//	APPROVED                 → ACTIVE             (Activate: second distinct approver)
//	ACTIVE                   → SUSPENDED          (Suspend: single operator, fast)
//	SUSPENDED                → APPROVED           (Resume: must be re-activated)
//	APPROVED|ACTIVE          → EXPIRED            (ExpireDue: by time)
//	any but REVOKED          → REVOKED            (Revoke: terminal for the version)
var transitions = map[GateState][]GateState{
	StateDisabled:        {StatePendingApproval, StateRevoked},
	StatePendingApproval: {StateApproved, StateRevoked},
	StateApproved:        {StateActive, StateExpired, StateRevoked},
	StateActive:          {StateSuspended, StateExpired, StateRevoked},
	StateSuspended:       {StateApproved, StateRevoked},
	StateRevoked:         {StatePendingApproval},
	StateExpired:         {StatePendingApproval, StateRevoked},
}

// CanTransition reports whether from → to is a legal gate transition.
func CanTransition(from, to GateState) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Environments accepted by NewChecker, NewAdmin and Bootstrap; mirrors the
// CHECK constraint on capability_gates.environment.
var environments = []string{"LOCAL", "TEST", "DEV", "STAGING", "PROD"}

func validEnvironment(env string) bool {
	for _, e := range environments {
		if e == env {
			return true
		}
	}
	return false
}

type (
	gateKind       struct{}
	transitionKind struct{}
)

// GateID identifies a capability_gates row.
type GateID = id.ID[gateKind]

// TransitionID identifies a capability_gate_transitions row.
type TransitionID = id.ID[transitionKind]

// NewGateID mints a gate id.
func NewGateID() GateID { return id.New[gateKind]() }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }
