package delegation

import (
	"github.com/nodal/controlplane/internal/config"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/devnethost"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/security"
)

type (
	capabilityKind  struct{}
	swapKind        struct{}
	venueKind       struct{}
	usageKind       struct{}
	withdrawalKind  struct{}
	destinationKind struct{}
	policyKind      struct{}
)

// CapabilityID identifies a delegated capability.
type CapabilityID = id.ID[capabilityKind]

// SwapID identifies a delegated swap.
type SwapID = id.ID[swapKind]

// VenueID identifies a delegation venue.
type VenueID = id.ID[venueKind]

// UsageID identifies one use of a capability.
type UsageID = id.ID[usageKind]

// WithdrawalID identifies a delegated withdrawal.
type WithdrawalID = id.ID[withdrawalKind]

// DestinationID identifies an allowlisted withdrawal destination.
type DestinationID = id.ID[destinationKind]

// PolicyID identifies one version of an account's withdrawal policy.
type PolicyID = id.ID[policyKind]

// ParseCapabilityID parses the canonical form.
func ParseCapabilityID(s string) (CapabilityID, error) { return id.Parse[capabilityKind](s) }

// ParseSwapID parses the canonical form.
func ParseSwapID(s string) (SwapID, error) { return id.Parse[swapKind](s) }

// ParseVenueID parses the canonical form.
func ParseVenueID(s string) (VenueID, error) { return id.Parse[venueKind](s) }

// ParseWithdrawalID parses the canonical form.
func ParseWithdrawalID(s string) (WithdrawalID, error) { return id.Parse[withdrawalKind](s) }

// ParseDestinationID parses the canonical form.
func ParseDestinationID(s string) (DestinationID, error) { return id.Parse[destinationKind](s) }

// ActionClass is what a capability lets its agent do.
type ActionClass string

// Action classes. ACQUIRE_EXPOSURE and REDUCE_EXPOSURE are delegated swaps
// (the econ intent kinds of the same names); PROPOSE_WITHDRAWAL lets the agent
// propose -- never execute -- a withdrawal.
const (
	ActionAcquireExposure   ActionClass = "ACQUIRE_EXPOSURE"
	ActionReduceExposure    ActionClass = "REDUCE_EXPOSURE"
	ActionProposeWithdrawal ActionClass = "PROPOSE_WITHDRAWAL"
)

var allActionClasses = []ActionClass{ActionAcquireExposure, ActionReduceExposure, ActionProposeWithdrawal}

// AllActionClasses returns every action class (delegated_capabilities
// action_classes, delegated_capability_usage action_class).
func AllActionClasses() []ActionClass { return append([]ActionClass(nil), allActionClasses...) }

// SwapActionClasses returns the action classes a delegated swap may carry
// (delegated_swaps action_class).
func SwapActionClasses() []ActionClass {
	return []ActionClass{ActionAcquireExposure, ActionReduceExposure}
}

// Valid reports whether a is declared.
func (a ActionClass) Valid() bool { return contains(allActionClasses, a) }

// IsSwap reports whether a is a delegated swap.
func (a ActionClass) IsSwap() bool { return a == ActionAcquireExposure || a == ActionReduceExposure }

// InstrumentClass is the class of asset a capability lets its agent touch.
type InstrumentClass string

// Instrument classes, derived from an asset's registry row (ClassifyAsset):
// a USD-pegged stable unit, an ordinary token, or a speculative token.
const (
	ClassStableValue      InstrumentClass = "STABLE_VALUE"
	ClassSpotToken        InstrumentClass = "SPOT_TOKEN"
	ClassSpeculativeToken InstrumentClass = "SPECULATIVE_TOKEN"
)

var allInstrumentClasses = []InstrumentClass{ClassStableValue, ClassSpotToken, ClassSpeculativeToken}

// AllInstrumentClasses returns every instrument class
// (delegated_capabilities instrument_classes).
func AllInstrumentClasses() []InstrumentClass {
	return append([]InstrumentClass(nil), allInstrumentClasses...)
}

