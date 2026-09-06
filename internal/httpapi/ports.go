package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/auth"
	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/identity"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/killswitch"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/quote"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/webhook"
	"github.com/nodal/controlplane/internal/withdrawal"
)

// The ports below are the only way a handler reaches the rest of the system.
// They are deliberately narrow: each method is one domain call. Concrete
// implementations are built by Wire (wiring.go) from the real domain services;
// tests substitute in-memory doubles so the router, middleware chain and error
// contract can be exercised without a database.
//
// A nil port is a wiring decision, not an error: the handler answers with the
// stable code UNSUPPORTED (HTTP 422, "not supported by this deployment") rather
// than pretending. Nothing is ever fabricated.

// IdentityPort is the OIDC login flow (internal/identity).
type IdentityPort interface {
	Begin(ctx context.Context, req identity.BeginRequest) (identity.BeginResult, error)
	Complete(ctx context.Context, req identity.CompleteRequest) (identity.Completed, error)
	Logout(ctx context.Context, sess auth.Session, req identity.CompleteRequest) error
}

// SessionsPort lists and revokes the caller's own sessions (internal/auth).
type SessionsPort interface {
	ListForSubject(ctx context.Context, subjectID string) ([]auth.Summary, error)
	Revoke(ctx context.Context, sessionID string) error
}

// AccountsPort reads and transitions accounts (internal/accounts).
type AccountsPort interface {
	Get(ctx context.Context, accountID accounts.AccountID) (accounts.Account, error)
	ListByOwner(ctx context.Context, ownerUserID accounts.UserID) ([]accounts.Account, error)
	Search(ctx context.Context, query, cursor string, limit int) (AccountPage, error)
	Transition(ctx context.Context, accountID accounts.AccountID, ch accounts.StatusChange) (accounts.Account, error)
}

// AccountPage is one cursor page of accounts.
type AccountPage struct {
	Items      []accounts.Account
	NextCursor string
}

// BuyingPowerPort computes buying power now (internal/capital/buyingpower).
// It is never cached: PART 25 requires a fresh computation per call.
type BuyingPowerPort interface {
	Compute(ctx context.Context, accountID accounts.AccountID, purpose buyingpower.Purpose) (buyingpower.BuyingPower, error)
}

// HoldingsPort assembles the portfolio view: lot basis from internal/positions
// and the USD mark and price reference the buying-power engine already
// computed. It performs no valuation of its own.
type HoldingsPort interface {
	Holdings(ctx context.Context, accountID accounts.AccountID) (HoldingsView, error)
}

// HoldingsView is the assembled holdings answer.
type HoldingsView struct {
	AsOf     time.Time
	Holdings []HoldingView
}

// HoldingView is one asset the account holds.
type HoldingView struct {
	AssetID        assets.AssetID
	Symbol         string
	Chain          string
	MintAddress    string
	Decimals       uint8
	Quantity       money.Quantity
	USDMark        money.USD
	PriceRef       string
	CostBasisUSD   money.USD
	UnrealizedUSD  money.USD
	RealizedUSD    *money.USD
	CustodyAddress string
}

// LedgerPort reads journal history (internal/ledger). There is no write port:
// the API never posts.
type LedgerPort interface {
	ListTransactions(ctx context.Context, filter ledger.TransactionFilter, cursor string, limit int) ([]ledger.Transaction, string, error)
}

