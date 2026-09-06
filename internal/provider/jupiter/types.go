package jupiter

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/provider"
)

// Operation names one client method. It is the key for retry classes,
// evidence records, health samples and fault injection.
type Operation string

// Operations.
const (
	OpOrder   Operation = "order"
	OpBuild   Operation = "build"
	OpExecute Operation = "execute"
	OpStatus  Operation = "status"
)

// RetryClassOf codifies the provider retry matrix (PART 106) for each
// operation: reads and pure construction are SAFE_RETRY, Execute is
// UNKNOWN_EFFECT_WRITE. Unknown operations are treated as
// UNKNOWN_EFFECT_WRITE so a new method can never be retried by accident.
func RetryClassOf(op Operation) provider.RetryClass {
	switch op {
	case OpOrder, OpBuild, OpStatus:
		return provider.SafeRetry
	default:
		return provider.UnknownEffectWrite
	}
}

// Service is the provider-neutral-shaped method set shared by Client and
// Fake. The integrator adapts it to execution.ExecutionAdapter.
type Service interface {
	// Name returns ProviderName.
	Name() string
	// VerificationLabel reports the honest integration status.
	VerificationLabel() provider.VerificationLabel
	// Order requests a quote and, when a taker is given, an unsigned
	// transaction (GET /order). SAFE_RETRY.
	Order(ctx context.Context, req OrderRequest) (Order, error)
	// ValidateQuote checks an Order against a policy at time now. Pure.
	ValidateQuote(o Order, now time.Time, p ValidationPolicy) error
	// Build requests raw instructions (GET /build). SAFE_RETRY.
	Build(ctx context.Context, req BuildRequest) (BuildResult, error)
	// Execute submits a signed transaction (POST /execute) exactly once.
	// UNKNOWN_EFFECT_WRITE: a timeout or ambiguous answer is
	// SUBMISSION_STATE_UNKNOWN.
	Execute(ctx context.Context, req ExecuteRequest) (ExecuteResult, error)
	// Status is UNSUPPORTED for Swap API V2 (no status endpoint is
	// documented); status comes from the chain observers.
	Status(ctx context.Context, signature string) (StatusResult, error)
}

// SwapModeExactIn is the only documented swapMode.
const SwapModeExactIn = "ExactIn"

// BroadcastFeeType values documented for /order.
const (
	BroadcastFeeMaxCap   = "maxCap"
	BroadcastFeeExactFee = "exactFee"
)

// OrderRequest carries the documented GET /order query parameters.
type OrderRequest struct {
	InputMint  string
	OutputMint string
	// Amount is the input amount in atomic units (sent as a string).
	Amount money.Quantity
	// SlippageBPS is 0..10000. Nil lets Jupiter choose (RTSE auto slippage).
	SlippageBPS *money.BPS
	// TakerPubkey is the signer wallet. Empty means quote-only: no
	// transaction is returned.
	TakerPubkey string
	// Receiver, when set, must differ from the taker.
	Receiver string
	// SwapMode must be empty or SwapModeExactIn.
	SwapMode string
	// ReferralAccount and ReferralFeeBPS (50-255) must be given together.
	ReferralAccount string
	ReferralFeeBPS  *money.BPS
	// Payer restricts routing to Metis (documented side effect).
	Payer string
	// PriorityFeeLamports / JitoTipLamports override the fee estimate.
	PriorityFeeLamports *money.Quantity
	JitoTipLamports     *money.Quantity
	// BroadcastFeeType is maxCap or exactFee.
	BroadcastFeeType string
	// ExcludeRouters is a subset of metis, jupiterz, dflow, okx.
	ExcludeRouters []string
	// ExcludeDexes applies to Metis routing only.
	ExcludeDexes []string
}

// RouteStep is one hop of the route plan, decoded from routePlan[].swapInfo
// with amounts parsed exactly.
type RouteStep struct {
	AMMKey     string
	Label      string
	InputMint  string
	OutputMint string
	InAmount   money.Quantity
	OutAmount  money.Quantity
	// BPS is the documented share of the input routed through this step
	// (routePlan[].bps). Zero when absent.
	BPS int64
}

// PlatformFee is the documented platformFee object.
type PlatformFee struct {
	Amount  money.Quantity
	FeeBPS  money.BPS
	FeeMint string
}

// RFQInfo carries the JupiterZ (RFQ) route fields, present only on RFQ
// routes.
type RFQInfo struct {
	QuoteID string
	Maker   string
	// ExpireAt is the documented quote expiry. ASSUMED format: the OpenAPI
	// types it as a string without a format; digits are read as unix
	// seconds (milliseconds when > 1e12), anything else as RFC 3339.
	ExpireAt time.Time
}

// RateLimitInfo is what the gateway reported on the last attempt.
type RateLimitInfo struct {
	// Remaining may be negative (documented).
	Remaining int64
	Current   int64
	// Reset is the documented unix timestamp; zero when absent.
	Reset time.Time
	// Present is false when the headers were absent.
	Present bool
}

