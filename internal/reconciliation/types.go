package reconciliation

import (
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

// Kind is what a record compares. The set matches the CHECK constraint of
// reconciliation_records (migration 00301).
type Kind string

// Kinds.
const (
	// KindExecution compares an order's expected fills, quantities and fees
	// with the transaction actually observed on chain (PARTS 48, 49).
	KindExecution Kind = "EXECUTION"
	// KindWalletBalance compares the WALLET ledger balance of one (account,
	// asset) with the balance observed on chain.
	KindWalletBalance Kind = "WALLET_BALANCE"
	// KindFunding compares a deposit's provider status and expected quantity
	// with the observed chain receipt.
	KindFunding Kind = "FUNDING"
	// KindLedgerInternal is a drift between Σ journal entries and
	// ledger_balances, or between Σ active reservations and the totals row.
	KindLedgerInternal Kind = "LEDGER_INTERNAL"
	// KindPositionLedger is a drift between Σ open lots and the WALLET
	// ledger balance of an asset.
	KindPositionLedger Kind = "POSITION_LEDGER"
	// KindSubmissionUnknown is an attempt whose outcome is not yet known, or
	// wallet activity that matches no attempt (PART 48).
	KindSubmissionUnknown Kind = "SUBMISSION_UNKNOWN"
)

var allKinds = []Kind{
	KindExecution, KindWalletBalance, KindFunding, KindLedgerInternal, KindPositionLedger, KindSubmissionUnknown,
}

// AllKinds returns every kind in declaration order.
func AllKinds() []Kind { return append([]Kind(nil), allKinds...) }

// Valid reports whether k is a declared kind.
func (k Kind) Valid() bool {
	for _, x := range allKinds {
		if x == k {
			return true
		}
	}
	return false
}

// AlwaysMaterial reports whether a mismatch of this kind is material
// regardless of its magnitude. Internal drift (RECONCILIATION.md §6) and an
// unknown submission (PART 48) are never "small".
func (k Kind) AlwaysMaterial() bool {
	return k == KindLedgerInternal || k == KindPositionLedger || k == KindSubmissionUnknown
}

// Mode is how the comparison was triggered (PART 50).
type Mode string

// Modes.
const (
	// ModeEventDriven runs after an execution or funding action.
	ModeEventDriven Mode = "EVENT_DRIVEN"
	// ModePeriodic sweeps provider/account windows since the last checkpoint.
	ModePeriodic Mode = "PERIODIC"
	// ModeFull compares every balance of an account.
	ModeFull Mode = "FULL"
)

var allModes = []Mode{ModeEventDriven, ModePeriodic, ModeFull}

// AllModes returns every mode in declaration order.
func AllModes() []Mode { return append([]Mode(nil), allModes...) }

// Valid reports whether m is a declared mode.
func (m Mode) Valid() bool {
	for _, x := range allModes {
		if x == m {
			return true
		}
	}
	return false
}

// Status is the PART 51 record state.
type Status string

// Statuses.
const (
	// StatusNone is not a stored status: it is the from_status of the
	// transition row written when a record is created, so that every row in
	// reconciliation_records has a complete transition history.
	StatusNone Status = "NONE"

	StatusOpen              Status = "OPEN"
	StatusMatched           Status = "MATCHED"
	StatusMismatch          Status = "MISMATCH"
	StatusInvestigating     Status = "INVESTIGATING"
	StatusResolvedAutomatic Status = "RESOLVED_AUTOMATIC"
	StatusResolvedManual    Status = "RESOLVED_MANUAL"
	StatusEscalated         Status = "ESCALATED"
)

var allStatuses = []Status{
	StatusOpen, StatusMatched, StatusMismatch, StatusInvestigating,
	StatusResolvedAutomatic, StatusResolvedManual, StatusEscalated,
}

// AllStatuses returns every stored status in declaration order (StatusNone is
// not a stored status and is excluded).
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// Transitions is the explicit PART 51 state machine
// (docs/architecture/RECONCILIATION.md §2):
//
//	OPEN → MATCHED
//	OPEN → MISMATCH → INVESTIGATING → RESOLVED_AUTOMATIC | RESOLVED_MANUAL | ESCALATED
//	ESCALATED → INVESTIGATING → RESOLVED_MANUAL
//
// MISMATCH → RESOLVED_AUTOMATIC is allowed because automatic resolution is a
// deterministic, policy-enumerated fix that needs no investigation; MISMATCH →
// ESCALATED lets an unattended engine raise a record straight to an operator.
// RESOLVED_MANUAL is reachable only from INVESTIGATING: a human must have
// looked at the evidence before resolving.
var Transitions = map[Status][]Status{
	StatusOpen:              {StatusMatched, StatusMismatch},
	StatusMatched:           {},
	StatusMismatch:          {StatusInvestigating, StatusResolvedAutomatic, StatusEscalated},
	StatusInvestigating:     {StatusResolvedAutomatic, StatusResolvedManual, StatusEscalated},
	StatusEscalated:         {StatusInvestigating},
	StatusResolvedAutomatic: {},
	StatusResolvedManual:    {},
}

// Valid reports whether s is a declared, stored status.
func (s Status) Valid() bool {
	_, ok := Transitions[s]
	return ok
}

// Terminal reports whether no transition leaves s.
func (s Status) Terminal() bool { return s.Valid() && len(Transitions[s]) == 0 }

// Unresolved reports whether the record still needs attention. It matches the
// partial index reconciliation_records_open_idx of migration 00301.
func (s Status) Unresolved() bool {
	switch s {
	case StatusOpen, StatusMismatch, StatusInvestigating, StatusEscalated:
		return true
	}
	return false
}

// CanTransition reports whether from → to is in the table.
func CanTransition(from, to Status) bool {
	for _, s := range Transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

func checkTransition(recordID RecordID, from, to Status) error {
	if CanTransition(from, to) {
		return nil
	}
	return errs.Newf(errs.CodeInvalidStateTransition, "reconciliation record is %s; %s -> %s is not allowed", from, from, to).
		WithField("record_id", recordID.String()).WithField("from", string(from)).WithField("to", string(to))
}

// Scope types. scope_type/scope_id name the thing being reconciled; they are
// free text in the schema and enumerated here so operators and queries agree.
const (
	ScopeOrder     = "order"
	ScopeAttempt   = "execution_attempt"
	ScopeDeposit   = "deposit"
	ScopeWallet    = "wallet_asset"
	ScopeAccount   = "account"
	ScopeSignature = "tx_signature"
	ScopeLedger    = "ledger_account"
	ScopeSystem    = "system"
)

// Record mirrors one reconciliation_records row.
type Record struct {
	ID        RecordID
	Kind      Kind
	Mode      Mode
	ScopeType string
	ScopeID   string
	// AccountID and AssetID are zero when the record is not scoped to one.
	AccountID accounts.AccountID
	AssetID   assets.AssetID

	// Expected, Observed and Difference are JSON objects. Expected is
	// internal accounting truth, Observed is external truth, Difference is
	// the derived delta an operator reads first.
	Expected   json.RawMessage
	Observed   json.RawMessage
	Difference json.RawMessage

	Status        Status
	Material      bool
	BlocksNewRisk bool

	OpenedAt   time.Time
	MatchedAt  *time.Time
	ResolvedAt *time.Time

	ResolvedByActorType     security.ActorType
	ResolvedByActorID       string
	ResolutionReason        string
	ResolutionEvidenceRef   string
	ApprovalID              string // uuid text; "" when none
	CompensatingJournalTxID string // uuid text; "" when none

	CorrelationID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Transition is one reconciliation_transitions row. The table is immutable
// (forbid_mutation trigger) and every status change writes exactly one row in
// the same transaction as the change (migration 00603, SQLSTATE AU001).
type Transition struct {
	ID          TransitionID
	RecordID    RecordID
	From        Status
	To          Status
	ActorType   security.ActorType
	ActorID     string
	Reason      string
	EvidenceRef string
	OccurredAt  time.Time
}

// BalanceObservation mirrors one wallet_balance_observations row: what one
// observer saw for one (wallet, asset) at one moment.
type BalanceObservation struct {
	ID         ObservationID
	WalletID   string // uuid text
	AssetID    assets.AssetID
	Quantity   money.Quantity
	Source     string
	Slot       *int64
	ObservedAt time.Time
	ReceivedAt time.Time
	RawRef     string
}

// Actor identifies who caused a transition. SYSTEM/"reconciliation" is the
// default for engine-driven transitions; a manual resolution always carries a
// real operator.
type Actor struct {
	Type security.ActorType
	ID   string
}

// SystemActor is the engine itself.
func SystemActor() Actor { return Actor{Type: security.ActorSystem, ID: ActorName} }

// ActorName is the actor id the engine records for its own transitions.
const ActorName = "reconciliation"

// normalize fills in the system defaults for an unnamed actor.
func (a Actor) normalize() Actor {
	if a.Type == "" {
		a.Type = security.ActorSystem
	}
	if a.ID == "" {
		a.ID = ActorName
	}
	return a
}

// validate refuses agents and unknown actor types. An AGENT may never cause a
// reconciliation transition: the database CHECK on resolved_by_actor_type
// enforces the same rule for resolutions, and this enforces it in Go for
// every transition (PART 51, CONVENTIONS "Agent authority").
func (a Actor) validate() error {
	if !a.Type.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "reconciliation: unknown actor type %q", a.Type).
			WithField("actor_type", string(a.Type))
	}
	if a.Type == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "reconciliation: an agent may never change a reconciliation record").
			WithField("actor_type", string(a.Type))
	}
	if a.ID == "" {
		return errs.New(errs.CodeValidationFailed, "reconciliation: actor id is required")
	}
	return nil
}

