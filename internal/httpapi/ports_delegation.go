package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/delegation"
	"github.com/nodal/controlplane/internal/devnet"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/money"
)

// Delegated agent authority (internal/delegation; ADR-0020, goal §16 and
// §17). Every figure behind this port is value domain SIMULATED.
//
// The port serves an account's OWNER and the OPERATORS, never an agent. An
// agent's swap request and withdrawal proposal are admitted in-process, by the
// agent runtime calling delegation.Service.RequestSwap and ProposeWithdrawal
// under the agent's own principal; nothing here offers them, and no route of
// the HTTP surface admits an agent principal at all (authz.go). Everything an
// agent can never do -- sign, execute, decide, read a key, grant or widen a
// capability -- is equally absent: the executions this port runs are the
// service's own, after an owner's approval or an operator's run.
//
// The delegation service decides every rule, figure and refusal. The adapter
// chooses the transactions and projects the delegation tables read-only where
// the package exposes no listing of its own (swaps, withdrawals, destinations,
// policies, uses, approvals, signing decisions, transitions, venues).
type DelegationPort interface {
	// Reads of one account; the caller has scoped the account.
	Agents(ctx context.Context, account accounts.AccountID) ([]DelegationAgentView, error)
	Capabilities(ctx context.Context, account accounts.AccountID, cursor string, limit int) (DelegationCapabilityPage, error)
	Destinations(ctx context.Context, account accounts.AccountID) ([]delegation.Destination, error)
	WithdrawalPolicy(ctx context.Context, account accounts.AccountID) (DelegationPolicyView, error)
	Swaps(ctx context.Context, account accounts.AccountID, status delegation.SwapStatus, cursor string, limit int) (DelegationSwapPage, error)
	Withdrawals(ctx context.Context, account accounts.AccountID, status delegation.WithdrawalStatus, cursor string, limit int) (DelegationWithdrawalPage, error)

	// Records read by their own id. The caller scopes each by the record's
	// own account, and answers NOT_FOUND for one it may not see.
	Capability(ctx context.Context, id delegation.CapabilityID) (DelegationCapabilityView, error)
	Destination(ctx context.Context, id delegation.DestinationID) (delegation.Destination, error)
	Swap(ctx context.Context, id delegation.SwapID) (DelegationSwapView, error)
	Withdrawal(ctx context.Context, id delegation.WithdrawalID) (DelegationWithdrawalView, error)

	// Venues lists the registered venues and the co-signers this deployment
	// holds.
	Venues(ctx context.Context) (DelegationVenuesView, error)

	// The account owner's commands, each one service operation in its own
	// transaction under the caller's principal.
	Grant(ctx context.Context, r delegation.GrantRequest) (DelegationCapabilityView, error)
	Revoke(ctx context.Context, r delegation.RevokeRequest) (DelegationCapabilityView, error)
	AddDestination(ctx context.Context, r delegation.DestinationRequest) (delegation.Destination, error)
	RemoveDestination(ctx context.Context, id delegation.DestinationID, reason string) (delegation.Destination, error)
	SetWithdrawalPolicy(ctx context.Context, p delegation.WithdrawalPolicy) (delegation.WithdrawalPolicy, error)
	// DecideSwap records the owner's decision and, for an approval, executes
	// the swap under the service's own authority.
	DecideSwap(ctx context.Context, d delegation.SwapDecision) (DelegationSwapOutcome, error)
	// DecideWithdrawal records the owner's decision and, for an approval,
	// executes the withdrawal under the service's own authority.
	DecideWithdrawal(ctx context.Context, d delegation.WithdrawalDecision) (DelegationWithdrawalOutcome, error)
	CancelWithdrawal(ctx context.Context, id delegation.WithdrawalID, reason string) (DelegationWithdrawalView, error)

	// Operator reads across accounts, and operator actions.
	AllCapabilities(ctx context.Context, status delegation.CapabilityStatus, cursor string, limit int) (DelegationCapabilityPage, error)
	Pending(ctx context.Context, limit int) (DelegationPendingView, error)
	RegisterVenue(ctx context.Context, r DelegationVenueRegistration) (DelegationVenueView, error)
	// ExecutePending executes the swaps and withdrawals that can be signed
	// now, as delegation.Service.ExecutableSubjects chooses them: fairly
	// across accounts (at most Params.ExecutablePerAccount of one account per
	// run), swaps with the soonest-expiring quote first, and never a
	// withdrawal still in its cooling period or a subject of another network.
	ExecutePending(ctx context.Context, limit int) ([]DelegationExecutionResult, error)
}

// DelegationAgentView is one of an account's agents.
type DelegationAgentView struct {
	ID         string
	Name       string
	Stage      string
	State      string
	StrategyID string
	CreatedAt  time.Time
}

// DelegationUsage is what a capability has used, measured from its usage
// rows: on Day (the UTC day of the read) and in all.
type DelegationUsage struct {
	Day       time.Time
	Uses      int
	UsedToday money.USD
	UsedTotal money.USD
}

// DelegationUse is one use of a capability.
type DelegationUse struct {
	ID          string
	SubjectKind delegation.SubjectKind
	SubjectID   string
	Action      delegation.ActionClass
	Notional    money.USD
	UsedAt      time.Time
}

