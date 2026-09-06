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
)

var allCapabilities = []Capability{
	LiveFunding, LiveManualTrading, LiveAgentTrading, Withdrawals,
	SocialDataPersistence, Marketplace, CrossChain, PredictionMarkets,
	Securities, CEXTrading,
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
