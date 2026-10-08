package marketplace

import (
	"strconv"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxrt"
	"github.com/nodal/controlplane/internal/security"
)

// Status is a listing version's lifecycle status.
type Status string

// Statuses.
const (
	StatusDraft      Status = "DRAFT"
	StatusSubmitted  Status = "SUBMITTED"
	StatusValidated  Status = "VALIDATED"
	StatusInReview   Status = "IN_REVIEW"
	StatusApproved   Status = "APPROVED"
	StatusRejected   Status = "REJECTED"
	StatusDeprecated Status = "DEPRECATED"
)

var allStatuses = []Status{StatusDraft, StatusSubmitted, StatusValidated, StatusInReview, StatusApproved, StatusRejected, StatusDeprecated}

// AllStatuses returns every status (marketplace_listing_versions_status_check).
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// Valid reports whether s is declared.
func (s Status) Valid() bool {
	for _, x := range allStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// transitions is the lifecycle. It mirrors cp_marketplace_transition_legal
// (migration 00750); TestIntegration_SQLAndGoAgreeOnTheLifecycle compares
// the two.
var transitions = map[Status][]Status{
	StatusDraft:     {StatusSubmitted},
	StatusSubmitted: {StatusValidated, StatusRejected},
	StatusValidated: {StatusInReview, StatusRejected},
	StatusInReview:  {StatusApproved, StatusRejected},
	StatusApproved:  {StatusDeprecated},
}

// CanTransition reports whether from -> to is a legal lifecycle edge.
func CanTransition(from, to Status) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Moderation is a version's moderation state, separate from its lifecycle:
// an approved version can later be flagged or taken down.
type Moderation string

// Moderation states.
const (
	ModerationUnreviewed Moderation = "UNREVIEWED"
	ModerationCleared    Moderation = "CLEARED"
	ModerationFlagged    Moderation = "FLAGGED"
	ModerationTakenDown  Moderation = "TAKEN_DOWN"
)

var allModeration = []Moderation{ModerationUnreviewed, ModerationCleared, ModerationFlagged, ModerationTakenDown}

// AllModerationStates returns every moderation state
// (marketplace_listing_versions_moderation_state_check).
func AllModerationStates() []Moderation { return append([]Moderation(nil), allModeration...) }

var moderationTransitions = map[Moderation][]Moderation{
	ModerationUnreviewed: {ModerationCleared, ModerationFlagged, ModerationTakenDown},
	ModerationCleared:    {ModerationFlagged, ModerationTakenDown},
	ModerationFlagged:    {ModerationCleared, ModerationTakenDown},
}

// CanModerate reports whether from -> to is a legal moderation edge
// (cp_marketplace_moderation_legal).
func CanModerate(from, to Moderation) bool {
	for _, t := range moderationTransitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// RiskClass is the creator's statement of how risky the strategy is.
type RiskClass string

// Risk classes.
const (
	RiskConservative RiskClass = "CONSERVATIVE"
	RiskModerate     RiskClass = "MODERATE"
	RiskAggressive   RiskClass = "AGGRESSIVE"
	RiskSpeculative  RiskClass = "SPECULATIVE"
)

// AllRiskClasses returns every risk class (marketplace_listing_versions_risk_class_check).
func AllRiskClasses() []RiskClass {
	return []RiskClass{RiskConservative, RiskModerate, RiskAggressive, RiskSpeculative}
}

// Valid reports whether r is declared.
func (r RiskClass) Valid() bool {
	for _, x := range AllRiskClasses() {
		if x == r {
			return true
		}
	}
	return false
}

// InstallStatus is an install's status.
type InstallStatus string

// Install statuses.
const (
	InstallActive  InstallStatus = "ACTIVE"
	InstallRemoved InstallStatus = "REMOVED"
)

// AllInstallStatuses returns every install status (marketplace_installs_status_check).
func AllInstallStatuses() []InstallStatus { return []InstallStatus{InstallActive, InstallRemoved} }

// AllInstallModes returns the only modes an install can have
// (marketplace_installs_mode_check): PAPER. The marketplace never allocates
// real customer money, and the database refuses any other mode.
func AllInstallModes() []gates.Mode { return []gates.Mode{gates.ModePaper} }

// ReviewDecision is a reviewer's decision.
type ReviewDecision string

// Review decisions.
const (
	DecisionApprove ReviewDecision = "APPROVE"
	DecisionReject  ReviewDecision = "REJECT"
)

// AllReviewDecisions returns every decision (marketplace_reviews_decision_check).
func AllReviewDecisions() []ReviewDecision { return []ReviewDecision{DecisionApprove, DecisionReject} }

// ValidationOutcome is the outcome of building and scanning a version.
type ValidationOutcome string

// Validation outcomes.
const (
	ValidationPassed ValidationOutcome = "PASSED"
	ValidationFailed ValidationOutcome = "FAILED"
)

// AllValidationOutcomes returns every outcome (marketplace_version_validations_outcome_check).
func AllValidationOutcomes() []ValidationOutcome {
	return []ValidationOutcome{ValidationPassed, ValidationFailed}
}

// AllValidationFailureReasons returns every failure reason
// (marketplace_version_validations_failure_reason_check): the builder's.
func AllValidationFailureReasons() []string { return sandboxrt.AllBuildFailures() }

// FollowTrigger is what started a follow run.
type FollowTrigger string

// Follow triggers.
const (
	TriggerExplicit  FollowTrigger = "EXPLICIT"
	TriggerScheduled FollowTrigger = "SCHEDULED"
)

// AllFollowTriggers returns every trigger (marketplace_follow_runs_trigger_check).
func AllFollowTriggers() []FollowTrigger { return []FollowTrigger{TriggerExplicit, TriggerScheduled} }

// FollowOutcome is what became of one proposed intent.
type FollowOutcome string

// Follow outcomes.
const (
	OutcomeDispatched      FollowOutcome = "DISPATCHED"
	OutcomeRiskRejected    FollowOutcome = "RISK_REJECTED"
	OutcomeRouteRefused    FollowOutcome = "ROUTE_REFUSED"
	OutcomeDispatchFailed  FollowOutcome = "DISPATCH_FAILED"
	OutcomeDispatchUnknown FollowOutcome = "DISPATCH_UNKNOWN"
)

// AllFollowOutcomes returns every outcome (marketplace_follow_intents_outcome_check).
func AllFollowOutcomes() []FollowOutcome {
	return []FollowOutcome{OutcomeDispatched, OutcomeRiskRejected, OutcomeRouteRefused, OutcomeDispatchFailed, OutcomeDispatchUnknown}
}

// EvaluationKind is the kind of an evaluation.
type EvaluationKind string

// EvaluationSimulation is a run against a fixed historical snapshot set.
const EvaluationSimulation EvaluationKind = "SIMULATION"

// AllEvaluationKinds returns every evaluation kind (marketplace_evaluations_kind_check).
func AllEvaluationKinds() []EvaluationKind { return []EvaluationKind{EvaluationSimulation} }

// CreatorActorTypes may create listings and versions, and install them
// (marketplace_listings_creator_actor_type_check,
// marketplace_listing_versions_created_by_actor_type_check,
// marketplace_installs_installed_by_actor_type_check). Never an agent.
func CreatorActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService}
}

