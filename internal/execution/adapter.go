package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
)

// ExecutionAdapter is the provider-neutral execution contract (PART 41,
// SETTLEMENT_COMPILER §6). A provider implementation owns the venue protocol,
// route decoding, transaction expiry, partial-fill semantics, fee
// interpretation, provider request ids, status mapping and cancellation
// semantics; nothing provider-specific is visible through this interface.
//
// Retry classes (PART 106), also available as MethodRetryClass:
//
//	Quote, ValidateQuote, Status, Reconcile  SAFE_RETRY          idempotent reads
//	Build                                    SAFE_RETRY          pure construction, nothing is sent
//	Submit                                   UNKNOWN_EFFECT_WRITE never retried without a status investigation
//	Cancel                                   IDEMPOTENT_WRITE    only where the venue guarantees it; UNSUPPORTED for atomic-swap venues
//
// Submit reports a transport timeout or any ambiguous outcome as an
// *errs.Error with code SUBMISSION_STATE_UNKNOWN (PART 48). It reports a
// definitive rejection with any other code; the caller still verifies the
// signature is absent from the chain before treating it as a failure.
//
// Name returns the provider name used as the health and kill-switch key.
type ExecutionAdapter interface {
	Name() string
	Quote(ctx context.Context, req QuoteRequest) (QuoteSnapshot, error)
	ValidateQuote(ctx context.Context, q QuoteSnapshot) error
	Build(ctx context.Context, req BuildRequest) (UnsignedAction, error)
	Submit(ctx context.Context, req SignedSubmission) (SubmissionResult, error)
	Status(ctx context.Context, ref ExternalReference) (ExecutionStatus, error)
	Cancel(ctx context.Context, ref ExternalReference) error
	Reconcile(ctx context.Context, scope ReconcileScope) ([]ExternalExecutionEvent, error)
}

// AdapterMethod names one ExecutionAdapter method for the retry matrix.
type AdapterMethod string

// Adapter methods.
const (
	MethodQuote         AdapterMethod = "Quote"
	MethodValidateQuote AdapterMethod = "ValidateQuote"
	MethodBuild         AdapterMethod = "Build"
	MethodSubmit        AdapterMethod = "Submit"
	MethodStatus        AdapterMethod = "Status"
	MethodCancel        AdapterMethod = "Cancel"
	MethodReconcile     AdapterMethod = "Reconcile"
)

// methodRetryClasses is the PART 106 matrix for the adapter contract.
var methodRetryClasses = map[AdapterMethod]provider.RetryClass{
	MethodQuote:         provider.SafeRetry,
	MethodValidateQuote: provider.SafeRetry,
	MethodBuild:         provider.SafeRetry,
	MethodSubmit:        provider.UnknownEffectWrite,
	MethodStatus:        provider.SafeRetry,
	MethodCancel:        provider.IdempotentWrite,
	MethodReconcile:     provider.SafeRetry,
}

// AllAdapterMethods returns every method in contract order.
func AllAdapterMethods() []AdapterMethod {
	return []AdapterMethod{MethodQuote, MethodValidateQuote, MethodBuild, MethodSubmit, MethodStatus, MethodCancel, MethodReconcile}
}

// MethodRetryClass returns the retry class of an adapter method. Unknown
// methods are UNKNOWN_EFFECT_WRITE: an unclassified operation may never be
// retried blindly.
func MethodRetryClass(m AdapterMethod) provider.RetryClass {
	if c, ok := methodRetryClasses[m]; ok {
		return c
	}
	return provider.UnknownEffectWrite
}

// Side is the direction of an order relative to the instrument's base asset.
type Side string

// Sides.
const (
	SideBuy  Side = "BUY"
	SideSell Side = "SELL"
)

// Valid reports whether s is BUY or SELL.
func (s Side) Valid() bool { return s == SideBuy || s == SideSell }

// QuoteRequest asks a venue for an executable quote. Quantities are exact
// base units of the named assets; the mints are the venue-facing identifiers
// of the same assets. Slippage is the maximum the caller will accept, in
// basis points.
type QuoteRequest struct {
	IntentID       string
	PlanID         string
	InstrumentID   string
	VenueListingID string
	VenueNativeID  string
	Side           Side
	InputAsset     assets.AssetID
	InputMint      string
	InputQuantity  money.Quantity
	OutputAsset    assets.AssetID
	OutputMint     string
	MinOutput      money.Quantity
	MaxSlippageBPS money.BPS
	WalletAddress  string
	// PlatformFeeBPS is disclosed to the venue only when the venue collects
	// the platform fee on the platform's behalf; otherwise zero.
	PlatformFeeBPS money.BPS
	Deadline       time.Time
}

