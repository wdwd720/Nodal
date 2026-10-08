package transfers

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/risk"
)

type transferKind struct{}

// TransferID identifies an internal transfer.
type TransferID = id.ID[transferKind]

// NewTransferID returns a fresh transfer id.
func NewTransferID() TransferID { return id.New[transferKind]() }

// ParseTransferID parses the canonical form.
func ParseTransferID(s string) (TransferID, error) { return id.Parse[transferKind](s) }

// Status is the status of an internal transfer.
type Status string

// Statuses. PENDING exists only inside the transaction that decides the
// transfer: a committed transfer is COMPLETED, REJECTED or REVERSED.
const (
	StatusPending   Status = "PENDING"
	StatusCompleted Status = "COMPLETED"
	StatusRejected  Status = "REJECTED"
	StatusReversed  Status = "REVERSED"
)

var allStatuses = []Status{StatusPending, StatusCompleted, StatusRejected, StatusReversed}

// AllStatuses returns every status in declaration order (a copy). It is the
// Go twin of the CHECK internal_transfers_status_check.
func AllStatuses() []Status { return append([]Status(nil), allStatuses...) }

// Valid reports whether s is a declared status.
func (s Status) Valid() bool {
	for _, x := range allStatuses {
		if x == s {
			return true
		}
	}
	return false
}

// transitions mirrors cp_internal_transfer_transition_legal (migration
// 00744); a parity test compares the two.
var transitions = map[Status][]Status{
	StatusPending:   {StatusCompleted, StatusRejected},
	StatusCompleted: {StatusReversed},
}

// CanTransition reports whether from -> to is legal.
func CanTransition(from, to Status) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transition is legal.
func (s Status) Terminal() bool { return len(transitions[s]) == 0 }

// RuleKind is a per-sender rule about one recipient.
type RuleKind string

// Rule kinds. The latest rule for (sender, recipient) is the one in force;
// CLEAR withdraws an earlier ALLOW or BLOCK.
const (
	RuleAllow RuleKind = "ALLOW"
	RuleBlock RuleKind = "BLOCK"
	RuleClear RuleKind = "CLEAR"
)

// AllRuleKinds returns every rule kind (the Go twin of the CHECK
// transfer_recipient_rules_rule_check).
func AllRuleKinds() []RuleKind { return []RuleKind{RuleAllow, RuleBlock, RuleClear} }

// Valid reports whether r is a declared rule kind.
func (r RuleKind) Valid() bool {
	for _, x := range AllRuleKinds() {
		if x == r {
			return true
		}
	}
	return false
}

// SenderMode says whether a sender may pay anyone or only allowlisted
// recipients.
type SenderMode string

// Sender modes. No recorded mode means OPEN.
const (
	SenderOpen          SenderMode = "OPEN"
	SenderAllowlistOnly SenderMode = "ALLOWLIST_ONLY"
)

// AllSenderModes returns every sender mode (the Go twin of the CHECK
// transfer_sender_modes_mode_check).
func AllSenderModes() []SenderMode { return []SenderMode{SenderOpen, SenderAllowlistOnly} }

// Valid reports whether m is a declared sender mode.
func (m SenderMode) Valid() bool { return m == SenderOpen || m == SenderAllowlistOnly }

// SupportedModes are the execution modes a transfer can be made in: the
// simulated modes the P2P_TRANSFERS gate spec implements. It is the Go twin
// of the CHECK internal_transfers_mode_check.
func SupportedModes() []gates.Mode {
	spec, ok := gates.LookupAdvanced(gates.P2PTransfers)
	if !ok {
		return nil
	}
	return append([]gates.Mode(nil), spec.SimulatedModes...)
}

func supportedMode(m gates.Mode) bool {
	for _, x := range SupportedModes() {
		if x == m {
			return true
		}
	}
	return false
}

// Rejection codes this package assigns. A REJECTED transfer carries one of
// these or a Risk Kernel V2 reason code (RISK_*) as its primary code, and
// every applicable code in reason_codes.
const (
	RejectRouteRefused          = "TRANSFER_ROUTE_REFUSED"
	RejectRecipientBlocked      = "TRANSFER_RECIPIENT_BLOCKED"
	RejectRecipientNotAllowed   = "TRANSFER_RECIPIENT_NOT_ALLOWLISTED"
	RejectLimitPerTransfer      = "TRANSFER_LIMIT_PER_TRANSFER"
	RejectLimitDaily            = "TRANSFER_LIMIT_DAILY"
	RejectVelocityCount         = "TRANSFER_VELOCITY_COUNT"
	RejectVelocityNotional      = "TRANSFER_VELOCITY_NOTIONAL"
	RejectComplianceBlocked     = "TRANSFER_COMPLIANCE_BLOCKED"
	RejectComplianceUnavailable = "TRANSFER_COMPLIANCE_UNAVAILABLE"
	RejectInsufficientFunds     = "TRANSFER_INSUFFICIENT_FUNDS"
)

// AllRejectionCodes returns this package's own rejection codes in
// precedence order: when several apply, the first is the primary code.
func AllRejectionCodes() []string {
	return []string{
		RejectRouteRefused, RejectRecipientBlocked, RejectRecipientNotAllowed, RejectLimitPerTransfer,
		RejectLimitDaily, RejectVelocityCount, RejectVelocityNotional, RejectComplianceBlocked,
		RejectComplianceUnavailable, RejectInsufficientFunds,
	}
}

// Transfer is an internal_transfers row.
type Transfer struct {
	ID                 TransferID
	IntentID           econ.IntentID
	SenderAccountID    accounts.AccountID
	SenderUserID       accounts.UserID
	RecipientAccountID accounts.AccountID
	RecipientHandle    string
	AssetID            assets.AssetID
	Quantity           money.Quantity
	NotionalUSD        money.USD
	Mode               gates.Mode
	Memo               string
	IdempotencyKey     string
	RequestHash        []byte
	PolicyVersion      string
	PolicyHash         string
	Status             Status

	RejectionCode        string
	ReasonCodes          []string
	RiskDecisionID       *risk.DecisionID
	EffectID             *econ.EffectID
	JournalTransactionID *ledger.TransactionID
	ReservationID        string
	EvidenceRef          string
	EvidenceSHA256       []byte
	DecidedAt            *time.Time

	ReversalJournalTransactionID *ledger.TransactionID
	ReversalReasonCode           string
	// ReversedByActorID is the operator who approved the reversal (the
	// second operator of its dual control).
	ReversedByActorID   string
	ReversalEvidenceRef string
	ReversedAt          *time.Time
	// ReversalProposalID names the approved proposal the reversal executed.
	// It is nil for a transfer that is not REVERSED, and for a reversal made
	// before migration 00758 introduced dual control.
	ReversalProposalID *ReversalProposalID

	CorrelationID string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