// DelegationCapabilityView is a capability with its agent's name and its
// use as of AsOf. Uses is present when the capability was read by its id.
type DelegationCapabilityView struct {
	Capability delegation.Capability
	AgentName  string
	Usage      DelegationUsage
	Uses       []DelegationUse
	AsOf       time.Time
}

// DelegationCapabilityPage is one page of capabilities, newest first.
type DelegationCapabilityPage struct {
	Items      []DelegationCapabilityView
	NextCursor string
}

// DelegationPolicyView is an account's withdrawal policy versions, newest
// first; the first is the one in force. Empty when the account has none.
type DelegationPolicyView struct {
	AccountID accounts.AccountID
	Versions  []delegation.WithdrawalPolicy
}

// DelegationAssetFacts names an asset and the unit its amounts are in.
type DelegationAssetFacts struct {
	ID       assets.AssetID
	Symbol   string
	Decimals uint8
}

// DelegationApprovalView is the one decision recorded on a swap or a
// withdrawal: the owner's, or the policy service's for an auto-approval.
type DelegationApprovalView struct {
	Decision        delegation.Decision
	PlanHash        []byte
	Approver        delegation.Actor
	AuthenticatedAt *time.Time
	DecidedAt       time.Time
	Reason          string
}

// DelegationInspectionView is the signing provider's one decision about a
// swap or a withdrawal: the inspection result.
type DelegationInspectionView struct {
	ID               string
	Verdict          delegation.Verdict
	ReasonCodes      []string
	SignerPublicKey  string
	CapabilityHash   []byte
	PlanHash         []byte
	MessageSHA256    []byte
	PolicyVersion    string
	PolicyHash       string
	InspectorVersion string
	DecidedAt        time.Time
}

// DelegationTransitionView is one status change of a swap or a withdrawal.
type DelegationTransitionView struct {
	From   string
	To     string
	Actor  delegation.Actor
	Reason string
	At     time.Time
}

// DelegationVenueFacts names a swap's venue.
type DelegationVenueFacts struct {
	ID      delegation.VenueID
	Name    string
	Address string
}

// DelegationSwapView is a signing request: the swap with its assets and
// venue, and its decision trail.
type DelegationSwapView struct {
	Swap         delegation.Swap
	Venue        DelegationVenueFacts
	PayAsset     DelegationAssetFacts
	ReceiveAsset DelegationAssetFacts
	Approval     *DelegationApprovalView
	Inspection   *DelegationInspectionView
	Transitions  []DelegationTransitionView
}

// DelegationSwapPage is one page of swaps, newest first.
type DelegationSwapPage struct {
	Items      []DelegationSwapView
	NextCursor string
}

// DelegationDestinationFacts is a withdrawal's destination as the allow-list
// holds it at the read.
type DelegationDestinationFacts struct {
	ID     delegation.DestinationID
	Label  string
	Status delegation.DestinationStatus
}

// DelegationWithdrawalView is a withdrawal with its asset, destination,
// policy version and decision trail.
type DelegationWithdrawalView struct {
	Withdrawal    delegation.Withdrawal
	Asset         DelegationAssetFacts
	Destination   DelegationDestinationFacts
	PolicyVersion int
	Approval      *DelegationApprovalView
	Inspection    *DelegationInspectionView
	Transitions   []DelegationTransitionView
}

// DelegationWithdrawalPage is one page of withdrawals, newest first.
type DelegationWithdrawalPage struct {
	Items      []DelegationWithdrawalView
	NextCursor string
}

// DelegationExecution is whether a decision was followed by an execution
// and, when that execution did not run to its end, the error it ended with.
// The decision stands either way: it was committed first.
type DelegationExecution struct {
	Attempted bool
	Err       error
}

// DelegationSwapOutcome is a swap after its owner's decision.
type DelegationSwapOutcome struct {
	Swap      DelegationSwapView
	Execution DelegationExecution
}

// DelegationWithdrawalOutcome is a withdrawal after its owner's decision.
type DelegationWithdrawalOutcome struct {
	Withdrawal DelegationWithdrawalView
	Execution  DelegationExecution
}

// DelegationPendingView is every swap and withdrawal not yet at an end,
// across accounts, oldest first.
type DelegationPendingView struct {
	Swaps       []DelegationSwapView
	Withdrawals []DelegationWithdrawalView
	AsOf        time.Time
}

// DelegationVenueView is a registered venue and whether this deployment
// holds its co-signer.
type DelegationVenueView struct {
	Venue    delegation.Venue
	CoSigner bool
}

// DelegationCoSignerView is a venue co-signer this deployment holds.
type DelegationCoSignerView struct {
	Name       string
	Chain      devnet.ChainID
	Address    devnet.PublicKey
	Registered bool
}

// DelegationVenuesView is the registered venues and the deployment's
// co-signers.
type DelegationVenuesView struct {
	Venues    []DelegationVenueView
	CoSigners []DelegationCoSignerView
}

// DelegationVenueRegistration registers a venue this deployment co-signs
// for. Its chain and settlement address are the co-signer's; a nil house book
// gets a dedicated one.
type DelegationVenueRegistration struct {
	Name           string
	HouseAccountID *econ.HouseAccountID
	SpreadBPS      money.BPS
	Actor          delegation.Actor
}

// DelegationExecutionResult is what an execution run did to one subject.
type DelegationExecutionResult struct {
	Kind         delegation.SubjectKind
	ID           string
	AccountID    accounts.AccountID
	StatusBefore string
	Status       string
	Err          error
}
