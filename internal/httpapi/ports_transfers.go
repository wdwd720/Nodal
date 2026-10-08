package httpapi

import (
	"context"

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

// TransferReversal asks an operator correction of a COMPLETED transfer.
type TransferReversal struct {
	TransferID    transfers.TransferID
	ReasonCode    string
	Reason        string
	CorrelationID string
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
	Reverse(ctx context.Context, r TransferReversal) (TransferView, error)
	// Reconcile reads the register, the effects and the ledger; a nil asset
	// reconciles every asset.
	Reconcile(ctx context.Context, asset *assets.AssetID) (transfers.ReconcileReport, error)
	RegisterHandle(ctx context.Context, account accounts.AccountID, handle string) (transfers.Handle, error)
	SetRecipientRule(ctx context.Context, r TransferRecipientRule) error
	SetSenderMode(ctx context.Context, account accounts.AccountID, mode transfers.SenderMode) error
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
