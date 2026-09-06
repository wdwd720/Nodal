package funding

import (
	"context"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
	"github.com/nodal/controlplane/internal/webhook"
)

// ProviderStatus is the provider-neutral session status an adapter reports.
// Adapters map their own strings onto it and keep the raw value in
// Session.RawStatus; anything they do not recognize is ProviderStatusUnknown
// and the service routes the deposit to REVIEW_REQUIRED instead of guessing.
type ProviderStatus string

// Provider statuses.
const (
	// ProviderStatusInitialized: session minted, customer has not started.
	ProviderStatusInitialized ProviderStatus = "INITIALIZED"
	// ProviderStatusCustomerActionRequired: onboarding/payment/quote step
	// with the customer (Stripe requires_payment, quote_ready).
	ProviderStatusCustomerActionRequired ProviderStatus = "CUSTOMER_ACTION_REQUIRED"
	// ProviderStatusProcessing: payment taken, crypto not yet delivered.
	ProviderStatusProcessing ProviderStatus = "PROCESSING"
	// ProviderStatusConfirmed: provider reports crypto delivered.
	ProviderStatusConfirmed ProviderStatus = "CONFIRMED"
	// ProviderStatusRejected: KYC, sanctions or fraud rejection.
	ProviderStatusRejected ProviderStatus = "REJECTED"
	// ProviderStatusUnknown: a status this binary does not understand.
	ProviderStatusUnknown ProviderStatus = "UNKNOWN"
)

// MapProviderStatus returns the deposit status a provider status drives
// the deposit towards, and false when the status is unknown (the caller
// escalates to REVIEW_REQUIRED, never crashes).
func MapProviderStatus(s ProviderStatus) (Status, bool) {
	switch s {
	case ProviderStatusInitialized, ProviderStatusCustomerActionRequired:
		return StatusCustomerActionRequired, true
	case ProviderStatusProcessing:
		return StatusProviderProcessing, true
	case ProviderStatusConfirmed:
		return StatusProviderConfirmed, true
	case ProviderStatusRejected:
		return StatusFailed, true
	}
	return "", false
}

// Session is the provider-neutral view of an onramp session. Monetary
// amounts are the provider's exact decimal strings; the service converts
// them with ParseDecimalAmount at the asset's precision.
type Session struct {
	ID        string
	Status    ProviderStatus
	RawStatus string
	// ClientSecret drives the customer's embedded widget. It is returned to
	// the customer once and never persisted or logged.
	ClientSecret string
	RedirectURL  string
	CreatedAt    time.Time
	Livemode     bool

	DestinationNetwork  string
	DestinationCurrency string
	DestinationAmount   string // decimal text; "" until the provider sets it
	SourceCurrency      string
	SourceAmount        string // decimal text
	WalletAddress       string
	// TransactionID is the provider's fulfillment reference (Stripe cxt_...),
	// set once fulfilled.
	TransactionID      string
	NetworkFee         string
	TransactionFee     string
	KYCDetailsProvided bool
	Metadata           map[string]string
}

// CustomerInformation is optional KYC prefill passed to the provider. It is
// forwarded once and never stored by this package.
type CustomerInformation struct {
	Email     string
	FirstName string
	LastName  string
}

// CreateSessionRequest is the input of FundingProvider.CreateSession.
type CreateSessionRequest struct {
	// IdempotencyKey is sent to the provider so a retried create never
	// mints a second session (the deposit id in practice).
	IdempotencyKey      string
	DestinationNetwork  string // e.g. "solana"
	DestinationCurrency string // e.g. "usdc"
	WalletAddress       string
	LockWalletAddress   bool
	SourceCurrency      string // e.g. "usd"
	SourceAmount        string // decimal text; "" when the customer chooses
	CustomerIPAddress   string
	Customer            *CustomerInformation
	Metadata            map[string]string
}

// WebhookEvent is a verified, parsed provider webhook. SessionKnown is
// false for event types the adapter recognizes as signed but does not
// model (the pipeline records them as IGNORED).
type WebhookEvent struct {
	Identity     webhook.Identity
	Session      Session
	SessionKnown bool
	Raw          []byte
}

// WebhookIdentity implements webhook.Event.
func (e WebhookEvent) WebhookIdentity() webhook.Identity { return e.Identity }

// FundingProvider is the contract a fiat-to-crypto onramp adapter
// implements (PART 29). Signature verification happens inside ParseWebhook;
// the pipeline never sees an unverified event.
type FundingProvider interface {
	Name() string
	Verification() provider.VerificationLabel
	CreateSession(ctx context.Context, req CreateSessionRequest) (Session, error)
	GetSession(ctx context.Context, providerSessionID string) (Session, error)
	ParseWebhook(ctx context.Context, raw []byte, headers http.Header) (WebhookEvent, error)
}

// ChainReceiptQuery asks the observer for a credit of AssetID to Address
// observed at or after Since.
type ChainReceiptQuery struct {
	Address string
	AssetID assets.AssetID
	Since   time.Time
}

// ChainReceipt is an observed on-chain credit.
type ChainReceipt struct {
	Quantity   money.Quantity
	Signature  string
	Slot       int64
	ObservedAt time.Time
}

// ChainReceiptObserver is the funding-local contract for chain observation
// (the wallet/execution packages provide the real one later). It reports
// the observed credit, its signature and slot for a destination address
// since a time; found is false when no credit has been seen yet.
type ChainReceiptObserver interface {
	ObserveCredit(ctx context.Context, q ChainReceiptQuery) (receipt ChainReceipt, found bool, err error)
}