// Order is the provider-neutral projection of a GET /order response.
type Order struct {
	// QuoteID is Jupiter's requestId, which must be passed to Execute.
	QuoteID string
	// Mode is "ultra" or "manual"; Router is the winning router
	// (metis, jupiterz, dflow, okx). Informational.
	Mode   string
	Router string

	// InputMint, OutputMint, Taker and Receiver are echoed from the request
	// (the V2 response does not repeat them); the route plan is checked for
	// consistency with the mints.
	InputMint  string
	OutputMint string
	Taker      string
	Receiver   string

	InAmount             money.Quantity
	OutAmount            money.Quantity
	OtherAmountThreshold money.Quantity
	SlippageBPS          money.BPS
	FeeBPS               money.BPS
	PlatformFee          *PlatformFee

	// PriceImpactBPS is derived exactly from the decimal text of the
	// documented priceImpact field (percentage points, RoundCeil so impact
	// is never understated). PriceImpactUnavailable is true and the value is
	// 0 when the field is absent or not a plain decimal; PriceImpactSource
	// names the wire field used.
	PriceImpactBPS         money.BPS
	PriceImpactUnavailable bool
	PriceImpactSource      string

	Route []RouteStep
	// RoutePlanSummary is the canonical JSON of routePlan; RouteHash is its
	// SHA-256.
	RoutePlanSummary json.RawMessage
	RouteHash        []byte

	// UnsignedTransaction is the base64-decoded v0 transaction; empty when
	// the request had no taker (HasTransaction false).
	UnsignedTransaction []byte
	HasTransaction      bool
	// TransactionHash is SHA-256 of UnsignedTransaction.
	TransactionHash []byte
	// FeePayer, RecentBlockhash, RequiredSigners, ComputeUnitLimit and
	// ComputeUnitPriceMicroLamports are decoded from the transaction bytes
	// (V2 /order does not report a compute unit limit; the source is the
	// transaction's ComputeBudget instructions, ComputeUnitLimitSource
	// "transaction", or "" when absent).
	FeePayer                      string
	RecentBlockhash               string
	RequiredSigners               []string
	ComputeUnitLimit              uint32
	ComputeUnitLimitSource        string
	ComputeUnitPriceMicroLamports uint64
	// ProgramIDs lists the top-level program ids in the transaction.
	ProgramIDs []string

	// LastValidBlockHeight is the hard expiry for aggregator routes.
	// LastValidBlockHeightRaw is the exact wire string, to be passed to
	// Execute unchanged (documented type conflict: string vs number).
	LastValidBlockHeight    uint64
	LastValidBlockHeightRaw string

	SignatureFeeLamports      money.Quantity
	PrioritizationFeeLamports money.Quantity
	RentFeeLamports           money.Quantity
	Gasless                   bool

	RFQ *RFQInfo

	// ReceivedAt is when the response was received (injected clock).
	// ExpiresAt is RFQ.ExpireAt when present, else ReceivedAt +
	// Config.AssumedOrderTTL with ExpiresAtAssumed true.
	ReceivedAt       time.Time
	ExpiresAt        time.Time
	ExpiresAtAssumed bool

	// Evidence.
	RawRef           string
	RawHash          []byte
	GatewayRequestID string
	RateLimit        RateLimitInfo
}

// ValidationPolicy is what ValidateQuote enforces.
type ValidationPolicy struct {
	// MaxAge, when positive, rejects orders older than now-ReceivedAt.
	MaxAge time.Duration
	// Expected identities; empty values are not checked.
	ExpectedInputMint  string
	ExpectedOutputMint string
	ExpectedTaker      string
	// ExpectedInAmount, when non-nil, must equal InAmount exactly.
	ExpectedInAmount *money.Quantity
	// ExpectedMinOut, when non-nil, requires OtherAmountThreshold >= it.
	ExpectedMinOut *money.Quantity
	// MaxSlippageBPS / MaxPriceImpactBPS, when non-nil, cap the order's
	// values. RequirePriceImpact rejects orders whose impact is unavailable.
	MaxSlippageBPS     *money.BPS
	MaxPriceImpactBPS  *money.BPS
	RequirePriceImpact bool
	// CurrentBlockHeight, when non-zero, must be below
	// LastValidBlockHeight - MinBlockHeightMargin.
	CurrentBlockHeight   uint64
	MinBlockHeightMargin uint64
	// RequireTransaction rejects quote-only orders.
	RequireTransaction bool
}

// ExecuteRequest carries the documented POST /execute body.
type ExecuteRequest struct {
	// SignedTransaction is the fully (or, for RFQ routes, partially) signed
	// transaction bytes; sent base64-encoded.
	SignedTransaction []byte
	// RequestID is Order.QuoteID.
	RequestID string
	// LastValidBlockHeight is Order.LastValidBlockHeightRaw, passed through
	// unchanged. Optional.
	LastValidBlockHeight string
}