// AutoCause enumerates the only causes for which a record may resolve without
// a human (RECONCILIATION.md §2). Anything not in this set needs an operator.
type AutoCause string

// Automatic resolution causes.
const (
	// AutoCauseFeeDust is a difference at or below the policy dust threshold
	// for the asset. It posts a RECONCILIATION_ADJUSTMENT when the policy
	// allows automatic dust posting, otherwise it resolves with no economic
	// effect and the difference recorded.
	AutoCauseFeeDust AutoCause = "FEE_DUST"
	// AutoCauseFinalityUpgrade is an observation that only strengthened
	// finality (OBSERVED → FINALIZED); there is no economic change.
	AutoCauseFinalityUpgrade AutoCause = "FINALITY_UPGRADE"
	// AutoCauseDuplicateProviderEvent is a provider event the inbox had
	// already processed.
	AutoCauseDuplicateProviderEvent AutoCause = "DUPLICATE_PROVIDER_EVENT"
	// AutoCauseObservationCaughtUp is a difference that disappeared on
	// re-observation: internal and external now agree exactly.
	AutoCauseObservationCaughtUp AutoCause = "OBSERVATION_CAUGHT_UP"
)

var allAutoCauses = []AutoCause{
	AutoCauseFeeDust, AutoCauseFinalityUpgrade, AutoCauseDuplicateProviderEvent, AutoCauseObservationCaughtUp,
}