// Valid reports whether c is declared.
func (c InstrumentClass) Valid() bool { return contains(allInstrumentClasses, c) }

// CapabilityStatus is the status of a capability. Expiry is not a status: it
// is decided against the clock every time the capability is used.
type CapabilityStatus string

// Capability statuses.
const (
	CapabilityActive  CapabilityStatus = "ACTIVE"
	CapabilityRevoked CapabilityStatus = "REVOKED"
)

// AllCapabilityStatuses returns every capability status.
func AllCapabilityStatuses() []CapabilityStatus {
	return []CapabilityStatus{CapabilityActive, CapabilityRevoked}
}

var capabilityTransitions = map[CapabilityStatus][]CapabilityStatus{
	CapabilityActive: {CapabilityRevoked},
}

// CanTransitionCapability mirrors cp_delegation_capability_transition_legal.
func CanTransitionCapability(from, to CapabilityStatus) bool {
	return contains(capabilityTransitions[from], to)
}

// SwapStatus is the status of a delegated swap.
type SwapStatus string

// Swap statuses.
const (
	SwapAdmitted         SwapStatus = "ADMITTED"
	SwapAwaitingApproval SwapStatus = "AWAITING_APPROVAL"
	SwapApproved         SwapStatus = "APPROVED"
	SwapSigned           SwapStatus = "SIGNED"
	SwapSubmitted        SwapStatus = "SUBMITTED"
	SwapUnknown          SwapStatus = "UNKNOWN"
	SwapSettled          SwapStatus = "SETTLED"
	SwapRejected         SwapStatus = "REJECTED"
	SwapCancelled        SwapStatus = "CANCELLED"
	SwapFailed           SwapStatus = "FAILED"
)

var allSwapStatuses = []SwapStatus{
	SwapAdmitted, SwapAwaitingApproval, SwapApproved, SwapSigned, SwapSubmitted,
	SwapUnknown, SwapSettled, SwapRejected, SwapCancelled, SwapFailed,
}

// AllSwapStatuses returns every swap status (delegated_swaps status).
func AllSwapStatuses() []SwapStatus { return append([]SwapStatus(nil), allSwapStatuses...) }

// Terminal reports whether nothing further can happen to the swap.
func (s SwapStatus) Terminal() bool {
	return s == SwapSettled || s == SwapRejected || s == SwapCancelled || s == SwapFailed
}

// Signable reports whether the swap may be signed now.
func (s SwapStatus) Signable() bool { return s == SwapAdmitted || s == SwapApproved }

// SwapStatusesBeforeSigning are the statuses in which a swap carries no
// signed transaction (delegated_swaps_signed_has_tx: every other status
// carries one).
func SwapStatusesBeforeSigning() []SwapStatus {
	return []SwapStatus{SwapAdmitted, SwapAwaitingApproval, SwapApproved, SwapRejected, SwapCancelled}
}

// SwapStatusesWithFailureCode are the statuses that carry a failure code,
// and the only ones (delegated_swaps_failure_code).
func SwapStatusesWithFailureCode() []SwapStatus {
	return []SwapStatus{SwapRejected, SwapCancelled, SwapFailed}
}

var swapTransitions = map[SwapStatus][]SwapStatus{
	SwapAdmitted:         {SwapAwaitingApproval, SwapSigned, SwapRejected},
	SwapAwaitingApproval: {SwapApproved, SwapCancelled, SwapRejected},
	SwapApproved:         {SwapSigned, SwapRejected},
	SwapSigned:           {SwapSubmitted, SwapUnknown, SwapFailed},
	SwapSubmitted:        {SwapSettled, SwapFailed},
	SwapUnknown:          {SwapSettled, SwapFailed},
}

// CanTransitionSwap mirrors cp_delegation_swap_transition_legal.
func CanTransitionSwap(from, to SwapStatus) bool { return contains(swapTransitions[from], to) }

// WithdrawalStatus is the status of a delegated withdrawal.
type WithdrawalStatus string

