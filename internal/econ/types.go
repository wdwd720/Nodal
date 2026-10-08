package econ

import (
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

// IntentKind is the typed economic intent family (goal §4). Venue-specific
// behaviour never appears here: a subsystem maps an intent onto its own
// mechanics.
type IntentKind string

// Intent kinds.
const (
	KindAcquireExposure           IntentKind = "ACQUIRE_EXPOSURE"
	KindReduceExposure            IntentKind = "REDUCE_EXPOSURE"
	KindTransferValue             IntentKind = "TRANSFER_VALUE"
	KindBorrow                    IntentKind = "BORROW"
	KindRepay                     IntentKind = "REPAY"
	KindOpenPosition              IntentKind = "OPEN_POSITION"
	KindClosePosition             IntentKind = "CLOSE_POSITION"
	KindExerciseOption            IntentKind = "EXERCISE_OPTION"
	KindMintStableAsset           IntentKind = "MINT_STABLE_ASSET"
	KindBurnStableAsset           IntentKind = "BURN_STABLE_ASSET"
	KindProvideLiquidity          IntentKind = "PROVIDE_LIQUIDITY"
	KindTakeLiquidity             IntentKind = "TAKE_LIQUIDITY"
	KindWithdraw                  IntentKind = "WITHDRAW"
	KindBridgeValue               IntentKind = "BRIDGE_VALUE"
	KindCreatePredictionPosition  IntentKind = "CREATE_PREDICTION_POSITION"
	KindResolvePredictionPosition IntentKind = "RESOLVE_PREDICTION_POSITION"
)

var allIntentKinds = []IntentKind{
	KindAcquireExposure, KindReduceExposure, KindTransferValue, KindBorrow, KindRepay,
	KindOpenPosition, KindClosePosition, KindExerciseOption, KindMintStableAsset, KindBurnStableAsset,
	KindProvideLiquidity, KindTakeLiquidity, KindWithdraw, KindBridgeValue,
	KindCreatePredictionPosition, KindResolvePredictionPosition,
}

// AllIntentKinds returns every intent kind in declaration order (a copy).
func AllIntentKinds() []IntentKind { return append([]IntentKind(nil), allIntentKinds...) }

// Valid reports whether k is a declared kind.
func (k IntentKind) Valid() bool {
	for _, x := range allIntentKinds {
		if x == k {
			return true
		}
	}
	return false
}

// RiskIncreasing reports whether an intent of this kind can only add risk.
// Kinds that can reduce risk (REDUCE_EXPOSURE, REPAY, CLOSE_POSITION, BURN,
// RESOLVE) are not; the subsystem decides per request for the rest.
func (k IntentKind) RiskIncreasing() bool {
	switch k {
	case KindAcquireExposure, KindBorrow, KindOpenPosition, KindMintStableAsset,
		KindProvideLiquidity, KindTakeLiquidity, KindCreatePredictionPosition:
		return true
	}
	return false
}

// IntentStatus is the status of an economic intent.
type IntentStatus string

// Intent statuses.
const (
	StatusReceived     IntentStatus = "RECEIVED"
	StatusRouted       IntentStatus = "ROUTED"
	StatusRiskApproved IntentStatus = "RISK_APPROVED"
	StatusReserved     IntentStatus = "RESERVED"
	StatusExecuting    IntentStatus = "EXECUTING"
	StatusCompleted    IntentStatus = "COMPLETED"
	StatusRejected     IntentStatus = "REJECTED"
	StatusFailed       IntentStatus = "FAILED"
	StatusUnknown      IntentStatus = "UNKNOWN"
	StatusReconciled   IntentStatus = "RECONCILED"
	StatusCancelled    IntentStatus = "CANCELLED"
)

var allIntentStatuses = []IntentStatus{
	StatusReceived, StatusRouted, StatusRiskApproved, StatusReserved, StatusExecuting,
	StatusCompleted, StatusRejected, StatusFailed, StatusUnknown, StatusReconciled, StatusCancelled,
}

// AllIntentStatuses returns every intent status in declaration order (a copy).
func AllIntentStatuses() []IntentStatus { return append([]IntentStatus(nil), allIntentStatuses...) }

// Valid reports whether s is a declared status.
func (s IntentStatus) Valid() bool {
	for _, x := range allIntentStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is legal.
func (s IntentStatus) Terminal() bool {
	switch s {
	case StatusCompleted, StatusRejected, StatusFailed, StatusReconciled, StatusCancelled:
		return true
	}
	return false
}

// intentTransitions is the explicit legal transition table. Every non-terminal
// status may be REJECTED or CANCELLED; EXECUTING may end UNKNOWN, and UNKNOWN
// ends only through reconciliation.
var intentTransitions = map[IntentStatus][]IntentStatus{
	StatusReceived:     {StatusRouted, StatusRejected, StatusCancelled},
	StatusRouted:       {StatusRiskApproved, StatusRejected, StatusCancelled},
	StatusRiskApproved: {StatusReserved, StatusExecuting, StatusRejected, StatusCancelled},
	StatusReserved:     {StatusExecuting, StatusRejected, StatusCancelled},
	StatusExecuting:    {StatusCompleted, StatusFailed, StatusUnknown},
	StatusUnknown:      {StatusReconciled, StatusFailed},
}

// CanTransitionIntent reports whether from -> to is legal.
func CanTransitionIntent(from, to IntentStatus) bool {
	for _, t := range intentTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// PersistableModes are the only modes an economic intent can be stored in.
// gates.ModeLive is deliberately absent, here and in the CHECK constraint.
func PersistableModes() []gates.Mode {
	return []gates.Mode{gates.ModePaper, gates.ModeSandbox, gates.ModeDevnet}
}

// AdvancedCapabilities returns the capabilities an economic intent may
// exercise: exactly the advanced gate specs.
func AdvancedCapabilities() []gates.Capability {
	specs := gates.AdvancedSpecs()
	out := make([]gates.Capability, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Capability)
	}
	return out
}

// Subsystem names the advanced subsystem that produced an economic effect.
type Subsystem string

// Subsystems.
const (
	SubsystemSandboxFunding    Subsystem = "SANDBOX_FUNDING"
	SubsystemStableAsset       Subsystem = "STABLE_ASSET"
	SubsystemMargin            Subsystem = "MARGIN"
	SubsystemDerivatives       Subsystem = "DERIVATIVES"
	SubsystemEquities          Subsystem = "EQUITIES"
	SubsystemOrders            Subsystem = "ORDERS"
	SubsystemTransfers         Subsystem = "TRANSFERS"
	SubsystemTreasury          Subsystem = "TREASURY"
	SubsystemFees              Subsystem = "FEES"
	SubsystemMarketplace       Subsystem = "MARKETPLACE"
	SubsystemSandboxRuntime    Subsystem = "SANDBOX_RUNTIME"
	SubsystemAgentAuthority    Subsystem = "AGENT_AUTHORITY"
	SubsystemWithdrawals       Subsystem = "WITHDRAWALS"
	SubsystemBridge            Subsystem = "BRIDGE"
	SubsystemPredictionMarkets Subsystem = "PREDICTION_MARKETS"
)

var allSubsystems = []Subsystem{
	SubsystemSandboxFunding, SubsystemStableAsset, SubsystemMargin, SubsystemDerivatives, SubsystemEquities,
	SubsystemOrders, SubsystemTransfers, SubsystemTreasury, SubsystemFees, SubsystemMarketplace,
	SubsystemSandboxRuntime, SubsystemAgentAuthority, SubsystemWithdrawals, SubsystemBridge,
	SubsystemPredictionMarkets,
}

// AllSubsystems returns every subsystem in declaration order (a copy).
func AllSubsystems() []Subsystem { return append([]Subsystem(nil), allSubsystems...) }

// Valid reports whether s is a declared subsystem.
func (s Subsystem) Valid() bool {
	for _, x := range allSubsystems {
		if x == s {
			return true
		}
	}
	return false
}

// EffectStatus is the status of an economic effect.
type EffectStatus string

// Effect statuses.
const (
	EffectPending     EffectStatus = "PENDING"
	EffectApplied     EffectStatus = "APPLIED"
	EffectUnknown     EffectStatus = "UNKNOWN"
	EffectFailed      EffectStatus = "FAILED"
	EffectCompensated EffectStatus = "COMPENSATED"
)

var allEffectStatuses = []EffectStatus{EffectPending, EffectApplied, EffectUnknown, EffectFailed, EffectCompensated}

// AllEffectStatuses returns every effect status in declaration order (a copy).
func AllEffectStatuses() []EffectStatus { return append([]EffectStatus(nil), allEffectStatuses...) }

// Valid reports whether s is a declared effect status.
func (s EffectStatus) Valid() bool {
	for _, x := range allEffectStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// effectTransitions mirrors cp_effect_transition_legal (migration 00731); a
// parity test compares the two.
var effectTransitions = map[EffectStatus][]EffectStatus{
	EffectPending: {EffectApplied, EffectUnknown, EffectFailed},
	EffectUnknown: {EffectApplied, EffectFailed},
	EffectApplied: {EffectCompensated},
}

// CanTransitionEffect reports whether from -> to is legal.
func CanTransitionEffect(from, to EffectStatus) bool {
	for _, t := range effectTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// FacilityStatus is the status of a facility.
type FacilityStatus string

// Facility statuses.
const (
	FacilityActive    FacilityStatus = "ACTIVE"
	FacilitySuspended FacilityStatus = "SUSPENDED"
	FacilityClosed    FacilityStatus = "CLOSED"
)

// AllFacilityStatuses returns every facility status.
func AllFacilityStatuses() []FacilityStatus {
	return []FacilityStatus{FacilityActive, FacilitySuspended, FacilityClosed}
}

// HouseStatus is the status of a house book.
type HouseStatus string

// House statuses.
const (
	HouseActive HouseStatus = "ACTIVE"
	HouseKilled HouseStatus = "KILLED"
	HouseClosed HouseStatus = "CLOSED"
)

// AllHouseStatuses returns every house book status.
func AllHouseStatuses() []HouseStatus { return []HouseStatus{HouseActive, HouseKilled, HouseClosed} }

// HouseStrategy is what a house book is for.
type HouseStrategy string

// House strategies.
const (
	HouseLiquidityProvision   HouseStrategy = "LIQUIDITY_PROVISION"
	HouseMarketMaking         HouseStrategy = "MARKET_MAKING"
	HouseHedging              HouseStrategy = "HEDGING"
	HouseInventoryRebalancing HouseStrategy = "INVENTORY_REBALANCING"
	HouseTreasuryConversion   HouseStrategy = "TREASURY_CONVERSION"
)

// AllHouseStrategies returns every house strategy.
func AllHouseStrategies() []HouseStrategy {
	return []HouseStrategy{HouseLiquidityProvision, HouseMarketMaking, HouseHedging, HouseInventoryRebalancing, HouseTreasuryConversion}
}

// CreatorActorTypes are the actor types that may create a facility or a house
// book: never an agent.
func CreatorActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService, security.ActorSystem}
}

type (
	intentKind   struct{}
	effectKind   struct{}
	facilityKind struct{}
	houseKind    struct{}
	evidenceKind struct{}
)

// IntentID identifies an economic intent.
type IntentID = id.ID[intentKind]

// EffectID identifies an economic effect.
type EffectID = id.ID[effectKind]

// FacilityID identifies a facility.
type FacilityID = id.ID[facilityKind]

// HouseAccountID identifies a house book.
type HouseAccountID = id.ID[houseKind]

// NewIntentID returns a fresh intent id.
func NewIntentID() IntentID { return id.New[intentKind]() }

// NewEffectID returns a fresh effect id.
func NewEffectID() EffectID { return id.New[effectKind]() }

// NewFacilityID returns a fresh facility id.
func NewFacilityID() FacilityID { return id.New[facilityKind]() }

// NewHouseAccountID returns a fresh house book id.
func NewHouseAccountID() HouseAccountID { return id.New[houseKind]() }

// ParseIntentID parses the canonical form.
func ParseIntentID(s string) (IntentID, error) { return id.Parse[intentKind](s) }

// ParseEffectID parses the canonical form.
func ParseEffectID(s string) (EffectID, error) { return id.Parse[effectKind](s) }

// ParseFacilityID parses the canonical form.
func ParseFacilityID(s string) (FacilityID, error) { return id.Parse[facilityKind](s) }

// ParseHouseAccountID parses the canonical form.
func ParseHouseAccountID(s string) (HouseAccountID, error) { return id.Parse[houseKind](s) }