// AllAutoCauses returns every enumerated cause in declaration order.
func AllAutoCauses() []AutoCause { return append([]AutoCause(nil), allAutoCauses...) }

// Valid reports whether c is enumerated.
func (c AutoCause) Valid() bool {
	for _, x := range allAutoCauses {
		if x == c {
			return true
		}
	}
	return false
}

// ManualResolution is the input to Resolver.ResolveManual. Operator, Reason
// and EvidenceRef are always mandatory (the database CHECK repeats it);
// ApprovalID is mandatory when the record is material.
type ManualResolution struct {
	// Operator is the resolving principal. ActorType must not be AGENT.
	Operator Actor
	Reason   string
	// EvidenceRef points at what the operator looked at: an archived raw
	// response, an incident id, a support ticket.
	EvidenceRef string
	// ApprovalID is an admin_actions id of kind RECONCILIATION_RESOLVE_MATERIAL
	// whose target is this record. Required when Record.Material.
	ApprovalID string
	// Compensation, when set, is posted before the record is resolved and its
	// journal transaction id is stored on the record. It is the only way a
	// resolution changes financial state (PART 195).
	Compensation *Repair
}

// MinReasonLength is the shortest acceptable resolution reason. A reason is
// evidence: "ok" is not one.
const MinReasonLength = 8

// Validate checks the structural rules that do not need the record.
func (m ManualResolution) Validate() error {
	op := m.Operator.normalize()
	if err := op.validate(); err != nil {
		return err
	}
	problems := map[string]any{}
	if op.Type != security.ActorOperator && op.Type != security.ActorUser {
		problems["actor_type"] = "manual resolution requires a human operator"
	}
	if len([]rune(m.Reason)) < MinReasonLength {
		problems["reason"] = "at least 8 characters"
	}
	if m.EvidenceRef == "" {
		problems["evidence_ref"] = "required"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "reconciliation: invalid manual resolution").WithFields(problems)
	}
	return nil
}