// TransitionActorTypes may move a lifecycle (the actor_type checks of the
// three transition tables, of follow runs, evaluations and validations).
// Never an agent.
func TransitionActorTypes() []security.ActorType {
	return []security.ActorType{security.ActorUser, security.ActorOperator, security.ActorService, security.ActorSystem}
}

// ReviewerActorTypes may review (marketplace_reviews_reviewer_actor_type_check).
func ReviewerActorTypes() []security.ActorType { return []security.ActorType{security.ActorOperator} }

func actorIn(a security.ActorType, list []security.ActorType) bool {
	for _, x := range list {
		if x == a {
			return true
		}
	}
	return false
}

type (
	listingKind    struct{}
	versionKind    struct{}
	installKind    struct{}
	followKind     struct{}
	evaluationKind struct{}
)

// ListingID identifies a listing.
type ListingID = id.ID[listingKind]

// VersionID identifies a listing version.
type VersionID = id.ID[versionKind]

// InstallID identifies an install.
type InstallID = id.ID[installKind]

// FollowRunID identifies a paper-follow run.
type FollowRunID = id.ID[followKind]

// EvaluationID identifies an evaluation.
type EvaluationID = id.ID[evaluationKind]

// FeeModel is fee METADATA: what the creator states they would charge. This
// package never charges it.
type FeeModel struct {
	PerformanceFeeBPS money.BPS `json:"performance_fee_bps"`
	ManagementFeeBPS  money.BPS `json:"management_fee_bps"`
	HighWaterMark     bool      `json:"high_water_mark"`
}

// Listing is a marketplace_listings row.
type Listing struct {
	ID                  ListingID
	CreatorAccountID    accounts.AccountID
	Creator             econ.Actor
	Title               string
	ForkedFromVersionID *VersionID
	CreatedAt           time.Time
}

// Version is a marketplace_listing_versions row.
type Version struct {
	ID                     VersionID
	ListingID              ListingID
	Number                 int
	Status                 Status
	Moderation             Moderation
	Description            string
	RiskClass              RiskClass
	SupportedInstrumentIDs []string
	Manifest               sandboxrt.Manifest
	ManifestHash           []byte
	SourceDigest           []byte
	SourceCommit           string
	BacktestEvidenceRef    string
	DataDependencies       []string
	Fees                   FeeModel
	Config                 map[string]string
	CreatedBy              econ.Actor
	RejectionCode          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// StrategyRef is how intents name the version: "<listing id>@<version>".
func (v Version) StrategyRef() string { return v.ListingID.String() + "@" + strconv.Itoa(v.Number) }

// Followable reports whether the version may be installed and followed.
func (v Version) Followable() bool {
	return v.Status == StatusApproved && v.Moderation == ModerationCleared
}

// Install is a marketplace_installs row.
type Install struct {
	ID          InstallID
	AccountID   accounts.AccountID
	VersionID   VersionID
	Mode        gates.Mode
	Status      InstallStatus
	InstalledBy econ.Actor
	// PaperCapital is what the install's paper book started with, in
	// simulated dollars (an exact decimal string).
	PaperCapital string
	CreatedAt    time.Time
}