// Withdrawal statuses.
const (
	WithdrawalProposed         WithdrawalStatus = "PROPOSED"
	WithdrawalAwaitingApproval WithdrawalStatus = "AWAITING_APPROVAL"
	WithdrawalApproved         WithdrawalStatus = "APPROVED"
	WithdrawalSigned           WithdrawalStatus = "SIGNED"
	WithdrawalSubmitted        WithdrawalStatus = "SUBMITTED"
	WithdrawalUnknown          WithdrawalStatus = "UNKNOWN"
	WithdrawalSettled          WithdrawalStatus = "SETTLED"
	WithdrawalRejected         WithdrawalStatus = "REJECTED"
	WithdrawalDeclined         WithdrawalStatus = "DECLINED"
	WithdrawalCancelled        WithdrawalStatus = "CANCELLED"
	WithdrawalFailed           WithdrawalStatus = "FAILED"
)

var allWithdrawalStatuses = []WithdrawalStatus{
	WithdrawalProposed, WithdrawalAwaitingApproval, WithdrawalApproved, WithdrawalSigned, WithdrawalSubmitted,
	WithdrawalUnknown, WithdrawalSettled, WithdrawalRejected, WithdrawalDeclined, WithdrawalCancelled, WithdrawalFailed,
}

// AllWithdrawalStatuses returns every withdrawal status
// (delegated_withdrawals status).
func AllWithdrawalStatuses() []WithdrawalStatus {
	return append([]WithdrawalStatus(nil), allWithdrawalStatuses...)
}

// Terminal reports whether nothing further can happen to the withdrawal.
func (s WithdrawalStatus) Terminal() bool {
	switch s {
	case WithdrawalSettled, WithdrawalRejected, WithdrawalDeclined, WithdrawalCancelled, WithdrawalFailed:
		return true
	}
	return false
}

// WithdrawalStatusesBeforeSigning are the statuses in which a withdrawal
// carries no signed transaction (delegated_withdrawals_signed_has_tx).
func WithdrawalStatusesBeforeSigning() []WithdrawalStatus {
	return []WithdrawalStatus{WithdrawalProposed, WithdrawalAwaitingApproval, WithdrawalApproved, WithdrawalRejected,
		WithdrawalDeclined, WithdrawalCancelled}
}

// WithdrawalStatusesWithFailureCode are the statuses that carry a failure
// code, and the only ones (delegated_withdrawals_failure_code).
func WithdrawalStatusesWithFailureCode() []WithdrawalStatus {
	return []WithdrawalStatus{WithdrawalRejected, WithdrawalDeclined, WithdrawalCancelled, WithdrawalFailed}
}

var withdrawalTransitions = map[WithdrawalStatus][]WithdrawalStatus{
	WithdrawalProposed:         {WithdrawalApproved, WithdrawalAwaitingApproval},
	WithdrawalAwaitingApproval: {WithdrawalApproved, WithdrawalDeclined, WithdrawalCancelled, WithdrawalRejected},
	WithdrawalApproved:         {WithdrawalSigned, WithdrawalCancelled, WithdrawalRejected},
	WithdrawalSigned:           {WithdrawalSubmitted, WithdrawalUnknown, WithdrawalFailed},
	WithdrawalSubmitted:        {WithdrawalSettled, WithdrawalFailed},
	WithdrawalUnknown:          {WithdrawalSettled, WithdrawalFailed},
}

// CanTransitionWithdrawal mirrors cp_delegation_withdrawal_transition_legal.
func CanTransitionWithdrawal(from, to WithdrawalStatus) bool {
	return contains(withdrawalTransitions[from], to)
}

// DestinationStatus is the status of an allowlisted withdrawal destination.
type DestinationStatus string

// Destination statuses.
const (
	DestinationActive  DestinationStatus = "ACTIVE"
	DestinationRemoved DestinationStatus = "REMOVED"
)

// AllDestinationStatuses returns every destination status.
func AllDestinationStatuses() []DestinationStatus {
	return []DestinationStatus{DestinationActive, DestinationRemoved}
}

