package httpapi

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/sandboxfund"
	"github.com/nodal/controlplane/internal/transfers"
)

// Customer-to-customer transfers of simulated value (internal/transfers) and
// the sandbox faucet (internal/sandboxfund), ADR-0020. Every figure behind
// these ports is value domain SIMULATED. The principal travels in ctx to the
// services, which decide every rule themselves: eligibility, the capability
// gate and kill switches, step-up above the transfer policy's threshold, the
// policy's limits and recipient rules, Risk Kernel V2, the compliance screen,
// the reservation of the sender's funds and the faucet's cap.

// TransferView is a transfer with the registry facts a reader needs to render
// its quantity: the asset's symbol and decimals.
type TransferView struct {
	Transfer      transfers.Transfer
	AssetSymbol   string
	AssetDecimals uint8
}

// TransferSend is one send as the client stated it. The value is exactly one
// of Amount, a plain decimal in whole units of the asset that the adapter
// converts with transfers.ParseAmount against the asset's own decimals (never
// rounding), and Quantity, exact base units.
type TransferSend struct {
	SenderAccountID    accounts.AccountID
	RecipientAccountID accounts.AccountID
	RecipientHandle    string
	// AssetID zero means the sandbox dollar (econ.SandboxDollarID).
	AssetID        assets.AssetID
	Amount         string
	Quantity       *money.Quantity
	Mode           gates.Mode
	Memo           string
	IdempotencyKey string
	CorrelationID  string
}

// TransferSent is the answer to a send that moved value.
type TransferSent struct {
	View TransferView
	// Replayed is true when the service had already decided this request under
	// the same idempotency key and wrote nothing new.
	Replayed bool
}

// TransferRecipientRule is a sender's rule about one recipient, named by
// exactly one of a handle (resolved on the server, never echoed as an account
// id) or an account id.
type TransferRecipientRule struct {
	SenderAccountID    accounts.AccountID
	RecipientHandle    string
	RecipientAccountID accounts.AccountID
	Rule               transfers.RuleKind
	Reason             string
}

// AdvancedTransfersPort serves internal/transfers.
type AdvancedTransfersPort interface {
	// Send moves value. A refusal the service records (a REJECTED transfer)
	// is committed and returned as its error, carrying its stable code.
	Send(ctx context.Context, r TransferSend) (TransferSent, error)
	// List pages the transfers an account sent or received, newest first.
	List(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]TransferView, string, error)
	// Get returns one transfer; the service answers NOT_FOUND to anyone but its
	// parties and operators who may read any account.
	Get(ctx context.Context, id transfers.TransferID) (TransferView, error)
	// Reconcile reads the register, the effects and the ledger; a nil asset
	// reconciles every asset.
	Reconcile(ctx context.Context, asset *assets.AssetID) (transfers.ReconcileReport, error)
	RegisterHandle(ctx context.Context, account accounts.AccountID, handle string) (transfers.Handle, error)
	SetRecipientRule(ctx context.Context, r TransferRecipientRule) error
	SetSenderMode(ctx context.Context, account accounts.AccountID, mode transfers.SenderMode) error
}

// --- reversal under dual control ---------------------------------------------
//
// A COMPLETED transfer is reversed by two distinct operators (review finding
// F3): one proposes, a second approves, which executes the reversal exactly
// once in the approval's own transaction; anyone holding the permission may
// reject instead. The transfer service enforces every rule (operator,
// ledger:post_correction, step-up, proposer != approver, one pending proposal
// per transfer) and the database holds the same line (migration 00758).

// TransferReversalProposalView is one reversal proposal with its decision,
// and the transfer it reverses as it is now, as an operator sees it.
type TransferReversalProposalView struct {
	ID         uuid.UUID
	TransferID transfers.TransferID
	Seq        int
	// Status is the proposal's derived status: PENDING, APPROVED (executed)
	// or REJECTED.
	Status         string
	ReasonCode     string
	Reason         string
	ProposedBy     string
	ProposedAt     time.Time
	DecidedBy      string
	DecidedAt      *time.Time
	DecisionReason string
	Transfer       TransferView
}

// TransferReversalProposalPage is one page of proposals, newest first.
type TransferReversalProposalPage struct {
	Items      []TransferReversalProposalView
	NextCursor string
}

// TransferReversalPropose is one operator's proposal to reverse a transfer.
type TransferReversalPropose struct {
	TransferID    transfers.TransferID
	ReasonCode    string
	Reason        string
	CorrelationID string
}

// TransferReversalDecide approves or rejects one proposal. Reason is
// optional for an approval and required for a rejection (the service judges).
type TransferReversalDecide struct {
	ProposalID    uuid.UUID
	Reason        string
	CorrelationID string
}

// TransferReversalOutcome is a proposal after a command. Replayed is true
// when the service had already recorded exactly this and wrote nothing new.
type TransferReversalOutcome struct {
	View     TransferReversalProposalView
	Replayed bool
}

// TransferReversalsPort serves the dual control over transfer reversals.
// Every method acts as the principal in ctx; the service decides who may.
type TransferReversalsPort interface {
	Propose(ctx context.Context, r TransferReversalPropose) (TransferReversalOutcome, error)
	// Approve records the second operator's approval and executes the
	// reversal in the same transaction; a refusal leaves nothing behind.
	Approve(ctx context.Context, r TransferReversalDecide) (TransferReversalOutcome, error)
	Reject(ctx context.Context, r TransferReversalDecide) (TransferReversalOutcome, error)
	// List pages proposals newest first; an empty status lists every status
	// and a nil transfer every transfer.
	List(ctx context.Context, status string, transfer *transfers.TransferID, cursor string, limit int) (TransferReversalProposalPage, error)
	Get(ctx context.Context, id uuid.UUID) (TransferReversalProposalView, error)
}

// FaucetRequest asks the sandbox faucet for sandbox dollars. The value is
// exactly one of Amount (a plain decimal in whole sandbox dollars, never
// rounded) and Quantity (base units).
type FaucetRequest struct {
	AccountID      accounts.AccountID
	Amount         string
	Quantity       *money.Quantity
	IdempotencyKey string
}

// FaucetGrantView is a grant with its asset's symbol and decimals.
type FaucetGrantView struct {
	Grant         sandboxfund.Grant
	AssetSymbol   string
	AssetDecimals uint8
}

// FaucetAllowance is what the faucet would still grant one account of the
// sandbox dollar in the policy's current rolling window, in base units.
type FaucetAllowance struct {
	AccountID     accounts.AccountID
	AssetID       assets.AssetID
	AssetSymbol   string
	AssetDecimals uint8
	Cap           money.Quantity
	Used          money.Quantity
	Remaining     money.Quantity
	Policy        sandboxfund.Policy
	PolicyHash    string
	Environment   string
}

// SandboxFaucetPort serves internal/sandboxfund for the sandbox dollar.
type SandboxFaucetPort interface {
	Grant(ctx context.Context, r FaucetRequest) (FaucetGrantView, error)
	Allowance(ctx context.Context, account accounts.AccountID) (FaucetAllowance, error)
}