// ExecuteStatus is the documented status of an /execute response.
type ExecuteStatus string

// Execute statuses.
const (
	ExecuteSuccess ExecuteStatus = "Success"
	ExecuteFailed  ExecuteStatus = "Failed"
)

// SwapEvent is one documented swapEvents[] entry.
type SwapEvent struct {
	InputMint    string
	InputAmount  money.Quantity
	OutputMint   string
	OutputAmount money.Quantity
}

// ExecuteResult is the provider-neutral projection of a 200 /execute
// response. A Status of ExecuteFailed is returned with a nil error: the
// signature is known and the caller must establish on-chain state before
// treating the attempt as failed (EXECUTION.md section 4).
type ExecuteResult struct {
	Signature string
	Status    ExecuteStatus
	Slot      uint64
	// ProviderCode / ProviderError are the documented code and error
	// fields (0 and "" on success).
	ProviderCode  int64
	ProviderError string

	TotalInputAmount   *money.Quantity
	TotalOutputAmount  *money.Quantity
	InputAmountResult  *money.Quantity
	OutputAmountResult *money.Quantity
	SwapEvents         []SwapEvent

	SubmittedAt      time.Time
	ReceivedAt       time.Time
	RawRef           string
	RawHash          []byte
	GatewayRequestID string
	RateLimit        RateLimitInfo
}

// StatusResult is the (unsupported) status projection.
type StatusResult struct {
	Signature string
}

// BuildRequest carries the documented GET /build query parameters.
type BuildRequest struct {
	InputMint  string
	OutputMint string
	Amount     money.Quantity
	// Taker is required by /build.
	Taker string
	// SlippageBPS (0..10000) or SlippageRTSE ("rtse"); at most one. When
	// neither is set the documented default (50) applies server-side.
	SlippageBPS  *money.BPS
	SlippageRTSE bool
	// Mode is the documented "fast" mode when set.
	Mode string
	// Dexes and ExcludeDexes are mutually exclusive.
	Dexes        []string
	ExcludeDexes []string
	// PlatformFeeBPS requires FeeAccount.
	PlatformFeeBPS *money.BPS
	FeeAccount     string
	// MaxAccounts is 1..64 (default 64 server-side when zero).
	MaxAccounts int
	Payer       string
	// WrapAndUnwrapSOL defaults to true server-side when nil.
	WrapAndUnwrapSOL *bool
	// DestinationTokenAccount and NativeDestinationAccount are alternatives.
	DestinationTokenAccount  string
	NativeDestinationAccount string
	// BlockhashSlotsToExpiry is 1..300 (default 150 when zero).
	BlockhashSlotsToExpiry int
	// TipAmount is in lamports (sent as a string).
	TipAmount *money.Quantity
	// ComputeUnitPricePercentile is medium|high|veryHigh or "0".."10000".
	ComputeUnitPricePercentile string
	ForJitoBundle              bool
}

// AccountMeta is a provider-neutral instruction account.
type AccountMeta struct {
	Pubkey     string
	IsSigner   bool
	IsWritable bool
}

// Instruction is a provider-neutral instruction as returned by /build.
type Instruction struct {
	ProgramID string
	Accounts  []AccountMeta
	Data      []byte
}

// BuildResult is the provider-neutral projection of a GET /build response.
// It cannot be executed through Execute (no requestId): the caller
// assembles, simulates (to size the compute unit limit, which /build does
// not return), inspects, signs and submits through the chain RPC.
type BuildResult struct {
	InputMint  string
	OutputMint string
	Taker      string

	InAmount             money.Quantity
	OutAmount            money.Quantity
	OtherAmountThreshold money.Quantity
	SlippageBPS          money.BPS
	// PriceImpactBPS is derived exactly from priceImpactPct (a decimal
	// fraction string, "0.001" = 0.1%, RoundCeil).
	PriceImpactBPS         money.BPS
	PriceImpactUnavailable bool

	Route            []RouteStep
	RoutePlanSummary json.RawMessage
	RouteHash        []byte

	ComputeBudgetInstructions []Instruction
	SetupInstructions         []Instruction
	SwapInstruction           Instruction
	CleanupInstruction        *Instruction
	OtherInstructions         []Instruction
	TipInstruction            *Instruction
	// AddressLookupTables maps ALT address -> addresses it contains.
	AddressLookupTables map[string][]string

	// ComputeUnitLimitUnavailable is always true: documented as absent.
	ComputeUnitLimitUnavailable bool

	Blockhash            string
	LastValidBlockHeight uint64
	BlockhashFetchedAt   time.Time

	ReceivedAt       time.Time
	ExpiresAt        time.Time
	ExpiresAtAssumed bool

	RawRef           string
	RawHash          []byte
	GatewayRequestID string
	RateLimit        RateLimitInfo
}
