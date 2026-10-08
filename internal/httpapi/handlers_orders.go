package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/fees"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
)

// The internal order book (ADR-0020; internal/orders behind /advanced/books).
// These handlers translate shapes and decide what a reader may see of a
// record; every financial rule -- the gate, Risk Kernel V2, reservations,
// self-trade prevention, the crossing policy -- is the service's.

// bookDepthDefault and bookDepthMax bound a depth read's levels per side.
const (
	bookDepthDefault = 50
	bookDepthMax     = 200
)

// --- books ------------------------------------------------------------------

// GetAdvancedBooks pages the internal books, oldest first.
func (s *Server) GetAdvancedBooks(ctx context.Context, request api.GetAdvancedBooksRequestObject) (api.GetAdvancedBooksResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	items, next, err := s.opts.Ports.Books.Books(ctx, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedBookPage{Items: make([]api.AdvancedBook, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED}
	for _, v := range items {
		page.Items = append(page.Items, toAPIBook(v))
	}
	return api.GetAdvancedBooks200JSONResponse(page), nil
}

// GetAdvancedBooksInstrumentId returns one book.
func (s *Server) GetAdvancedBooksInstrumentId(ctx context.Context, request api.GetAdvancedBooksInstrumentIdRequestObject) (api.GetAdvancedBooksInstrumentIdResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Books.Book(ctx, iid)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedBooksInstrumentId200JSONResponse(toAPIBook(v)), nil
}

// GetAdvancedBooksInstrumentIdDepth returns the resting book, aggregated by
// price level, with the journal cursor it was read at.
func (s *Server) GetAdvancedBooksInstrumentIdDepth(ctx context.Context, request api.GetAdvancedBooksInstrumentIdDepthRequestObject) (api.GetAdvancedBooksInstrumentIdDepthResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	levels := bookDepthDefault
	if request.Params.Levels != nil {
		levels = *request.Params.Levels
		if levels < 1 || levels > bookDepthMax {
			return nil, validationError("levels", "levels must be within 1..200")
		}
	}
	v, err := s.opts.Ports.Books.Depth(ctx, iid, levels)
	if err != nil {
		return nil, err
	}
	b, t := v.Book, v.Book.Terms
	out := api.AdvancedBookDepth{
		BookId: toUUID(b.ID), InstrumentId: toUUID(b.InstrumentID), Status: api.AdvancedBookStatus(b.Status),
		BaseDecimals: int(t.BaseDecimals), QuoteDecimals: int(t.QuoteDecimals),
		Bids: make([]api.AdvancedBookDepthLevel, 0, len(v.Depth.Bids)), Asks: make([]api.AdvancedBookDepthLevel, 0, len(v.Depth.Asks)),
		LastCommandSeq: b.LastCommandSeq, StateHash: hex.EncodeToString(b.StateHash[:]), ValueDomain: api.SIMULATED,
	}
	level := func(l orders.DepthLevel) api.AdvancedBookDepthLevel {
		return api.AdvancedBookDepthLevel{
			Price: l.Price, PriceTicks: l.PriceTicks, Quantity: t.LotsToQuantity(l.QtyLots).String(), Orders: l.Orders,
		}
	}
	for _, l := range v.Depth.Bids {
		out.Bids = append(out.Bids, level(l))
	}
	for _, l := range v.Depth.Asks {
		out.Asks = append(out.Asks, level(l))
	}
	return api.GetAdvancedBooksInstrumentIdDepth200JSONResponse(out), nil
}

// PostAdvancedBooks opens an instrument's book (operator, step-up).
func (s *Server) PostAdvancedBooks(ctx context.Context, request api.PostAdvancedBooksRequestObject) (api.PostAdvancedBooksResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	iid, err := bookInstrumentID(b.InstrumentId)
	if err != nil {
		return nil, err
	}
	maker, err := bookScheduleFrom(b.MakerFee, "maker_fee")
	if err != nil {
		return nil, err
	}
	taker, err := bookScheduleFrom(b.TakerFee, "taker_fee")
	if err != nil {
		return nil, err
	}
	cmd := BookOpen{InstrumentID: iid, Mode: gates.Mode(b.Mode), MakerSchedule: maker, TakerSchedule: taker}
	if b.SnapshotInterval != nil {
		cmd.SnapshotInterval = *b.SnapshotInterval
	}
	if b.SpotTerms != nil {
		terms, terr := fromAPIInstrumentTerms(*b.SpotTerms)
		if terr != nil {
			return nil, terr
		}
		cmd.SpotTerms = &terms
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBook, commandMeta, error) {
			v, oerr := s.opts.Ports.Books.OpenBook(ctx, cmd)
			if oerr != nil {
				return api.AdvancedBook{}, commandMeta{}, oerr
			}
			status := http.StatusCreated
			if v.Existed {
				// The book was already open with these terms: nothing new exists.
				status = http.StatusOK
			}
			return toAPIBook(v), commandMeta{Status: status, ResourceType: "order_book", ResourceID: v.Book.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedBooks200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedBooks201JSONResponse(res.Value), nil
}

// PostAdvancedBooksInstrumentIdStatus halts, resumes or closes a book
// (operator, step-up).
func (s *Server) PostAdvancedBooksInstrumentIdStatus(ctx context.Context, request api.PostAdvancedBooksInstrumentIdStatusRequestObject) (api.PostAdvancedBooksInstrumentIdStatusResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	to, reason := orders.BookStatus(request.Body.Status), request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBook, commandMeta, error) {
			v, serr := s.opts.Ports.Books.SetBookStatus(ctx, iid, to, reason)
			if serr != nil {
				return api.AdvancedBook{}, commandMeta{}, serr
			}
			return toAPIBook(v), commandMeta{Status: http.StatusOK, ResourceType: "order_book", ResourceID: v.Book.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBooksInstrumentIdStatus200JSONResponse(res.Value), nil
}

// PostAdvancedBooksInstrumentIdCrossingRule records the next version of a
// book's house-versus-customer crossing rule (operator, step-up).
func (s *Server) PostAdvancedBooksInstrumentIdCrossingRule(ctx context.Context, request api.PostAdvancedBooksInstrumentIdCrossingRuleRequestObject) (api.PostAdvancedBooksInstrumentIdCrossingRuleResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	allow, reason := request.Body.AllowHouseCustomerCrossing, request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBookCrossingRule, commandMeta, error) {
			r, serr := s.opts.Ports.Books.SetCrossingRule(ctx, iid, allow, reason)
			if serr != nil {
				return api.AdvancedBookCrossingRule{}, commandMeta{}, serr
			}
			return toAPIBookCrossingRule(r, false), commandMeta{
				Status: http.StatusOK, ResourceType: "order_book_crossing_rule", ResourceID: r.BookID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBooksInstrumentIdCrossingRule200JSONResponse(res.Value), nil
}

// GetAdvancedBooksInstrumentIdJournalVerification replays a book's journal
// from the database and reports what it proved (operator, read-only).
func (s *Server) GetAdvancedBooksInstrumentIdJournalVerification(ctx context.Context, request api.GetAdvancedBooksInstrumentIdJournalVerificationRequestObject) (api.GetAdvancedBooksInstrumentIdJournalVerificationResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	p, err := s.opts.Ports.Books.VerifyJournal(ctx, iid)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedBooksInstrumentIdJournalVerification200JSONResponse(api.AdvancedBookJournalVerification{
		BookId: toUUID(p.BookID), InstrumentId: toUUID(p.InstrumentID), Commands: p.Commands, Events: p.Events,
		Trades: p.Trades, GenesisHash: hex.EncodeToString(p.GenesisHash[:]), StateHash: hex.EncodeToString(p.StateHash[:]),
		FullReplayHash: hex.EncodeToString(p.FullReplayHash[:]), SnapshotReplayHash: hex.EncodeToString(p.SnapshotReplayHash[:]),
		SnapshotSeq: p.SnapshotSeq, OrdersChecked: p.OrdersChecked, VerifiedAt: s.opts.Clock.Now().UTC(),
	}), nil
}

// --- orders -----------------------------------------------------------------

// PostAdvancedBooksInstrumentIdOrders submits a customer order to a book from
// one of the caller's own accounts.
func (s *Server) PostAdvancedBooksInstrumentIdOrders(ctx context.Context, request api.PostAdvancedBooksInstrumentIdOrdersRequestObject) (api.PostAdvancedBooksInstrumentIdOrdersResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	qty, err := bookQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	cmd := BookOrderSubmit{
		InstrumentID: iid, AccountID: account, Side: orders.Side(b.Side), Type: orders.OrderType(b.OrderType),
		TimeInForce: orders.TimeInForce(b.TimeInForce), Price: b.Price, Quantity: qty,
		PostOnly: b.PostOnly != nil && *b.PostOnly, ReduceOnly: b.ReduceOnly != nil && *b.ReduceOnly,
		ClientOrderID: deref(b.ClientOrderId), IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID: observability.CorrelationID(ctx),
	}
	if b.Stp != nil {
		cmd.STP = orders.STPMode(*b.Stp)
	}
	if b.Mode != nil {
		cmd.Mode = gates.Mode(*b.Mode)
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBookCommandResult, commandMeta, error) {
			v, serr := s.opts.Ports.Books.Submit(ctx, cmd)
			if serr != nil {
				return api.AdvancedBookCommandResult{}, commandMeta{}, serr
			}
			return toAPIBookCommand(v, account.String()), commandMeta{
				Status: http.StatusCreated, ResourceType: "book_order", ResourceID: v.Order.Order.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedBooksInstrumentIdOrders200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedBooksInstrumentIdOrders201JSONResponse(res.Value), nil
}

// GetAdvancedBooksOrders pages an account's orders.
func (s *Server) GetAdvancedBooksOrders(ctx context.Context, request api.GetAdvancedBooksOrdersRequestObject) (api.GetAdvancedBooksOrdersResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	working := true
	if request.Params.Scope != nil {
		switch *request.Params.Scope {
		case api.BookOrderScopeWorking:
		case api.BookOrderScopeAll:
			working = false
		default:
			return nil, validationError("scope", "scope must be WORKING or ALL")
		}
	}
	items, next, err := s.opts.Ports.Books.Orders(ctx, account, working, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedBookOrderPage{Items: make([]api.AdvancedBookOrder, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED}
	for _, v := range items {
		page.Items = append(page.Items, toAPIBookOrder(v))
	}
	return api.GetAdvancedBooksOrders200JSONResponse(page), nil
}

// GetAdvancedBooksOrdersOrderId returns one order to its owner or an operator
// who may read any account. Anyone else gets the answer an absent id gets.
func (s *Server) GetAdvancedBooksOrdersOrderId(ctx context.Context, request api.GetAdvancedBooksOrdersOrderIdRequestObject) (api.GetAdvancedBooksOrdersOrderIdResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	oid, err := bookOrderID(request.OrderId)
	if err != nil {
		return nil, err
	}
	v, err := s.visibleBookOrder(ctx, oid, securityRequireAccount)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedBooksOrdersOrderId200JSONResponse(toAPIBookOrder(v)), nil
}

// PostAdvancedBooksOrdersOrderIdCancel cancels one of the caller's own orders.
func (s *Server) PostAdvancedBooksOrdersOrderIdCancel(ctx context.Context, request api.PostAdvancedBooksOrdersOrderIdCancelRequestObject) (api.PostAdvancedBooksOrdersOrderIdCancelResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	oid, err := bookOrderID(request.OrderId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBookCommandResult, commandMeta, error) {
			if _, oerr := s.ownBookOrder(ctx, oid, account); oerr != nil {
				return api.AdvancedBookCommandResult{}, commandMeta{}, oerr
			}
			v, cerr := s.opts.Ports.Books.Cancel(ctx, BookOrderCancel{OrderID: oid, AccountID: account})
			if cerr != nil {
				return api.AdvancedBookCommandResult{}, commandMeta{}, cerr
			}
			return toAPIBookCommand(v, account.String()), commandMeta{
				Status: http.StatusOK, ResourceType: "book_order", ResourceID: oid.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBooksOrdersOrderIdCancel200JSONResponse(res.Value), nil
}

// PostAdvancedBooksOrdersOrderIdReplace changes one of the caller's own
// resting orders.
func (s *Server) PostAdvancedBooksOrdersOrderIdReplace(ctx context.Context, request api.PostAdvancedBooksOrdersOrderIdReplaceRequestObject) (api.PostAdvancedBooksOrdersOrderIdReplaceResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	oid, err := bookOrderID(request.OrderId)
	if err != nil {
		return nil, err
	}
	qty, err := bookQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	cmd := BookOrderReplace{OrderID: oid, AccountID: account, Price: b.Price, Quantity: qty, IdempotencyKey: request.Params.IdempotencyKey}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedBookCommandResult, commandMeta, error) {
			if _, oerr := s.ownBookOrder(ctx, oid, account); oerr != nil {
				return api.AdvancedBookCommandResult{}, commandMeta{}, oerr
			}
			v, rerr := s.opts.Ports.Books.Replace(ctx, cmd)
			if rerr != nil {
				return api.AdvancedBookCommandResult{}, commandMeta{}, rerr
			}
			return toAPIBookCommand(v, account.String()), commandMeta{
				Status: http.StatusOK, ResourceType: "book_order", ResourceID: oid.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedBooksOrdersOrderIdReplace200JSONResponse(res.Value), nil
}

// visibleBookOrder returns a customer order to a principal the given rule
// admits for the order's own account, and NOT_FOUND -- the answer an absent
// id gets -- to anyone else, so a refusal never confirms that an id exists.
func (s *Server) visibleBookOrder(ctx context.Context, oid orders.OrderID, admit func(context.Context, string) error) (BookOrderView, error) {
	v, err := s.opts.Ports.Books.Order(ctx, oid)
	if err != nil {
		return BookOrderView{}, err
	}
	o := v.Order
	if o.OwnerType != orders.OwnerCustomer || o.AccountID == nil || admit(ctx, o.AccountID.String()) != nil {
		return BookOrderView{}, bookOrderNotFound(oid)
	}
	return v, nil
}

// ownBookOrder admits a command only on an order of the account the caller
// named and owns; any other order is NOT_FOUND.
func (s *Server) ownBookOrder(ctx context.Context, oid orders.OrderID, account accounts.AccountID) (BookOrderView, error) {
	return s.visibleBookOrder(ctx, oid, func(_ context.Context, owner string) error {
		if owner != account.String() {
			return errs.New(errs.CodeNotFound, "not this account's order")
		}
		return nil
	})
}

// bookOrderNotFound is the one answer for an order a principal may not see
// and an order that does not exist; it matches the service's own.
func bookOrderNotFound(oid orders.OrderID) error {
	return errs.New(errs.CodeNotFound, "orders: order not found").WithField("order_id", oid.String())
}

// GetAdvancedBooksTrades pages an account's fills, each from its own side.
func (s *Server) GetAdvancedBooksTrades(ctx context.Context, request api.GetAdvancedBooksTradesRequestObject) (api.GetAdvancedBooksTradesResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := s.opts.Ports.Books.Trades(ctx, account, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedBookFillPage{Items: make([]api.AdvancedBookFill, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED}
	for _, v := range items {
		f, ok := toAPIBookFill(v.Trade, v.InstrumentID, v.Assets, account.String())
		if !ok {
			// The listing selects the account's trades only; a row that is
			// not the account's is not shown, whatever put it there.
			continue
		}
		page.Items = append(page.Items, f)
	}
	return api.GetAdvancedBooksTrades200JSONResponse(page), nil
}

// GetAdvancedBooksInstrumentIdTradesTradeSeqProof returns a trade's audit
// proof to one of its parties or an operator who may read any account; anyone
// else is told the trade does not exist.
func (s *Server) GetAdvancedBooksInstrumentIdTradesTradeSeqProof(ctx context.Context, request api.GetAdvancedBooksInstrumentIdTradesTradeSeqProofRequestObject) (api.GetAdvancedBooksInstrumentIdTradesTradeSeqProofResponseObject, error) {
	if s.opts.Ports.Books == nil {
		return nil, errNotWired("the internal order book")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	if request.TradeSeq < 1 {
		return nil, validationError("tradeSeq", "tradeSeq must be a positive trade sequence")
	}
	p, err := s.opts.Ports.Books.TradeProof(ctx, iid, request.TradeSeq)
	if err != nil {
		return nil, err
	}
	t := p.Trade.Trade
	if securityRequireAccount(ctx, t.MakerOwnerID) != nil && securityRequireAccount(ctx, t.TakerOwnerID) != nil {
		// NOT_FOUND, never FORBIDDEN: a distinguishable refusal would confirm
		// that the trade exists (F-41).
		return nil, bookTradeNotFound(iid, request.TradeSeq)
	}
	as := p.Trade.Assets
	out := api.AdvancedBookTradeProof{
		BookId: toUUID(t.BookID), InstrumentId: toUUID(p.Trade.InstrumentID), VenueLabel: api.BookVenueSimulatedInternalBook,
		TradeSeq: t.TradeSeq, CommandSeq: t.CommandSeq, EventSeq: t.EventSeq, Price: bookPriceString(t),
		PriceTicks: t.PriceTicks, Quantity: t.BaseQuantity.String(), QuoteNotional: t.QuoteNotional.String(),
		AggressorSide: api.AdvancedBookSide(t.AggressorSide), MakerOrderId: toUUID(t.MakerOrderID), TakerOrderId: toUUID(t.TakerOrderID),
		MakerOwnerType: api.AdvancedBookOwnerType(t.MakerOwnerType), TakerOwnerType: api.AdvancedBookOwnerType(t.TakerOwnerType),
		MakerFee: t.MakerFee.String(), TakerFee: t.TakerFee.String(), MakerScheduleHash: t.MakerScheduleHash,
		TakerScheduleHash: t.TakerScheduleHash, CrossingRuleVersion: t.CrossingRuleVersion,
		JournalTransactionId: toUUID(t.JournalTransactionID), EffectId: toUUID(t.EffectID), EvidenceRef: t.EvidenceRef,
		EvidenceSha256: hex.EncodeToString(t.EvidenceSHA256), ExecutedAt: t.Price.At.UTC(),
		BaseSymbol: strPtr(as.BaseSymbol), BaseDecimals: int(as.BaseDecimals), QuoteSymbol: strPtr(as.QuoteSymbol),
		QuoteDecimals: int(as.QuoteDecimals), BookLastCommandSeq: p.Proof.LastCommandSeq,
		BookStateHash: hex.EncodeToString(p.Proof.StateHash[:]), SnapshotSeq: p.Proof.SnapshotSeq, ValueDomain: api.SIMULATED,
	}
	if len(p.Proof.SnapshotHash) > 0 {
		h := hex.EncodeToString(p.Proof.SnapshotHash)
		out.SnapshotHash = &h
	}
	return api.GetAdvancedBooksInstrumentIdTradesTradeSeqProof200JSONResponse(out), nil
}

// --- parsing ----------------------------------------------------------------

func bookInstrumentID(u api.UUID) (instruments.InstrumentID, error) {
	iid, err := instruments.ParseInstrumentID(u.String())
	if err != nil || iid.IsZero() {
		return instruments.InstrumentID{}, validationError("instrumentId", "instrumentId must be a canonical UUID")
	}
	return iid, nil
}

func bookOrderID(u api.UUID) (orders.OrderID, error) {
	oid, err := orders.ParseOrderID(u.String())
	if err != nil || oid.IsZero() {
		return orders.OrderID{}, validationError("orderId", "orderId must be a canonical UUID")
	}
	return oid, nil
}

// bookQuantity reads exact base units.
func bookQuantity(s, field string) (money.Quantity, error) {
	q, err := money.ParseQuantity(s)
	if err != nil {
		return money.Quantity{}, validationError(field, field+" must be an exact integer number of base units")
	}
	return q, nil
}

// bookScheduleFrom is the operator's fee schedule for one side of a book: the
// components the internal book may charge, and nothing else, rounding in the
// customer's favour. The service decides whether the book accepts it.
func bookScheduleFrom(r api.AdvancedBookFeeScheduleRequest, field string) (fees.Schedule, error) {
	quote, err := assets.ParseAssetID(r.QuoteAssetId.String())
	if err != nil || quote.IsZero() {
		return fees.Schedule{}, validationError(field+".quote_asset_id", "quote_asset_id must be a canonical UUID")
	}
	if r.QuoteDecimals < 0 || r.QuoteDecimals > 18 {
		return fees.Schedule{}, validationError(field+".quote_decimals", "quote_decimals must be within 0..18")
	}
	s := fees.ZeroSchedule(quote, uint8(r.QuoteDecimals)) // #nosec G115 -- bounded to 0..18 just above
	s.Version = r.Version
	s.PercentageFeeBPS = money.BPS(r.PercentageFeeBps)
	if r.StrategyFeeBps != nil {
		s.StrategyFeeBPS = money.BPS(*r.StrategyFeeBps)
	}
	if r.SubscriptionDiscountBps != nil {
		s.SubscriptionDiscountBPS = money.BPS(*r.SubscriptionDiscountBps)
	}
	if r.MaxTotalFeeBps != nil {
		s.MaxTotalFeeBPS = money.BPS(*r.MaxTotalFeeBps)
	}
	return s, nil
}

// fromAPIInstrumentTerms reads instrument terms as the contract states them.
// Every rule the terms must satisfy is instruments.Terms.Validate's, which the
// service runs; this only parses identifiers and exact integers.
func fromAPIInstrumentTerms(t api.InstrumentTerms) (instruments.Terms, error) {
	const field = "spot_terms"
	settlement, err := assets.ParseAssetID(t.SettlementAssetId.String())
	if err != nil || settlement.IsZero() {
		return instruments.Terms{}, validationError(field+".settlement_asset_id", "settlement_asset_id must be a canonical UUID")
	}
	out := instruments.Terms{
		Kind: instruments.InstrumentType(t.Kind), Symbol: t.Symbol, VenueCode: t.VenueCode, ExchangeMIC: deref(t.ExchangeMic),
		SettlementAssetID: settlement, SettlementStyle: instruments.SettlementStyle(t.SettlementStyle),
		TradingCalendar: t.TradingCalendar, JurisdictionTags: append([]string{}, t.JurisdictionTags...),
		OracleMaxAgeMS: t.OracleMaxAgeMs, OracleSources: append([]string{}, t.OracleSources...),
		MarginModel:             instruments.MarginModel(t.MarginModel),
		CorporateActionBehavior: instruments.CorporateActionBehavior(t.CorporateActionBehavior),
		Transferability:         instruments.Transferability(t.Transferability),
	}
	if t.PriceScale < 0 || t.PriceScale > 18 || t.QuantityDecimals < 0 || t.QuantityDecimals > 18 {
		return instruments.Terms{}, validationError(field, "price_scale and quantity_decimals must be within 0..18")
	}
	out.PriceScale, out.QuantityDecimals = int32(t.PriceScale), uint8(t.QuantityDecimals) // #nosec G115 -- bounded just above
	for _, p := range []struct {
		name string
		src  *api.UUID
		dst  **assets.AssetID
	}{{"base_asset_id", t.BaseAssetId, &out.BaseAssetID}, {"quote_asset_id", t.QuoteAssetId, &out.QuoteAssetID}} {
		if p.src == nil {
			continue
		}
		a, perr := assets.ParseAssetID(p.src.String())
		if perr != nil || a.IsZero() {
			return instruments.Terms{}, validationError(field+"."+p.name, p.name+" must be a canonical UUID")
		}
		*p.dst = &a
	}
	if t.UnderlyingInstrumentId != nil {
		u, perr := instruments.ParseInstrumentID(t.UnderlyingInstrumentId.String())
		if perr != nil || u.IsZero() {
			return instruments.Terms{}, validationError(field+".underlying_instrument_id", "underlying_instrument_id must be a canonical UUID")
		}
		out.UnderlyingInstrumentID = &u
	}
	for _, p := range []struct {
		name string
		src  string
		dst  *money.Quantity
	}{{"contract_multiplier", t.ContractMultiplier, &out.ContractMultiplier}, {"tick_size", t.TickSize, &out.TickSize},
		{"lot_size", t.LotSize, &out.LotSize}, {"min_quantity", t.MinQuantity, &out.MinQuantity}} {
		q, perr := bookQuantity(p.src, field+"."+p.name)
		if perr != nil {
			return instruments.Terms{}, perr
		}
		*p.dst = q
	}
	if t.StrikeMantissa != nil {
		q, perr := bookQuantity(*t.StrikeMantissa, field+".strike_mantissa")
		if perr != nil {
			return instruments.Terms{}, perr
		}
		out.StrikeMantissa = &q
	}
	if t.Expiry != nil {
		e := t.Expiry.UTC()
		out.Expiry = &e
	}
	if t.OptionRight != nil {
		out.OptionRight = instruments.OptionRight(*t.OptionRight)
	}
	if t.ExerciseStyle != nil {
		out.ExerciseStyle = instruments.ExerciseStyle(*t.ExerciseStyle)
	}
	if t.FundingIntervalMs != nil {
		out.FundingIntervalMS = *t.FundingIntervalMs
	}
	if t.Outcomes != nil {
		out.Outcomes = append([]string{}, (*t.Outcomes)...)
	}
	return out, nil
}

// --- rendering --------------------------------------------------------------

func toAPIBook(v BookView) api.AdvancedBook {
	b, t := v.Book, v.Book.Terms
	out := api.AdvancedBook{
		Id: toUUID(b.ID), InstrumentId: toUUID(b.InstrumentID), VenueLabel: api.AdvancedBookVenueLabel(b.VenueLabel),
		Mode: api.AdvancedBookMode(b.Mode), Status: api.AdvancedBookStatus(b.Status), InstrumentKind: api.InstrumentKind(t.Kind),
		BaseAssetId: toUUID(t.BaseAsset), BaseSymbol: v.Assets.BaseSymbol, BaseDecimals: int(t.BaseDecimals),
		QuoteAssetId: toUUID(t.QuoteAsset), QuoteSymbol: v.Assets.QuoteSymbol, QuoteDecimals: int(t.QuoteDecimals),
		PriceScale: int(t.PriceScale), TickPrice: t.PriceString(1), LotSize: t.LotSize.String(),
		MinQuantity: t.MinQuantity.String(), MaxPrice: t.PriceString(t.MaxPriceTicks), TradingCalendar: t.TradingCalendar,
		TermsSource:  api.AdvancedBookTermsSource(b.TermsSource),
		MakerFee:     toAPIBookSchedule(v.MakerSchedule, b.MakerScheduleHash),
		TakerFee:     toAPIBookSchedule(v.TakerSchedule, b.TakerScheduleHash),
		CrossingRule: toAPIBookCrossingRule(v.Crossing, v.CrossingDefault), SnapshotInterval: b.SnapshotInterval,
		LastCommandSeq: b.LastCommandSeq, LastEventSeq: b.LastEventSeq, LastTradeSeq: b.LastTradeSeq,
		StateHash: hex.EncodeToString(b.StateHash[:]), ValueDomain: api.SIMULATED,
	}
	if b.TermsSource == orders.TermsCanonical {
		tv := b.TermsVersion
		out.TermsVersion = &tv
	}
	return out
}

func toAPIBookSchedule(s fees.Schedule, hash string) api.AdvancedBookFeeSchedule {
	return api.AdvancedBookFeeSchedule{
		Version: s.Version, QuoteAssetId: toUUID(s.QuoteAsset), QuoteDecimals: int(s.QuoteDecimals),
		PercentageFeeBps: int(s.PercentageFeeBPS), StrategyFeeBps: int(s.StrategyFeeBPS),
		SubscriptionDiscountBps: int(s.SubscriptionDiscountBPS), MaxTotalFeeBps: int(s.MaxTotalFeeBPS), Hash: hash,
	}
}

func toAPIBookCrossingRule(r orders.CrossingRule, isDefault bool) api.AdvancedBookCrossingRule {
	out := api.AdvancedBookCrossingRule{
		BookId: toUUID(r.BookID), Version: r.Version, AllowHouseCustomerCrossing: r.AllowHouseCustomerCrossing, IsDefault: isDefault,
	}
	if isDefault {
		return out
	}
	out.Reason, out.ActorType, out.ActorId = strPtr(r.Reason), strPtr(string(r.Actor.Type)), strPtr(r.Actor.ID)
	if !r.CreatedAt.IsZero() {
		c := r.CreatedAt.UTC()
		out.CreatedAt = &c
	}
	return out
}

func toAPIBookOrder(v BookOrderView) api.AdvancedBookOrder {
	o, as := v.Order, v.Assets
	out := api.AdvancedBookOrder{
		Id: toUUID(o.ID), BookId: toUUID(o.BookID), InstrumentId: toUUID(o.InstrumentID),
		VenueLabel: api.AdvancedBookVenueLabel(o.VenueLabel), ClientOrderId: strPtr(o.ClientOrderID),
		Side: api.AdvancedBookSide(o.Side), OrderType: api.AdvancedBookOrderType(o.Type),
		TimeInForce: api.AdvancedBookTimeInForce(o.TimeInForce), Stp: api.AdvancedBookSTP(o.STP),
		LiquidityIntent: api.AdvancedBookLiquidityIntent(o.LiquidityIntent), PostOnly: o.PostOnly, ReduceOnly: o.ReduceOnly,
		StrategyOriginated: o.StrategyOriginated, Mode: api.AdvancedBookMode(o.Mode), Price: o.Price, PriceTicks: o.PriceTicks,
		Quantity: o.Quantity.String(), FilledQuantity: o.FilledQuantity.String(), RemainingQuantity: o.RemainingQuantity.String(),
		CancelledQuantity: o.CancelledQuantity.String(), BaseSymbol: strPtr(as.BaseSymbol), BaseDecimals: int(as.BaseDecimals),
		QuoteSymbol: strPtr(as.QuoteSymbol), QuoteDecimals: int(as.QuoteDecimals), Status: api.AdvancedBookOrderStatus(o.Status),
		RejectReason: strPtr(o.RejectReason), CancelReason: strPtr(o.CancelReason), ReservationId: parseUUIDText(o.ReservationID),
		AcceptedCommandSeq: o.AcceptedCommandSeq, LastCommandSeq: o.LastCommandSeq,
		CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(), ValueDomain: api.SIMULATED,
	}
	if o.AccountID != nil {
		out.AccountId = toUUID(*o.AccountID)
	}
	if o.IntentID != nil {
		out.IntentId = uuidPtr(*o.IntentID)
	}
	return out
}

// toAPIBookFill renders one trade from one account's own side: its order, its
// side, the role its order played and its own fee. The counterparty is named
// only by owner type. ok is false when the account is not a party.
func toAPIBookFill(t orders.Trade, instrumentID instruments.InstrumentID, as BookAssets, account string) (api.AdvancedBookFill, bool) {
	var (
		order        orders.OrderID
		side         orders.Side
		role         api.AdvancedBookLiquidity
		fee          money.Quantity
		counterparty orders.OwnerType
	)
	switch {
	case t.MakerOwnerType == orders.OwnerCustomer && t.MakerOwnerID == account:
		// The maker rests on the side the aggressor took from.
		order, role, fee, counterparty = t.MakerOrderID, api.BookLiquidityMaker, t.MakerFee, t.TakerOwnerType
		side = orders.SideBuy
		if t.AggressorSide == orders.SideBuy {
			side = orders.SideSell
		}
	case t.TakerOwnerType == orders.OwnerCustomer && t.TakerOwnerID == account:
		order, role, fee, counterparty = t.TakerOrderID, api.BookLiquidityTaker, t.TakerFee, t.MakerOwnerType
		side = t.AggressorSide
	default:
		return api.AdvancedBookFill{}, false
	}
	return api.AdvancedBookFill{
		TradeSeq: t.TradeSeq, BookId: toUUID(t.BookID), InstrumentId: toUUID(instrumentID), OrderId: toUUID(order),
		Side: api.AdvancedBookSide(side), Liquidity: role, Price: bookPriceString(t), PriceTicks: t.PriceTicks,
		Quantity: t.BaseQuantity.String(), QuoteNotional: t.QuoteNotional.String(), Fee: fee.String(),
		CounterpartyOwnerType: api.AdvancedBookOwnerType(counterparty), CrossingRuleVersion: t.CrossingRuleVersion,
		CommandSeq: t.CommandSeq, JournalTransactionId: toUUID(t.JournalTransactionID), EffectId: toUUID(t.EffectID),
		EvidenceRef: t.EvidenceRef, EvidenceSha256: hex.EncodeToString(t.EvidenceSHA256), ExecutedAt: t.Price.At.UTC(),
		BaseSymbol: strPtr(as.BaseSymbol), BaseDecimals: int(as.BaseDecimals), QuoteSymbol: strPtr(as.QuoteSymbol),
		QuoteDecimals: int(as.QuoteDecimals), ValueDomain: api.SIMULATED,
	}, true
}

// bookPriceString renders a trade's exact price at its book's price scale.
func bookPriceString(t orders.Trade) string {
	return t.Price.Mantissa.ToDecimalString(uint8(t.Price.Scale)) // #nosec G115 -- a book's price scale is CHECKed 0..18
}

func toAPIBookCommand(v BookCommandView, account string) api.AdvancedBookCommandResult {
	out := api.AdvancedBookCommandResult{
		Order: toAPIBookOrder(v.Order), IntentStatus: strPtr(v.IntentStatus),
		Fills: make([]api.AdvancedBookFill, 0, len(v.Trades)), ValueDomain: api.SIMULATED,
	}
	if v.CommandSeq > 0 {
		seq := v.CommandSeq
		h := hex.EncodeToString(v.StateHash[:])
		out.CommandSeq, out.StateHash = &seq, &h
	}
	for _, t := range v.Trades {
		if f, ok := toAPIBookFill(t, v.Order.Order.InstrumentID, v.Order.Assets, account); ok {
			out.Fills = append(out.Fills, f)
		}
	}
	return out
}
