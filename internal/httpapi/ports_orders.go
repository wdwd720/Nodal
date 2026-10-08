package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/orders"
)

// The internal order book (internal/orders), ADR-0020: one persistent,
// journalled book per instrument whose every fill is a SIM_TRADE between two
// parties. Every figure behind this port is value domain SIMULATED. The
// service decides every rule -- the INTERNAL_MATCHING gate and kill switches,
// Risk Kernel V2, the capital reservation, self-trade prevention, the
// house-versus-customer crossing policy -- and the port carries its answers.

// BookAssets names a book's two assets the way a reader needs them: the
// symbol and decimals of the base asset (quantities) and of the quote asset
// (prices, notionals and fees).
type BookAssets struct {
	BaseSymbol    string
	BaseDecimals  uint8
	QuoteSymbol   string
	QuoteDecimals uint8
}

// BookView is a book with what its read discloses beside it: its assets, the
// fee schedules that price every fill (decoded from what the book recorded
// and proven to hash to the book's own schedule hashes) and the crossing rule
// in force.
type BookView struct {
	Book          orders.Book
	Assets        BookAssets
	MakerSchedule fees.Schedule
	TakerSchedule fees.Schedule
	Crossing      orders.CrossingRule
	// CrossingDefault is true when no rule was ever recorded, so the default
	// -- a house book never trades with a customer -- is in force.
	CrossingDefault bool
	// Existed is set by OpenBook when the instrument's book was already open
	// with the same terms, so nothing new was created.
	Existed bool
}

// BookDepthView is one depth read, with the book cursor it was read at.
type BookDepthView struct {
	Book  orders.Book
	Depth orders.Depth
}

// BookOpen opens the book of an instrument.
type BookOpen struct {
	InstrumentID instruments.InstrumentID
	Mode         gates.Mode
	// SpotTerms are a SPOT_PAIR's terms, which it has no canonical row for;
	// nil for every other instrument.
	SpotTerms        *instruments.Terms
	MakerSchedule    fees.Schedule
	TakerSchedule    fees.Schedule
	SnapshotInterval int
}

// BookOrderView is a customer order with its book's assets.
type BookOrderView struct {
	Order  orders.OrderView
	Assets BookAssets
}

// BookTradeView is one trade with the instrument it traded and its book's
// assets.
type BookTradeView struct {
	Trade        orders.Trade
	InstrumentID instruments.InstrumentID
	Assets       BookAssets
}

// BookCommandView is an order after a command, with the trades the command
// made.
type BookCommandView struct {
	Order BookOrderView
	// IntentStatus is the order's economic intent status after a
	// submission; empty for a cancel or a replace.
	IntentStatus string
	// CommandSeq and StateHash are zero when the command never reached the
	// book.
	CommandSeq int64
	StateHash  [32]byte
	Trades     []orders.Trade
}

// BookOrderSubmit is a customer order as the client stated it. Price is the
// exact decimal the client sent; the adapter reads it, without rounding, as a
// price in the book's own quote asset.
type BookOrderSubmit struct {
	InstrumentID   instruments.InstrumentID
	AccountID      accounts.AccountID
	Mode           gates.Mode
	Side           orders.Side
	Type           orders.OrderType
	TimeInForce    orders.TimeInForce
	Price          string
	Quantity       money.Quantity
	PostOnly       bool
	ReduceOnly     bool
	STP            orders.STPMode
	ClientOrderID  string
	IdempotencyKey string
	CorrelationID  string
}

// BookOrderCancel cancels one of the account's orders.
type BookOrderCancel struct {
	OrderID   orders.OrderID
	AccountID accounts.AccountID
}

// BookOrderReplace changes one of the account's resting orders. Quantity is
// the new TOTAL quantity, fills included.
type BookOrderReplace struct {
	OrderID        orders.OrderID
	AccountID      accounts.AccountID
	Price          string
	Quantity       money.Quantity
	IdempotencyKey string
}

// BookTradeProof is the audit handle of one trade: the trade row (journal
// transaction, effect, evidence and schedule hashes) and the book's journal
// cursor and latest snapshot.
type BookTradeProof struct {
	Trade BookTradeView
	Proof orders.BookProof
}

// AdvancedBooksPort serves internal/orders. Commands that the service refuses
// after recording the refusal return that refusal as their error, carrying
// its stable reason and the id of the order it recorded.
type AdvancedBooksPort interface {
	Books(ctx context.Context, cursor string, limit int) ([]BookView, string, error)
	Book(ctx context.Context, instrumentID instruments.InstrumentID) (BookView, error)
	Depth(ctx context.Context, instrumentID instruments.InstrumentID, levels int) (BookDepthView, error)
	OpenBook(ctx context.Context, r BookOpen) (BookView, error)
	SetBookStatus(ctx context.Context, instrumentID instruments.InstrumentID, to orders.BookStatus, reason string) (BookView, error)
	SetCrossingRule(ctx context.Context, instrumentID instruments.InstrumentID, allow bool, reason string) (orders.CrossingRule, error)
	VerifyJournal(ctx context.Context, instrumentID instruments.InstrumentID) (orders.JournalProof, error)

	Submit(ctx context.Context, r BookOrderSubmit) (BookCommandView, error)
	Cancel(ctx context.Context, r BookOrderCancel) (BookCommandView, error)
	Replace(ctx context.Context, r BookOrderReplace) (BookCommandView, error)

	// Order returns one order whatever its owner; the handler decides who may
	// see it.
	Order(ctx context.Context, id orders.OrderID) (BookOrderView, error)
	// Orders pages an account's orders, newest first; workingOnly keeps those
	// still PENDING, OPEN or PARTIALLY_FILLED.
	Orders(ctx context.Context, account accounts.AccountID, workingOnly bool, cursor string, limit int) ([]BookOrderView, string, error)
	// Trades pages the trades an account took part in, newest first.
	Trades(ctx context.Context, account accounts.AccountID, cursor string, limit int) ([]BookTradeView, string, error)
	// TradeProof returns one trade's audit proof whatever its parties; the
	// handler decides who may see it.
	TradeProof(ctx context.Context, instrumentID instruments.InstrumentID, tradeSeq int64) (BookTradeProof, error)
}
