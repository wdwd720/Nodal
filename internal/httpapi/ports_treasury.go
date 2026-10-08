package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/id"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/treasury"
)

// TreasuryPort serves the house book (internal/treasury; ADR-0020): a
// platform-operated principal book that trades only its own SIMULATED house
// capital. The treasury decides everything -- the signing policy, the
// capabilities, the versioned risk limits, Risk Kernel V2 and the order book's
// own rules -- and records every house action, refused ones included. Every
// command runs one treasury operation in its own transaction; a refusal the
// treasury recorded is committed and then returned as the error, so its
// record stands and its code reaches the wire unchanged.
//
// Reads present the treasury's own views. Where the treasury package lists
// nothing (books, paper fills, risk decisions, the trades a house book made
// against a customer) the adapter projects its tables read-only.
type TreasuryPort interface {
	Books(ctx context.Context, cursor string, limit int) (TreasuryBookPage, error)
	// Book returns one house book with the signing policy and risk policy in
	// force. NOT_FOUND when no treasury book has the id.
	Book(ctx context.Context, house econ.HouseAccountID) (TreasuryBookView, error)
	Register(ctx context.Context, r TreasuryRegister) (TreasuryBookView, error)
	SetSigningPolicy(ctx context.Context, r TreasurySigningChange) (treasury.SigningPolicyVersion, error)
	SetRiskPolicy(ctx context.Context, r TreasuryRiskChange) (treasury.PolicyVersion, error)
	Fund(ctx context.Context, r treasury.FundRequest) (TreasuryFundingView, error)
	PlaceOrder(ctx context.Context, r TreasuryOrderCommand) (treasury.OrderOutcome, error)
	CancelOrder(ctx context.Context, r treasury.CancelRequest) (treasury.OrderOutcome, error)
	Quote(ctx context.Context, r TreasuryQuoteCommand) (treasury.QuoteOutcome, error)
	Hedge(ctx context.Context, r treasury.PaperRequest) (TreasuryPaperView, error)
	Convert(ctx context.Context, r treasury.ConvertRequest) (TreasuryPaperView, error)
	Rebalance(ctx context.Context, r treasury.RebalanceRequest) (TreasuryPaperView, error)
	ExecutePaper(ctx context.Context, r treasury.PaperRequest) (TreasuryPaperView, error)
	SetCrossingRule(ctx context.Context, r treasury.CrossingRuleRequest) (treasury.CrossingDisclosure, error)
	Sync(ctx context.Context, house econ.HouseAccountID) (int, error)
	MarkToMarket(ctx context.Context, house econ.HouseAccountID, key string, actor econ.Actor) (TreasurySnapshotView, error)

	// Report syncs the register and returns the PnL report at fresh marks with
	// the limits in use (it takes the book's lock).
	Report(ctx context.Context, house econ.HouseAccountID) (TreasuryReportView, error)
	Balances(ctx context.Context, house econ.HouseAccountID) (TreasuryBalancesView, error)
	// Reconcile syncs the register and proves it against the ledger (it takes
	// the book's lock).
	Reconcile(ctx context.Context, house econ.HouseAccountID) ([]treasury.Discrepancy, error)
	Events(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryEventPage, error)
	Actions(ctx context.Context, house econ.HouseAccountID, limit int) ([]treasury.Action, error)
	PaperFills(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryPaperFillPage, error)
	RiskDecisions(ctx context.Context, house econ.HouseAccountID, cursor string, limit int) (TreasuryDecisionPage, error)
	CrossingDisclosures(ctx context.Context, instrument instruments.InstrumentID) ([]treasury.CrossingDisclosure, error)
	// HouseTrades pages the internal-book trades in which a house book was the
	// account's counterparty, newest first.
	HouseTrades(ctx context.Context, account accounts.AccountID, cursor string, limit int) (TreasuryHouseTradePage, error)
}

// TreasuryAsset names an asset the way a treasury record is read: its id,
// symbol and the decimals its base units are in.
type TreasuryAsset struct {
	ID       assets.AssetID
	Symbol   string
	Decimals uint8
}

// TreasuryAssets describes every asset a view names, keyed by id.
type TreasuryAssets map[assets.AssetID]TreasuryAsset

// TreasuryBookView is a house book with its valuation asset. Signing and Risk
// are the policies in force; they are present on a read of one book (Risk is
// nil while the book has no risk policy).
type TreasuryBookView struct {
	Book           treasury.Book
	ValuationAsset TreasuryAsset
	Signing        *treasury.SigningPolicyVersion
	Risk           *treasury.PolicyVersion
}

// TreasuryBookPage is one page of house books, newest first.
type TreasuryBookPage struct {
	Items      []TreasuryBookView
	NextCursor string
}

// TreasuryRegister registers a house book. A nil ValuationAsset is the
// deployment's sandbox dollar.
type TreasuryRegister struct {
	Name           string
	Strategy       econ.HouseStrategy
	ValuationAsset *assets.AssetID
	Signing        treasury.SigningPolicy
	IdempotencyKey string
	Reason         string
	Actor          econ.Actor
}

// TreasurySigningChange records the next signing policy of a house book.
type TreasurySigningChange struct {
	House          econ.HouseAccountID
	Policy         treasury.SigningPolicy
	IdempotencyKey string
	Reason         string
	Actor          econ.Actor
}

// TreasuryRiskChange records the next risk policy of a house book. The policy
// is stated in the book's valuation asset, which the adapter reads from the
// book; Policy.ValuationAsset is ignored.
type TreasuryRiskChange struct {
	House          econ.HouseAccountID
	Policy         treasury.Policy
	IdempotencyKey string
	Reason         string
	Actor          econ.Actor
}