// ActivityPort is the account activity timeline (PART 108 activity view).
type ActivityPort interface {
	Activity(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (ActivityPage, error)
}

// ActivityPage is one cursor page of activity items.
type ActivityPage struct {
	Items      []ActivityItem
	NextCursor string
}

// ActivityItem is one entry of the timeline.
type ActivityItem struct {
	ID            string
	Kind          string
	OccurredAt    time.Time
	Summary       string
	CorrelationID string
	References    map[string]string
}

// ExportPort renders the account export document (PART 202).
type ExportPort interface {
	Export(ctx context.Context, accountID accounts.AccountID, from, to time.Time) (ExportDocument, error)
}

// ExportDocument is the export payload in both supported renderings.
type ExportDocument struct {
	JSON map[string]any
	CSV  []byte
}

// AssetsPort is the asset registry (internal/assets).
type AssetsPort interface {
	List(ctx context.Context, limit int) ([]assets.Asset, error)
}

// InstrumentsPort is the instrument registry (internal/instruments).
type InstrumentsPort interface {
	List(ctx context.Context, limit int) ([]instruments.Instrument, error)
	Detail(ctx context.Context, instrumentID instruments.InstrumentID) (InstrumentDetail, error)
	Transition(ctx context.Context, instrumentID instruments.InstrumentID, ch instruments.StatusChange) (instruments.Instrument, error)
}

// InstrumentDetail is an instrument with its assets and venue listings.
type InstrumentDetail struct {
	Instrument instruments.Instrument
	Base       assets.Asset
	Quote      assets.Asset
	Listings   []instruments.ListingWithVenue
}

// QuotePort produces a non-binding quote preview. It reserves nothing and
// moves nothing. Nil when no execution adapter is wired, in which case the
// endpoint answers PROVIDER_UNAVAILABLE rather than inventing a price.
type QuotePort interface {
	Preview(ctx context.Context, req QuotePreview) (QuoteView, error)
}

// QuoteView is a provider disclosure plus the two facts the wire contract needs
// that internal/quote's Disclosure does not carry: the venue the quote came
// from, and the total estimated cost expressed in USD.
//
// TotalEstimatedCostUSD is supplied by the layer that produced the quote,
// because turning per-asset cost lines into one USD figure is a valuation and
// valuations belong to internal/valuation, never to an HTTP handler.
type QuoteView struct {
	Disclosure            quote.Disclosure
	Venue                 string
	TotalEstimatedCostUSD money.USD
}

// QuotePreview is the preview request after decoding.
type QuotePreview struct {
	AccountID    accounts.AccountID
	InstrumentID instruments.InstrumentID
	Action       intent.Action
	NotionalUSD  *money.USD
	Quantity     *money.Quantity
	Constraints  intent.Constraints
}

// IntentsPort is the manual trading entry point (internal/intent).
type IntentsPort interface {
	Submit(ctx context.Context, p security.Principal, req intent.SubmitRequest) (intent.TradeIntent, error)
	Get(ctx context.Context, intentID intent.IntentID) (intent.TradeIntent, error)
	ListForAccount(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (intent.Page, error)
	Detail(ctx context.Context, intentID intent.IntentID) (IntentDetail, error)
	// RequestCancel asks for cancellation. Only external confirmation ever
	// yields CANCELLED (PART 227); this records the request.
	RequestCancel(ctx context.Context, p security.Principal, intentID intent.IntentID, idempotencyKey string) (intent.TradeIntent, error)
}

// IntentDetail is an intent with the records linked to it.
type IntentDetail struct {
	Intent      intent.TradeIntent
	Order       *execution.Order
	Transitions []StateTransition
}

// StateTransition is one row of an aggregate's transition history.
type StateTransition struct {
	From       string
	To         string
	Reason     string
	OccurredAt time.Time
}

// OrdersPort reads execution records (internal/execution).
type OrdersPort interface {
	ListForAccount(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) (OrderPage, error)
	Detail(ctx context.Context, orderID execution.OrderID) (OrderDetail, error)
}

// OrderPage is one cursor page of orders.
type OrderPage struct {
	Items      []execution.Order
	NextCursor string
}

// OrderDetail is an order with its attempts and fills.
type OrderDetail struct {
	Order    execution.Order
	Attempts []execution.Attempt
	Fills    []execution.Fill
}

// FundingPort is the deposit lifecycle (internal/funding).
type FundingPort interface {
	Start(ctx context.Context, p security.Principal, req StartDeposit) (funding.StartResult, error)
	Get(ctx context.Context, depositID funding.DepositID) (funding.Deposit, error)
	List(ctx context.Context, accountID accounts.AccountID, cursor string, limit int) ([]funding.Deposit, string, error)
	Detail(ctx context.Context, depositID funding.DepositID) (DepositDetail, error)
}

// StartDeposit is the decoded funding request. The settlement asset, network,
// currency and destination wallet are resolved by the wiring layer from the
// account's registry state, never by the client.
type StartDeposit struct {
	AccountID       accounts.AccountID
	FiatAmountMinor *int64
	FiatCurrency    string
	FundingSourceID string
	CustomerIP      string
	IdempotencyKey  string
	CorrelationID   string
	RequestID       string
}

// DepositDetail is a deposit with its transition history.
type DepositDetail struct {
	Deposit     funding.Deposit
	Transitions []StateTransition
}

// WithdrawalsPort is the withdrawal boundary (internal/withdrawal). The
// WITHDRAWALS capability gate is DISABLED in every environment today, so the
// only honest answer from this port is a refusal; that refusal is produced by
// the domain, never by the handler.
type WithdrawalsPort interface {
	Request(ctx context.Context, p security.Principal, req WithdrawalRequest) (withdrawal.Withdrawal, error)
}

// WithdrawalRequest is the decoded withdrawal request.
type WithdrawalRequest struct {
	AccountID          accounts.AccountID
	AssetID            assets.AssetID
	Quantity           money.Quantity
	DestinationAddress string
	IdempotencyKey     string
	CorrelationID      string
	RequestID          string
}

// GatesPort is the production capability gate plane (internal/gates).
type GatesPort interface {
	List(ctx context.Context) ([]GateView, error)
	Act(ctx context.Context, capability gates.Capability, action GateAction, req gates.Proposal, note string) (GateView, error)
}

// GateAction is one step of the gate state machine.
type GateAction string

// Gate actions, matching the path enum of the spec.
const (
	GateActionPropose  GateAction = "propose"
	GateActionApprove  GateAction = "approve"
	GateActionActivate GateAction = "activate"
	GateActionSuspend  GateAction = "suspend"
	GateActionResume   GateAction = "resume"
	GateActionRevoke   GateAction = "revoke"
)

// GateView is a gate row plus the five-condition activation verdict, which is
// computed by internal/gates and is not the same thing as the row's state.
type GateView struct {
	Gate    gates.Gate
	Verdict gates.Verdict
}

// KillSwitchesPort is the emergency control plane (internal/killswitch).
// Kill switches never stop reconciliation, settlement or ledger posting.
type KillSwitchesPort interface {
	List(ctx context.Context) ([]killswitch.Switch, error)
	Activate(ctx context.Context, kind killswitch.Kind, scope, reason string) (killswitch.Switch, error)
	Release(ctx context.Context, kind killswitch.Kind, scope, reason string, approvalID *string) (killswitch.Switch, error)
}

// AdminActionsPort is dual-controlled administrative action (internal/admin).
type AdminActionsPort interface {
	List(ctx context.Context, status, cursor string, limit int) (AdminActionPage, error)
	Propose(ctx context.Context, p admin.Proposal) (admin.Action, error)
	Approve(ctx context.Context, actionID, note string) (admin.Action, error)
	Reject(ctx context.Context, actionID, reason string) (admin.Action, error)
	Execute(ctx context.Context, actionID string) (admin.Action, error)
}

// AdminActionPage is one cursor page of admin actions.
type AdminActionPage struct {
	Items      []admin.Action
	NextCursor string
}

// ProvidersPort reports provider health and verification labels.
type ProvidersPort interface {
	List(ctx context.Context) ([]ProviderView, error)
}

// ProviderView is one provider's operational status.
type ProviderView struct {
	Name          string
	Role          string
	Health        string
	Verification  string
	Mode          string
	ErrorRateBPS  int64
	P95Millis     int64
	LastSuccessAt time.Time
	DisableReason string
}

// ReconciliationPort reads and resolves reconciliation records
// (internal/reconciliation). Resolution is dual-controlled for material
// mismatches and always posts a compensating journal transaction rather than
// editing a balance.
type ReconciliationPort interface {
	List(ctx context.Context, status string, accountID *accounts.AccountID, cursor string, limit int) (ReconciliationPage, error)
	Resolve(ctx context.Context, recordID string, res ReconciliationResolution) (ReconciliationRecord, error)
}

// ReconciliationPage is one cursor page of reconciliation records.
type ReconciliationPage struct {
	Items      []ReconciliationRecord
	NextCursor string
}

// ReconciliationRecord is the API projection of a reconciliation record.
type ReconciliationRecord struct {
	ID                    string
	Kind                  string
	Mode                  string
	ScopeType             string
	ScopeID               string
	AccountID             string
	AssetID               string
	Expected              map[string]any
	Observed              map[string]any
	Difference            map[string]any
	Status                string
	Material              bool
	BlocksNewRisk         bool
	OpenedAt              time.Time
	ResolvedAt            *time.Time
	ResolutionReason      string
	ResolutionEvidenceRef string
	CompensatingJournalTx string
}

// ReconciliationResolution is an operator's resolution of a mismatch.
type ReconciliationResolution struct {
	Reason      string
	EvidenceRef string
	ApprovalID  string
	// Compensation, when present, is a reason-coded compensating posting.
	// It is handed to the domain unchanged; the API never builds entries of
	// its own and never edits a balance.
	Compensation *Compensation
}

// Compensation is a reason-coded compensating posting request.
type Compensation struct {
	ReasonCode string
	Entries    []CompensationEntry
}

// CompensationEntry is one leg of a compensating posting.
type CompensationEntry struct {
	AccountCode string
	AssetID     assets.AssetID
	Side        string
	Quantity    money.Quantity
}

// WebhookPort ingests one provider's webhooks (internal/webhook). The handler
// hands over the raw body: signatures are verified over bytes, never over a
// re-encoded document.
type WebhookPort interface {
	Handle(ctx context.Context, raw []byte, headers http.Header, meta webhook.RequestMeta) webhook.Result
}

// HealthPort answers readiness. It reports the database and configuration
// only: provider liveness is never readiness (a degraded venue must not take
// the API out of the load balancer).
type HealthPort interface {
	Ready(ctx context.Context) error
}

// IdempotencyPort is the persisted idempotency contract (internal/idempotency)
// wrapped so a handler can run a command exactly once per key.
type IdempotencyPort interface {
	// Run executes fn at most once for (actorID, endpoint, key). On a
	// replay it returns the recorded status and body without calling fn.
	// A key reused with a different request body fails with
	// INVALID_IDEMPOTENCY_REUSE.
	Run(ctx context.Context, req IdempotentCommand, fn func(ctx context.Context) (CommandResult, error)) (CommandResult, error)
}

// IdempotentCommand identifies one attempt at a money-affecting command.
type IdempotentCommand struct {
	ActorID     string
	Endpoint    string
	Key         string
	RequestHash string
	TTL         time.Duration
}

// CommandResult is what a command produced, in the form the idempotency store
// records so a replay can reproduce it byte for byte.
type CommandResult struct {
	Status       int
	ResourceType string
	ResourceID   string
	Body         []byte
	// Replayed is set by the port when the result came from the store.
	Replayed bool
}