// QuoteSnapshot is the local projection of a venue quote (PART 42). It
// mirrors the quotes table of migration 00201 so the integrator can map it
// onto internal/quote without loss. Expected output is the venue's own
// quoted output: there is no hidden spread, and the platform fee is a
// separate, disclosed deduction (PART 126).
type QuoteSnapshot struct {
	ID                string
	IntentID          string
	Provider          string
	ProviderRequestID string
	InstrumentID      string
	VenueListingID    string
	Side              Side
	InputAsset        assets.AssetID
	InputQuantity     money.Quantity
	OutputAsset       assets.AssetID
	ExpectedOutput    money.Quantity
	MinimumOutput     money.Quantity
	EffectivePrice    money.Price
	PriceImpactBPS    money.BPS
	SlippageBPS       money.BPS
	EstNetworkCost    money.Quantity
	EstNetworkAsset   assets.AssetID
	EstVenueFee       money.Quantity
	EstVenueFeeAsset  assets.AssetID
	PlatformFee       money.Quantity
	PlatformFeeAsset  assets.AssetID
	PlatformFeeBPS    money.BPS
	FeePolicyVersion  string
	ReceivedAt        time.Time
	ExpiresAt         time.Time
	RouteHash         []byte
	RouteSummary      json.RawMessage
	RawResponseRef    string
	RawResponseHash   []byte
}

// BuildRequest asks the adapter to construct the unsigned transaction for a
// validated quote. The expectations the inspector will enforce are passed so
// the adapter can set the encoded minimum output and slippage consistently.
type BuildRequest struct {
	PlanID                 string
	PlanHash               []byte
	AttemptNo              int32
	Quote                  QuoteSnapshot
	WalletAddress          string
	MinOutput              money.Quantity
	MaxSlippageBPS         money.BPS
	MaxPriorityFeeLamports money.Quantity
	MaxComputeUnits        uint32
	Deadline               time.Time
}

// UnsignedAction is a built, unsigned transaction with the metadata the
// inspector and the timeout rule need (SETTLEMENT_COMPILER §6).
type UnsignedAction struct {
	Chain                string
	Bytes                []byte
	Hash                 []byte
	RecentBlockhash      string
	LastValidBlockHeight uint64
	ExpiresAt            time.Time
	ProviderRequestID    string
	RawRef               string
}

// SignedSubmission is what Submit sends: the signed bytes and their
// signature, bound to the attempt they belong to. The signature is known
// before submission so a lost response can still be investigated.
type SignedSubmission struct {
	AttemptID            string
	OrderID              string
	PlanID               string
	Chain                string
	SignedTx             []byte
	TxSignature          string
	LastValidBlockHeight uint64
	// IdempotencyKey is the semantic key of the SUBMIT step; adapters that
	// support provider-side idempotency forward it.
	IdempotencyKey string
}

// ExternalReference identifies a submission at the venue: the transaction
// signature and, where the provider issues one, its request id.
type ExternalReference struct {
	Venue             string
	TxSignature       string
	ProviderRequestID string
	WalletAddress     string
}

// SubmissionResult is a successful Submit.
type SubmissionResult struct {
	ExternalRef ExternalReference
	AcceptedAt  time.Time
	RawRef      string
}

// ExternalState is the venue's view of a submission.
type ExternalState string

// External states. NOT_FOUND means the venue has no record of the
// signature; on its own it never proves absence (PART 48 step 7 needs the
// chain height as well).
const (
	ExternalPending   ExternalState = "PENDING"
	ExternalObserved  ExternalState = "OBSERVED"
	ExternalConfirmed ExternalState = "CONFIRMED"
	ExternalFinalized ExternalState = "FINALIZED"
	ExternalFailed    ExternalState = "FAILED"
	ExternalExpired   ExternalState = "EXPIRED"
	ExternalNotFound  ExternalState = "NOT_FOUND"
)

// Valid reports whether s is a declared state.
func (s ExternalState) Valid() bool {
	switch s {
	case ExternalPending, ExternalObserved, ExternalConfirmed, ExternalFinalized, ExternalFailed, ExternalExpired, ExternalNotFound:
		return true
	}
	return false
}

// Terminal reports whether the venue will not change its answer.
func (s ExternalState) Terminal() bool {
	return s == ExternalFinalized || s == ExternalFailed || s == ExternalExpired
}

// Finality maps the state onto a finality level; states without one (PENDING,
// FAILED, EXPIRED, NOT_FOUND) return "" .
func (s ExternalState) Finality() FinalityLevel {
	switch s {
	case ExternalObserved:
		return FinalityObserved
	case ExternalConfirmed:
		return FinalityConfirmed
	case ExternalFinalized:
		return FinalityFinalized
	}
	return ""
}

// ExecutionStatus is the answer to Status.
type ExecutionStatus struct {
	State  ExternalState
	Fills  []ExternalExecutionEvent
	Slot   uint64
	Error  string
	RawRef string
}

// ExternalExecutionEvent is one fill as reported by the venue, the chain
// observer or a reconciliation sweep. Finality is the FinalityLevel name at
// which it was observed.
type ExternalExecutionEvent struct {
	Venue           string
	ExternalFillID  string
	TxSignature     string
	Slot            uint64
	InputAsset      assets.AssetID
	OutputAsset     assets.AssetID
	InputQty        money.Quantity
	OutputQty       money.Quantity
	NetworkFee      money.Quantity
	VenueFee        money.Quantity
	NetworkFeeAsset assets.AssetID
	ObservedAt      time.Time
	Finality        string
	RawRef          string
}

// ReconcileScope bounds a reconciliation sweep: a wallet and time window,
// optionally narrowed to one signature.
type ReconcileScope struct {
	WalletAddress string
	Since         time.Time
	Until         time.Time
	TxSignature   string
	Limit         int
}