// TreasuryFundingView is a house funding with the asset it funded.
type TreasuryFundingView struct {
	Funding treasury.Funding
	Asset   TreasuryAsset
}

// TreasuryOrderCommand is a house order. Price is the exact decimal price in
// the book's valuation asset; the adapter states it in that asset.
type TreasuryOrderCommand struct {
	House          econ.HouseAccountID
	Instrument     instruments.InstrumentID
	Activity       econ.HouseStrategy
	Side           orders.Side
	Type           orders.OrderType
	TimeInForce    orders.TimeInForce
	Price          string
	Quantity       money.Quantity
	PostOnly       bool
	ReduceOnly     bool
	ClientOrderID  string
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// TreasuryQuoteCommand is a two-sided quote, prices as exact decimals in the
// book's valuation asset.
type TreasuryQuoteCommand struct {
	House          econ.HouseAccountID
	Instrument     instruments.InstrumentID
	Activity       econ.HouseStrategy
	BidPrice       string
	BidQuantity    money.Quantity
	AskPrice       string
	AskQuantity    money.Quantity
	IdempotencyKey string
	CorrelationID  string
	Actor          econ.Actor
}

// TreasuryPaperView is the outcome of a paper-venue request with the assets
// its fill names. NoOp is a rebalance whose holding was already at its target:
// nothing was filled or recorded.
type TreasuryPaperView struct {
	Outcome treasury.PaperOutcome
	NoOp    bool
	Assets  TreasuryAssets
}

// TreasurySnapshotView is a mark-to-market snapshot with its report's assets.
type TreasurySnapshotView struct {
	Snapshot treasury.Snapshot
	Report   TreasuryReportView
}

// TreasuryLimitKind names what a limit bounds.
type TreasuryLimitKind string

// Limit kinds.
const (
	TreasuryLimitKindDailyLoss          TreasuryLimitKind = "DAILY_LOSS"
	TreasuryLimitKindInventory          TreasuryLimitKind = "INVENTORY"
	TreasuryLimitKindInstrumentExposure TreasuryLimitKind = "INSTRUMENT_EXPOSURE"
)

// TreasuryLimitUse is one limit of the risk policy in force with what is
// already used of it: the treasury's own evaluation (treasury.EvaluateLimits)
// of a risk-increasing request that adds nothing, over the book's state read
// in the report's transaction. Codes are what new risk would meet now.
type TreasuryLimitUse struct {
	Kind       TreasuryLimitKind
	Asset      *assets.AssetID
	Instrument *instruments.InstrumentID
	Cap        *money.Quantity
	Used       *money.Quantity
	Codes      []string
}

// TreasuryReportView is the PnL report with the assets it names and, when the
// book has a risk policy, its version and the limits in use.
type TreasuryReportView struct {
	Report            treasury.Report
	Valuation         TreasuryAsset
	Assets            TreasuryAssets
	RiskPolicyVersion int
	Limits            []TreasuryLimitUse
}

// TreasuryBalancesView is the book's balances, asset by asset.
type TreasuryBalancesView struct {
	House  econ.HouseAccountID
	Items  []treasury.AssetBalance
	Assets TreasuryAssets
}

// TreasuryEventPage is one page of the position register, newest first.
type TreasuryEventPage struct {
	Items      []treasury.PositionEvent
	Assets     TreasuryAssets
	NextCursor string
}

// TreasuryPaperFillPage is one page of paper fills, newest first.
type TreasuryPaperFillPage struct {
	Items      []treasury.PaperFill
	Assets     TreasuryAssets
	NextCursor string
}

// TreasuryDecisionRecord is one persisted treasury risk decision as its row
// states it, with the limit evaluation that fed it.
type TreasuryDecisionRecord struct {
	ID                    treasury.DecisionID
	HouseAccountID        econ.HouseAccountID
	IntentKind            string
	Instrument            *instruments.InstrumentID
	Asset                 assets.AssetID
	RiskIncreasing        bool
	Stage                 string
	Verdict               string
	ReasonCodes           []string
	TreasuryCodes         []string
	Limits                treasury.LimitEvaluation
	InputHash             string
	DecisionHash          string
	RiskPolicyVersion     string
	EvaluatorVersion      string
	TreasuryPolicyVersion int
	TreasuryPolicyHash    string
	EvaluatedAt           time.Time
}

// TreasuryDecisionPage is one page of risk decisions, newest first.
type TreasuryDecisionPage struct {
	Items      []TreasuryDecisionRecord
	NextCursor string
}

// TreasuryHouseTrade is one internal-book trade between a house book and the
// account, with the crossing rule version that allowed it and that version's
// customer disclosure.
type TreasuryHouseTrade struct {
	TradeID             id.ID[id.Any]
	BookID              orders.BookID
	Instrument          instruments.InstrumentID
	TradeSeq            int64
	House               econ.HouseAccountID
	HouseSide           orders.Side
	AccountSide         orders.Side
	Price               money.Price
	Base                TreasuryAsset
	Quote               TreasuryAsset
	BaseQuantity        money.Quantity
	QuoteNotional       money.Quantity
	AccountFee          money.Quantity
	CrossingRuleVersion int
	Disclosure          string
	JournalTransaction  ledger.TransactionID
	TradedAt            time.Time
}

// TreasuryHouseTradePage is one page of house trades, newest first.
type TreasuryHouseTradePage struct {
	Items      []TreasuryHouseTrade
	NextCursor string
}
