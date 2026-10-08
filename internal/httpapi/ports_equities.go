package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/instruments"
)

// Paper and sandbox equities (internal/equities), ADR-0020. Every figure
// behind this port is value domain SIMULATED; the venue is the deterministic
// paper venue and no adapter in this build reaches a real broker. The service
// decides every rule -- the EQUITIES gate and kill switches, the session, lots
// and the limit grid, pending corporate actions, Risk Kernel V2, the capital
// reservation, T+1 settlement -- and the port carries its answers.

// EquityListingView is a listing with the freshest mark of its shares no
// older than the listing's oracle bound (nil when there is none).
type EquityListingView struct {
	Listing equities.Listing
	Mark    *equities.Mark
}

// EquitySessionView is the calendar at one instant: the session in force, the
// New York trade date, and the T+1 settlement date of a trade made on it
// (zero when the trade date is not a trading day).
type EquitySessionView struct {
	Calendar       string
	Timezone       string
	At             time.Time
	Session        equities.Session
	TradeDate      equities.Date
	TradingDay     bool
	HolidayName    string
	NextTradingDay equities.Date
	SettlementDate equities.Date
}

// EquityListingsView is every listing read with the calendar's state.
type EquityListingsView struct {
	Items   []EquityListingView
	Session EquitySessionView
}

// EquityCalendarView is the calendar's state and its recorded closures.
type EquityCalendarView struct {
	State               EquitySessionView
	SettlementCycleDays int
	Holidays            []equities.Holiday
}

// EquityAssets names a listing's two assets: the share asset (quantities)
// and the settlement asset (prices and every amount of value).
type EquityAssets struct {
	ShareSymbol        string
	ShareDecimals      uint8
	SettlementAssetID  assets.AssetID
	SettlementSymbol   string
	SettlementDecimals uint8
}

// EquitySettlementView is a settlement with the symbol and decimals of what
// it delivers.
type EquitySettlementView struct {
	Settlement    equities.Settlement
	AssetSymbol   string
	AssetDecimals uint8
}

// EquityFillView is a fill with its listing's assets and its settlement.
type EquityFillView struct {
	Fill       equities.Fill
	Assets     EquityAssets
	Settlement *EquitySettlementView
}

// EquityOrderView is an order with its fill, when it filled.
type EquityOrderView struct {
	Order equities.Order
	Fill  *EquityFillView
}

// EquityPlacement is what placing an ACCEPTED order at the venue did. Placed
// means the venue's answer is recorded on the order; otherwise Err says why
// nothing was recorded, and the order stays ACCEPTED with its submission
// PENDING for an operator's venue run.
type EquityPlacement struct {
	Placed bool
	Report *equities.OrderReport
	Err    error
	// Earlier is true when the order already had the venue's answer before
	// this request (a replay of the service's own idempotency key).
	Earlier bool
}

// EquityOrderSubmitted is a submission's answer.
type EquityOrderSubmitted struct {
	Order     EquityOrderView
	Placement EquityPlacement
}

// EquityOrderSubmit is an equity order as the client stated it.
type EquityOrderSubmit struct {
	Request equities.OrderRequest
}

// EquityListingRequest lists an equity. DefaultSettlement asks for the
// sandbox dollar as the settlement asset.
type EquityListingRequest struct {
	Spec              equities.ListingSpec
	DefaultSettlement bool
}

// EquityApplicationView is one account's share of a corporate action, with
// the action's type and instrument and its listing's assets.
type EquityApplicationView struct {
	Application  equities.Application
	ActionType   equities.CorporateActionType
	InstrumentID instruments.InstrumentID
	Assets       EquityAssets
}

// EquityCorporateActionResultView is what applying a corporate action did.
type EquityCorporateActionResultView struct {
	Result       equities.CorporateActionResult
	Applications []EquityApplicationView
}

// EquitySettlementRun is what one run of the settlement worker's step did.
type EquitySettlementRun struct {
	At        time.Time
	TradeDate equities.Date
	Batches   int
	Settled   []EquitySettlementView
}

// Venue run actions.
const (
	EquityVenuePlace = "PLACE"
	EquityVenuePoll  = "POLL"
)

// EquityVenueRunItem is what one run of the venue worker's step did to one
// order.
type EquityVenueRunItem struct {
	OrderID equities.OrderID
	Action  string
	Before  equities.OrderStatus
	// After is empty when no answer was applied (Err says why).
	After  equities.OrderStatus
	Report *equities.OrderReport
	Err    error
}

// EquityVenueRun is one run of the venue worker's step.
type EquityVenueRun struct {
	At    time.Time
	Items []EquityVenueRunItem
}

// Settlement filters of an account's settlement listing.
const (
	EquitySettlementsAll     = ""
	EquitySettlementsPending = "PENDING"
	EquitySettlementsSettled = "SETTLED"
)

// AdvancedEquitiesPort serves internal/equities. A refusal the service
// records (a REJECTED order) is committed before it is returned as the
// command's error, carrying its stable code and the order it recorded.
type AdvancedEquitiesPort interface {
	Listings(ctx context.Context, limit int) (EquityListingsView, error)
	Listing(ctx context.Context, instrumentID instruments.InstrumentID) (EquityListingView, error)
	ListEquity(ctx context.Context, r EquityListingRequest) (EquityListingView, error)
	Calendar(ctx context.Context) (EquityCalendarView, error)
	AddHoliday(ctx context.Context, h equities.Holiday) (EquityCalendarView, error)

	// Submit records, routes and accepts the order, each in its own
	// transaction, then places an ACCEPTED order at the venue and applies
	// the venue's answer in a transaction of its own.
	Submit(ctx context.Context, r EquityOrderSubmit) (EquityOrderSubmitted, error)
	// Cancel asks the venue to cancel the order and applies its answer.
	Cancel(ctx context.Context, id equities.OrderID) (EquityOrderView, error)
	// Order returns one order whatever its owner; the handler decides who
	// may see it.
	Order(ctx context.Context, id equities.OrderID) (EquityOrderView, error)
	Orders(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityOrderView, string, error)
	Fills(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityFillView, string, error)
	Settlements(ctx context.Context, account accounts.AccountID, state string, cursor string, limit int) ([]EquitySettlementView, string, error)
	Applications(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]EquityApplicationView, string, error)

	RunSettlements(ctx context.Context, limit int) (EquitySettlementRun, error)
	RunVenue(ctx context.Context, limit int) (EquityVenueRun, error)

	CorporateActions(ctx context.Context, instrumentID *instruments.InstrumentID, cursor string, limit int) ([]equities.CorporateAction, string, error)
	CorporateAction(ctx context.Context, id equities.CorporateActionID) (equities.CorporateAction, error)
	AnnounceCorporateAction(ctx context.Context, spec equities.CorporateActionSpec) (equities.CorporateAction, error)
	CancelCorporateAction(ctx context.Context, id equities.CorporateActionID, reason string) (equities.CorporateAction, error)
	ApplyCorporateAction(ctx context.Context, id equities.CorporateActionID) (EquityCorporateActionResultView, error)
}