// CanTransitionDestination mirrors cp_delegation_destination_transition_legal.
func CanTransitionDestination(from, to DestinationStatus) bool {
	return from == DestinationActive && to == DestinationRemoved
}

// Decision is a human's (or, for an auto-approvable withdrawal, the
// policy's) answer to an approval request.
type Decision string

// Decisions.
const (
	DecisionApprove Decision = "APPROVE"
	DecisionDecline Decision = "DECLINE"
)

// AllDecisions returns every decision (delegated_swap_approvals and
// delegated_withdrawal_approvals decision).
func AllDecisions() []Decision { return []Decision{DecisionApprove, DecisionDecline} }

// Verdict is the signing provider's decision.
type Verdict string

// Verdicts.
const (
	VerdictApproved Verdict = "APPROVED"
	VerdictRejected Verdict = "REJECTED"
)

// AllVerdicts returns every verdict (delegated_signing_decisions verdict).
func AllVerdicts() []Verdict { return []Verdict{VerdictApproved, VerdictRejected} }

// EvaluationOutcome is what the withdrawal policy decided about a proposal.
// REJECT is never stored on a withdrawal row: a rejected proposal is recorded
// on its intent, its risk decision and its evaluation evidence instead.
type EvaluationOutcome string

// Evaluation outcomes.
const (
	OutcomeAutoApprove   EvaluationOutcome = "AUTO_APPROVE"
	OutcomeHumanRequired EvaluationOutcome = "HUMAN_REQUIRED"
	OutcomeReject        EvaluationOutcome = "REJECT"
)

// StoredEvaluationOutcomes returns the outcomes a withdrawal row can carry
// (delegated_withdrawals evaluation_outcome).
func StoredEvaluationOutcomes() []EvaluationOutcome {
	return []EvaluationOutcome{OutcomeAutoApprove, OutcomeHumanRequired}
}

// SubjectKind names what a capability use or a signing decision is about.
type SubjectKind string

// Subject kinds.
const (
	SubjectSwap       SubjectKind = "SWAP"
	SubjectWithdrawal SubjectKind = "WITHDRAWAL"
)

// AllSubjectKinds returns every subject kind
// (delegated_capability_usage subject_kind).
func AllSubjectKinds() []SubjectKind { return []SubjectKind{SubjectSwap, SubjectWithdrawal} }

// SupportedModes returns the modes a capability, a swap or a withdrawal can
// be in: AGENT_SIGNING and AGENT_WITHDRAWALS implement SANDBOX and DEVNET.
func SupportedModes() []gates.Mode { return []gates.Mode{gates.ModeSandbox, gates.ModeDevnet} }

// Chains returns the simulated chains delegated authority can act on
// (every chain_id and chain_allowlist list of 00752 and 00753).
func Chains() []devnet.ChainID { return devnethost.Chains() }

// Environments returns the deployment environments a capability or a
// withdrawal records (environment columns).
func Environments() []config.Environment {
	return []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev, config.EnvStaging, config.EnvProd}
}

// TestEnvironments are where a withdrawal policy may auto-approve: the
// low-risk test mode of goal §17.
func TestEnvironments() []config.Environment {
	return []config.Environment{config.EnvLocal, config.EnvTest, config.EnvDev}
}

// RevokerActorTypes returns who may revoke a capability
// (delegated_capabilities revoked_by_actor_type) or remove a destination
// (delegated_withdrawal_destinations removed_by_actor_type).
func RevokerActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorSystem}
}

// VenueCreatorActorTypes returns who may register a venue
// (delegation_venues created_by_actor_type).
func VenueCreatorActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorOperator, security.ActorService, security.ActorSystem}
}

// WithdrawalApproverActorTypes returns who may decide a withdrawal
// (delegated_withdrawal_approvals approver_actor_type): the account's owner,
// or the policy service for an auto-approvable proposal.
func WithdrawalApproverActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorService}
}

func contains[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
