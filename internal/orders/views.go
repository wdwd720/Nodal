package orders

import (
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
)

// OrderView is an order as callers see it: engine units and the exact decimal
// amounts they stand for.
type OrderView struct {
	ID                  OrderID
	BookID              BookID
	InstrumentID        instruments.InstrumentID
	VenueLabel          string
	OwnerType           OwnerType
	OwnerID             string
	AccountID           *accounts.AccountID
	HouseAccountID      *econ.HouseAccountID
	IntentID            *econ.IntentID
	ClientOrderID       string
	Side                Side
	Type                OrderType
	TimeInForce         TimeInForce
	STP                 STPMode
	LiquidityIntent     LiquidityIntent
	PostOnly            bool
	ReduceOnly          bool
	StrategyOriginated  bool
	Mode                gates.Mode
	Price               string
	PriceTicks          int64
	Quantity            money.Quantity
	FilledQuantity      money.Quantity
	RemainingQuantity   money.Quantity
	CancelledQuantity   money.Quantity
	QtyLots             int64
	FilledLots          int64
	RemainingLots       int64
	CancelledLots       int64
	ReduceOnlyLimitLots int64
	Status              Status
	RejectReason        string
	CancelReason        string
	ReservationID       string
	AcceptedCommandSeq  *int64
	LastCommandSeq      *int64
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

func viewOf(c *bookConfig, o orderRow) OrderView {
	t := c.Terms
	return OrderView{
		ID: o.ID, BookID: o.BookID, InstrumentID: o.InstrumentID, VenueLabel: VenueLabelInternalBook, OwnerType: o.OwnerType,
		OwnerID: o.OwnerID, AccountID: o.AccountID, HouseAccountID: o.HouseID, IntentID: o.IntentID, ClientOrderID: o.ClientOrderID,
		Side: o.Side, Type: o.Type, TimeInForce: o.TIF, STP: o.STP, LiquidityIntent: o.Liquidity, PostOnly: o.PostOnly,
		ReduceOnly: o.ReduceOnly, StrategyOriginated: o.StrategyOriginated, Mode: o.Mode, Price: t.PriceString(o.PriceTicks),
		PriceTicks: o.PriceTicks, Quantity: t.LotsToQuantity(o.QtyLots), FilledQuantity: t.LotsToQuantity(o.FilledLots),
		RemainingQuantity: t.LotsToQuantity(o.RemainingLots), CancelledQuantity: t.LotsToQuantity(o.CancelledLots),
		QtyLots: o.QtyLots, FilledLots: o.FilledLots, RemainingLots: o.RemainingLots, CancelledLots: o.CancelledLots,
		ReduceOnlyLimitLots: o.ReduceOnlyLimitLots, Status: o.Status, RejectReason: o.RejectReason, CancelReason: o.CancelReason,
		ReservationID: o.ReservationID, AcceptedCommandSeq: o.AcceptedCommandSeq, LastCommandSeq: o.LastCommandSeq,
		CreatedAt: o.CreatedAt, UpdatedAt: o.UpdatedAt,
	}
}

func (s *Service) outcome(c *bookConfig, o orderRow, in econ.Intent) StageOutcome {
	return StageOutcome{Order: viewOf(c, o), Intent: in}
}
