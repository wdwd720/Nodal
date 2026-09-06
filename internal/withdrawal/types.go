package withdrawal

import (
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/security"
)

type (
	withdrawalKind struct{}
	transitionKind struct{}
)

// WithdrawalID identifies a withdrawals row.
type WithdrawalID = id.ID[withdrawalKind]

// TransitionID identifies a withdrawal_transitions row.
type TransitionID = id.ID[transitionKind]

// NewWithdrawalID mints a withdrawal id.
func NewWithdrawalID() WithdrawalID { return id.New[withdrawalKind]() }

// ParseWithdrawalID parses the canonical form.
func ParseWithdrawalID(s string) (WithdrawalID, error) { return id.Parse[withdrawalKind](s) }

// NewTransitionID mints a transition id.
func NewTransitionID() TransitionID { return id.New[transitionKind]() }

// DestinationType classifies where value goes. INTERNAL_ACCOUNT is reserved
// for a future customer-to-customer path and is rejected in V1 (goal PART 4).
type DestinationType string

// Destination types.
const (
	DestinationExternalAddress DestinationType = "EXTERNAL_ADDRESS"
	DestinationInternalAccount DestinationType = "INTERNAL_ACCOUNT"
)

// Withdrawal mirrors one withdrawals row.
type Withdrawal struct {
	ID                   WithdrawalID
	AccountID            accounts.AccountID
	AssetID              assets.AssetID
	Quantity             money.Quantity
	DestinationAddress   string
	DestinationValidated bool
	Status               Status
	RequestedByUserID    accounts.UserID
	StepUpVerifiedAt     *time.Time
	ApprovalID           string
	CapabilityCheckRef   string
	ReservationID        string
	TxSignature          string
	JournalTransactionID *ledger.TransactionID
	IdempotencyKey       string
	CorrelationID        string
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// HumanActor is the only caller type Service.Request accepts. It can be
// built solely by HumanFrom, which refuses every non-human actor type, so
// an AGENT principal cannot be expressed as a withdrawal requester at
// compile time. The zero value is not a valid actor.
type HumanActor struct {
	principal security.Principal
	ok        bool
}

// HumanFrom admits USER and OPERATOR principals only. AGENT, SERVICE and
// SYSTEM are refused with FORBIDDEN; an invalid principal with
// UNAUTHENTICATED.
func HumanFrom(p security.Principal) (HumanActor, error) {
	if err := p.Validate(); err != nil {
		return HumanActor{}, errs.Wrap(err, errs.CodeUnauthenticated, "withdrawal: invalid principal")
	}
	switch p.ActorType {
	case security.ActorUser, security.ActorOperator:
		return HumanActor{principal: p, ok: true}, nil
	case security.ActorAgent:
		return HumanActor{}, errs.New(errs.CodeForbidden, "withdrawal: agents can never request withdrawals").
			WithField("actor_type", string(p.ActorType))
	default:
		return HumanActor{}, errs.Newf(errs.CodeForbidden, "withdrawal: actor type %s cannot request withdrawals", p.ActorType).
			WithField("actor_type", string(p.ActorType))
	}
}

// Principal returns the validated principal.
func (h HumanActor) Principal() security.Principal { return h.principal }

// Valid reports whether the actor was built by HumanFrom.
func (h HumanActor) Valid() bool { return h.ok && h.principal.ActorType != security.ActorAgent }

// Request is a customer's withdrawal request.
type Request struct {
	AccountID           accounts.AccountID
	AssetID             assets.AssetID
	Quantity            money.Quantity
	DestinationType     DestinationType
	DestinationAddress  string
	SourceWalletAddress string // the account's own wallet; the destination must differ
	IdempotencyKey      string
	CorrelationID       string
	RequestID           string
}

// Validate checks the request shape, the destination type and the
// destination address (ValidateSolanaAddress; not the source wallet).
func (r Request) Validate() error {
	problems := map[string]any{}
	if r.AccountID.IsZero() {
		problems["account_id"] = "required"
	}
	if r.AssetID.IsZero() {
		problems["asset_id"] = "required"
	}
	if !r.Quantity.IsPositive() {
		problems["quantity"] = "must be positive"
	}
	if strings.TrimSpace(r.IdempotencyKey) == "" {
		problems["idempotency_key"] = "required"
	}
	switch r.DestinationType {
	case DestinationExternalAddress:
	case DestinationInternalAccount:
		return errs.New(errs.CodeUnsupported, "withdrawal: internal account destinations are not available").
			WithField("destination_type", string(r.DestinationType))
	default:
		problems["destination_type"] = "must be EXTERNAL_ADDRESS"
	}
	if err := ValidateSolanaAddress(r.DestinationAddress); err != nil {
		problems["destination_address"] = err.Error()
	} else if r.SourceWalletAddress != "" && r.DestinationAddress == r.SourceWalletAddress {
		problems["destination_address"] = "must differ from the source wallet"
	}
	if len(problems) > 0 {
		return errs.New(errs.CodeValidationFailed, "withdrawal: invalid request").WithFields(problems)
	}
	return nil
}

// TransitionEvidence is who moved the withdrawal and why. AGENT actors are
// refused here and by the table's CHECK constraint.
type TransitionEvidence struct {
	ActorType     security.ActorType
	ActorID       string
	Reason        string
	EvidenceRef   string
	CorrelationID string
	RequestID     string
}

// Validate checks completeness and refuses agents.
func (e TransitionEvidence) Validate() error {
	if !e.ActorType.Valid() {
		return errs.Newf(errs.CodeValidationFailed, "withdrawal: unknown actor type %q", e.ActorType)
	}
	if e.ActorType == security.ActorAgent {
		return errs.New(errs.CodeForbidden, "withdrawal: agents cannot transition withdrawals")
	}
	if strings.TrimSpace(e.ActorID) == "" || strings.TrimSpace(e.Reason) == "" {
		return errs.New(errs.CodeValidationFailed, "withdrawal: actor id and reason are required")
	}
	return nil
}
