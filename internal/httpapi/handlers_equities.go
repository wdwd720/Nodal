package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"regexp"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/equities"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// Paper and sandbox equities (ADR-0020; internal/equities behind
// /advanced/equities). These handlers translate shapes and decide what a
// reader may see of a record; every financial rule -- sessions, lots, the
// limit grid, pending corporate actions, Risk Kernel V2, reservations, T+1
// settlement -- is the service's.

// --- listings and the calendar ------------------------------------------------

// GetAdvancedEquitiesListings lists the equity and ETF listings with the
// calendar's state.
func (s *Server) GetAdvancedEquitiesListings(ctx context.Context, request api.GetAdvancedEquitiesListingsRequestObject) (api.GetAdvancedEquitiesListingsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	v, err := s.opts.Ports.Equities.Listings(ctx, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.AdvancedEquityListingList{
		Items: make([]api.AdvancedEquityListing, 0, len(v.Items)), Session: toAPIEquitySession(v.Session), ValueDomain: api.SIMULATED,
	}
	for _, l := range v.Items {
		out.Items = append(out.Items, toAPIEquityListing(l))
	}
	return api.GetAdvancedEquitiesListings200JSONResponse(out), nil
}

// GetAdvancedEquitiesListingsInstrumentId returns one listing.
func (s *Server) GetAdvancedEquitiesListingsInstrumentId(ctx context.Context, request api.GetAdvancedEquitiesListingsInstrumentIdRequestObject) (api.GetAdvancedEquitiesListingsInstrumentIdResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	iid, err := bookInstrumentID(request.InstrumentId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Equities.Listing(ctx, iid)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedEquitiesListingsInstrumentId200JSONResponse(toAPIEquityListing(v)), nil
}

// PostAdvancedEquitiesListings lists a simulated equity or ETF (operator,
// step-up).
func (s *Server) PostAdvancedEquitiesListings(ctx context.Context, request api.PostAdvancedEquitiesListingsRequestObject) (api.PostAdvancedEquitiesListingsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	if b.ShareDecimals < 0 || b.ShareDecimals > 18 {
		return nil, validationError("share_decimals", "share_decimals must be within 0..18")
	}
	if b.PriceScale < 0 || b.PriceScale > 18 {
		return nil, validationError("price_scale", "price_scale must be within 0..18")
	}
	spec := equities.ListingSpec{
		Kind: instruments.InstrumentType(b.Kind), Symbol: b.Symbol, Name: b.Name, ExchangeMIC: b.ExchangeMic,
		ShareDecimals: uint8(b.ShareDecimals), PriceScale: int32(b.PriceScale), // #nosec G115 -- both bounded to 0..18 above
		OracleMaxAgeMS: b.OracleMaxAgeMs, OracleSources: append([]string{}, b.OracleSources...),
		RiskClass: assets.RiskClass(b.RiskClass),
	}
	var err error
	if spec.TickSize, err = bookQuantity(b.TickSize, "tick_size"); err != nil {
		return nil, err
	}
	if spec.LotSize, err = bookQuantity(b.LotSize, "lot_size"); err != nil {
		return nil, err
	}
	if spec.MinQuantity, err = bookQuantity(b.MinQuantity, "min_quantity"); err != nil {
		return nil, err
	}
	cmd := EquityListingRequest{Spec: spec, DefaultSettlement: b.SettlementAssetId == nil}
	if b.SettlementAssetId != nil {
		aid, perr := assets.ParseAssetID(b.SettlementAssetId.String())
		if perr != nil || aid.IsZero() {
			return nil, validationError("settlement_asset_id", "settlement_asset_id must be a canonical UUID")
		}
		cmd.Spec.CashAssetID = aid
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityListing, commandMeta, error) {
			v, lerr := s.opts.Ports.Equities.ListEquity(ctx, cmd)
			if lerr != nil {
				return api.AdvancedEquityListing{}, commandMeta{}, lerr
			}
			return toAPIEquityListing(v), commandMeta{
				Status: http.StatusCreated, ResourceType: "equity_listing", ResourceID: v.Listing.Instrument.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedEquitiesListings200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedEquitiesListings201JSONResponse(res.Value), nil
}

// GetAdvancedEquitiesCalendar returns the calendar's state and its holidays.
func (s *Server) GetAdvancedEquitiesCalendar(ctx context.Context, _ api.GetAdvancedEquitiesCalendarRequestObject) (api.GetAdvancedEquitiesCalendarResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	v, err := s.opts.Ports.Equities.Calendar(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedEquitiesCalendar200JSONResponse(toAPIEquityCalendar(v)), nil
}

// PostAdvancedEquitiesCalendarHolidays records a full-day closure (operator,
// step-up).
func (s *Server) PostAdvancedEquitiesCalendarHolidays(ctx context.Context, request api.PostAdvancedEquitiesCalendarHolidaysRequestObject) (api.PostAdvancedEquitiesCalendarHolidaysResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	day, err := equityDate(request.Body.Date, "date")
	if err != nil {
		return nil, err
	}
	h := equities.Holiday{Date: day, Name: request.Body.Name}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityCalendar, commandMeta, error) {
			v, herr := s.opts.Ports.Equities.AddHoliday(ctx, h)
			if herr != nil {
				return api.AdvancedEquityCalendar{}, commandMeta{}, herr
			}
			return toAPIEquityCalendar(v), commandMeta{
				Status: http.StatusOK, ResourceType: "equity_calendar_holiday", ResourceID: equities.CalendarXNYS + "/" + day.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesCalendarHolidays200JSONResponse(res.Value), nil
}

// --- orders -------------------------------------------------------------------

// PostAdvancedEquitiesOrders submits an equity order from one of the caller's
// own accounts and places it at the simulated venue.
func (s *Server) PostAdvancedEquitiesOrders(ctx context.Context, request api.PostAdvancedEquitiesOrdersRequestObject) (api.PostAdvancedEquitiesOrdersResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	iid, err := bookInstrumentID(b.InstrumentId)
	if err != nil {
		return nil, validationError("instrument_id", "instrument_id must be a canonical UUID")
	}
	qty, err := bookQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	req := equities.OrderRequest{
		AccountID: account, InstrumentID: iid, Side: equities.Side(b.Side), Type: equities.OrderType(b.OrderType),
		Quantity: qty, ExtendedHours: b.ExtendedHours != nil && *b.ExtendedHours, Mode: gates.Mode(b.Mode),
		IdempotencyKey: request.Params.IdempotencyKey, ActorType: p.ActorType, ActorID: p.SubjectID,
		CorrelationID: observability.CorrelationID(ctx),
	}
	if b.LimitPrice != nil {
		limit, perr := equityPricePoint(*b.LimitPrice, "limit_price")
		if perr != nil {
			return nil, perr
		}
		req.LimitPrice = &limit
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityOrderSubmission, commandMeta, error) {
			v, serr := s.opts.Ports.Equities.Submit(ctx, EquityOrderSubmit{Request: req})
			if serr != nil {
				return api.AdvancedEquityOrderSubmission{}, commandMeta{}, serr
			}
			return api.AdvancedEquityOrderSubmission{
				Order: toAPIEquityOrder(v.Order), Placement: toAPIEquityPlacement(v.Placement), ValueDomain: api.SIMULATED,
			}, commandMeta{
				Status: http.StatusCreated, ResourceType: "equity_order", ResourceID: v.Order.Order.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedEquitiesOrders200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedEquitiesOrders201JSONResponse(res.Value), nil
}

// GetAdvancedEquitiesOrders pages an account's equity orders.
func (s *Server) GetAdvancedEquitiesOrders(ctx context.Context, request api.GetAdvancedEquitiesOrdersRequestObject) (api.GetAdvancedEquitiesOrdersResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := s.opts.Ports.Equities.Orders(ctx, account, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedEquityOrderPage{Items: make([]api.AdvancedEquityOrder, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED}
	for _, v := range items {
		page.Items = append(page.Items, toAPIEquityOrder(v))
	}
	return api.GetAdvancedEquitiesOrders200JSONResponse(page), nil
}

// GetAdvancedEquitiesOrdersOrderId returns one order to its owner or an
// operator who may read any account; anyone else gets the answer an absent
// id gets.
func (s *Server) GetAdvancedEquitiesOrdersOrderId(ctx context.Context, request api.GetAdvancedEquitiesOrdersOrderIdRequestObject) (api.GetAdvancedEquitiesOrdersOrderIdResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	oid, err := equityOrderID(request.OrderId)
	if err != nil {
		return nil, err
	}
	v, err := s.visibleEquityOrder(ctx, oid, securityRequireAccount)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedEquitiesOrdersOrderId200JSONResponse(toAPIEquityOrder(v)), nil
}

// PostAdvancedEquitiesOrdersOrderIdCancel asks the venue to cancel one of the
// caller's own orders.
func (s *Server) PostAdvancedEquitiesOrdersOrderIdCancel(ctx context.Context, request api.PostAdvancedEquitiesOrdersOrderIdCancelRequestObject) (api.PostAdvancedEquitiesOrdersOrderIdCancelResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	oid, err := equityOrderID(request.OrderId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityOrder, commandMeta, error) {
			if _, oerr := s.visibleEquityOrder(ctx, oid, ownedBy(account)); oerr != nil {
				return api.AdvancedEquityOrder{}, commandMeta{}, oerr
			}
			v, cerr := s.opts.Ports.Equities.Cancel(ctx, oid)
			if cerr != nil {
				return api.AdvancedEquityOrder{}, commandMeta{}, cerr
			}
			return toAPIEquityOrder(v), commandMeta{Status: http.StatusOK, ResourceType: "equity_order", ResourceID: oid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesOrdersOrderIdCancel200JSONResponse(res.Value), nil
}

// visibleEquityOrder returns an order to a principal the rule admits for the
// order's own account and NOT_FOUND -- the answer an absent id gets -- to
// anyone else.
func (s *Server) visibleEquityOrder(ctx context.Context, oid equities.OrderID, admit func(context.Context, string) error) (EquityOrderView, error) {
	v, err := s.opts.Ports.Equities.Order(ctx, oid)
	if err != nil {
		return EquityOrderView{}, err
	}
	if admit(ctx, v.Order.AccountID.String()) != nil {
		// NOT_FOUND, never FORBIDDEN (F-41).
		return EquityOrderView{}, equityOrderNotFound(oid)
	}
	return v, nil
}

// ownedBy admits only the account the caller named (and owns, which
// accountScopeWrite established).
func ownedBy(account accounts.AccountID) func(context.Context, string) error {
	return func(_ context.Context, owner string) error {
		if owner != account.String() {
			return errs.New(errs.CodeNotFound, "not this account's order")
		}
		return nil
	}
}

// equityOrderNotFound matches the service's own answer for an absent order.
func equityOrderNotFound(oid equities.OrderID) error {
	return errs.New(errs.CodeNotFound, "equity order not found").WithField("order_id", oid.String())
}

// GetAdvancedEquitiesFills pages an account's fills.
func (s *Server) GetAdvancedEquitiesFills(ctx context.Context, request api.GetAdvancedEquitiesFillsRequestObject) (api.GetAdvancedEquitiesFillsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := s.opts.Ports.Equities.Fills(ctx, account, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedEquityFillPage{Items: make([]api.AdvancedEquityFill, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED}
	for _, v := range items {
		page.Items = append(page.Items, toAPIEquityFill(v))
	}
	return api.GetAdvancedEquitiesFills200JSONResponse(page), nil
}

// GetAdvancedEquitiesSettlements pages an account's settlements.
func (s *Server) GetAdvancedEquitiesSettlements(ctx context.Context, request api.GetAdvancedEquitiesSettlementsRequestObject) (api.GetAdvancedEquitiesSettlementsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	state := EquitySettlementsAll
	if request.Params.State != nil {
		switch *request.Params.State {
		case api.EquitySettlementFilterAll:
		case api.EquitySettlementFilterPending:
			state = EquitySettlementsPending
		case api.EquitySettlementFilterSettled:
			state = EquitySettlementsSettled
		default:
			return nil, validationError("state", "state must be PENDING, SETTLED or ALL")
		}
	}
	items, next, err := s.opts.Ports.Equities.Settlements(ctx, account, state, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedEquitySettlementPage{
		Items: make([]api.AdvancedEquitySettlement, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED,
	}
	for _, v := range items {
		page.Items = append(page.Items, toAPIEquitySettlement(v))
	}
	return api.GetAdvancedEquitiesSettlements200JSONResponse(page), nil
}

// GetAdvancedEquitiesCorporateActionApplications pages what corporate actions
// did to an account.
func (s *Server) GetAdvancedEquitiesCorporateActionApplications(ctx context.Context, request api.GetAdvancedEquitiesCorporateActionApplicationsRequestObject) (api.GetAdvancedEquitiesCorporateActionApplicationsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	items, next, err := s.opts.Ports.Equities.Applications(ctx, account, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedEquityApplicationPage{
		Items: make([]api.AdvancedEquityApplication, 0, len(items)), NextCursor: nextCursor(next), ValueDomain: api.SIMULATED,
	}
	for _, v := range items {
		page.Items = append(page.Items, toAPIEquityApplication(v))
	}
	return api.GetAdvancedEquitiesCorporateActionApplications200JSONResponse(page), nil
}

// --- the workers' steps (operator) ----------------------------------------------

// PostAdvancedEquitiesSettlementRuns runs due T+1 settlements now.
func (s *Server) PostAdvancedEquitiesSettlementRuns(ctx context.Context, request api.PostAdvancedEquitiesSettlementRunsRequestObject) (api.PostAdvancedEquitiesSettlementRunsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	limit := 0
	if request.Body.Limit != nil {
		limit = *request.Body.Limit
		if limit < 1 || limit > 500 {
			return nil, validationError("limit", "limit must be within 1..500")
		}
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquitySettlementRun, commandMeta, error) {
			run, rerr := s.opts.Ports.Equities.RunSettlements(ctx, limit)
			if rerr != nil {
				return api.AdvancedEquitySettlementRun{}, commandMeta{}, rerr
			}
			out := api.AdvancedEquitySettlementRun{
				AsOf: run.At, TradeDate: run.TradeDate.String(), Batches: run.Batches,
				Settled: make([]api.AdvancedEquitySettlement, 0, len(run.Settled)), ValueDomain: api.SIMULATED,
			}
			for _, v := range run.Settled {
				out.Settled = append(out.Settled, toAPIEquitySettlement(v))
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "equity_settlement_run"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesSettlementRuns200JSONResponse(res.Value), nil
}

// PostAdvancedEquitiesVenueRuns runs the venue worker's step once.
func (s *Server) PostAdvancedEquitiesVenueRuns(ctx context.Context, request api.PostAdvancedEquitiesVenueRunsRequestObject) (api.PostAdvancedEquitiesVenueRunsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	limit := 0
	if request.Body.Limit != nil {
		limit = *request.Body.Limit
		if limit < 1 || limit > 500 {
			return nil, validationError("limit", "limit must be within 1..500")
		}
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityVenueRun, commandMeta, error) {
			run, rerr := s.opts.Ports.Equities.RunVenue(ctx, limit)
			if rerr != nil {
				return api.AdvancedEquityVenueRun{}, commandMeta{}, rerr
			}
			out := api.AdvancedEquityVenueRun{AsOf: run.At, Items: make([]api.AdvancedEquityVenueRunItem, 0, len(run.Items)), ValueDomain: api.SIMULATED}
			for _, it := range run.Items {
				out.Items = append(out.Items, toAPIEquityVenueRunItem(it))
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "equity_venue_run"}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesVenueRuns200JSONResponse(res.Value), nil
}

// --- corporate actions ------------------------------------------------------------

// GetAdvancedEquitiesCorporateActions pages corporate actions.
func (s *Server) GetAdvancedEquitiesCorporateActions(ctx context.Context, request api.GetAdvancedEquitiesCorporateActionsRequestObject) (api.GetAdvancedEquitiesCorporateActionsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	var scope *instruments.InstrumentID
	if request.Params.InstrumentId != nil {
		iid, err := instruments.ParseInstrumentID(request.Params.InstrumentId.String())
		if err != nil || iid.IsZero() {
			return nil, validationError("instrument_id", "instrument_id must be a canonical UUID")
		}
		scope = &iid
	}
	items, next, err := s.opts.Ports.Equities.CorporateActions(ctx, scope, deref(request.Params.Cursor),
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.AdvancedEquityCorporateActionPage{Items: make([]api.AdvancedEquityCorporateAction, 0, len(items)), NextCursor: nextCursor(next)}
	for _, a := range items {
		page.Items = append(page.Items, toAPIEquityAction(a))
	}
	return api.GetAdvancedEquitiesCorporateActions200JSONResponse(page), nil
}

// GetAdvancedEquitiesCorporateActionsActionId returns one corporate action.
func (s *Server) GetAdvancedEquitiesCorporateActionsActionId(ctx context.Context, request api.GetAdvancedEquitiesCorporateActionsActionIdRequestObject) (api.GetAdvancedEquitiesCorporateActionsActionIdResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	aid, err := equityActionID(request.ActionId)
	if err != nil {
		return nil, err
	}
	a, err := s.opts.Ports.Equities.CorporateAction(ctx, aid)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedEquitiesCorporateActionsActionId200JSONResponse(toAPIEquityAction(a)), nil
}

// PostAdvancedEquitiesCorporateActions announces a corporate action
// (operator, step-up).
func (s *Server) PostAdvancedEquitiesCorporateActions(ctx context.Context, request api.PostAdvancedEquitiesCorporateActionsRequestObject) (api.PostAdvancedEquitiesCorporateActionsResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	spec, err := equityActionSpec(*request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityCorporateAction, commandMeta, error) {
			a, aerr := s.opts.Ports.Equities.AnnounceCorporateAction(ctx, spec)
			if aerr != nil {
				return api.AdvancedEquityCorporateAction{}, commandMeta{}, aerr
			}
			return toAPIEquityAction(a), commandMeta{
				Status: http.StatusCreated, ResourceType: "equity_corporate_action", ResourceID: a.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostAdvancedEquitiesCorporateActions200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedEquitiesCorporateActions201JSONResponse(res.Value), nil
}

// PostAdvancedEquitiesCorporateActionsActionIdCancel withdraws an announced
// action (operator, step-up).
func (s *Server) PostAdvancedEquitiesCorporateActionsActionIdCancel(ctx context.Context, request api.PostAdvancedEquitiesCorporateActionsActionIdCancelRequestObject) (api.PostAdvancedEquitiesCorporateActionsActionIdCancelResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	aid, err := equityActionID(request.ActionId)
	if err != nil {
		return nil, err
	}
	reason := request.Body.Reason
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityCorporateAction, commandMeta, error) {
			a, cerr := s.opts.Ports.Equities.CancelCorporateAction(ctx, aid, reason)
			if cerr != nil {
				return api.AdvancedEquityCorporateAction{}, commandMeta{}, cerr
			}
			return toAPIEquityAction(a), commandMeta{Status: http.StatusOK, ResourceType: "equity_corporate_action", ResourceID: aid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesCorporateActionsActionIdCancel200JSONResponse(res.Value), nil
}

// PostAdvancedEquitiesCorporateActionsActionIdApply applies an announced
// action on or after the day it takes effect (operator, step-up).
func (s *Server) PostAdvancedEquitiesCorporateActionsActionIdApply(ctx context.Context, request api.PostAdvancedEquitiesCorporateActionsActionIdApplyRequestObject) (api.PostAdvancedEquitiesCorporateActionsActionIdApplyResponseObject, error) {
	if s.opts.Ports.Equities == nil {
		return nil, errNotWired("equities")
	}
	aid, err := equityActionID(request.ActionId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.AdvancedEquityCorporateActionResult, commandMeta, error) {
			v, aerr := s.opts.Ports.Equities.ApplyCorporateAction(ctx, aid)
			if aerr != nil {
				return api.AdvancedEquityCorporateActionResult{}, commandMeta{}, aerr
			}
			out := api.AdvancedEquityCorporateActionResult{
				Action: toAPIEquityAction(v.Result.Action), Replayed: v.Result.Replayed,
				Applications: make([]api.AdvancedEquityApplication, 0, len(v.Applications)), ValueDomain: api.SIMULATED,
			}
			for _, app := range v.Applications {
				out.Applications = append(out.Applications, toAPIEquityApplication(app))
			}
			if v.Result.TermsVersion != nil {
				tv := v.Result.TermsVersion.Version
				out.TermsVersion = &tv
			}
			if v.Result.Instrument != nil {
				out.InstrumentStatus = strPtr(string(v.Result.Instrument.Status))
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "equity_corporate_action", ResourceID: aid.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedEquitiesCorporateActionsActionIdApply200JSONResponse(res.Value), nil
}

// --- parsing ------------------------------------------------------------------------

func equityOrderID(u api.UUID) (equities.OrderID, error) {
	oid, err := equities.ParseOrderID(u.String())
	if err != nil || oid.IsZero() {
		return equities.OrderID{}, validationError("orderId", "orderId must be a canonical UUID")
	}
	return oid, nil
}

func equityActionID(u api.UUID) (equities.CorporateActionID, error) {
	aid, err := equities.ParseCorporateActionID(u.String())
	if err != nil || aid.IsZero() {
		return equities.CorporateActionID{}, validationError("actionId", "actionId must be a canonical UUID")
	}
	return aid, nil
}

func equityDate(s, field string) (equities.Date, error) {
	d, err := equities.ParseDate(s)
	if err != nil {
		return equities.Date{}, validationError(field, field+" must be a calendar day, YYYY-MM-DD")
	}
	return d, nil
}

func equityOptionalDate(s *string, field string) (equities.Date, error) {
	if s == nil {
		return equities.Date{}, nil
	}
	return equityDate(*s, field)
}

// equityDecimal is the contract's exact decimal: digits with at most one
// point, no sign, no exponent.
var equityDecimal = regexp.MustCompile(`^([0-9]+)(?:\.([0-9]+))?$`)

// equityPricePoint reads an exact decimal as mantissa x 10^-scale, without
// rounding: every digit sent is kept, and a scale the instrument cannot price
// is the service's to refuse.
func equityPricePoint(s, field string) (equities.PricePoint, error) {
	m := equityDecimal.FindStringSubmatch(s)
	if m == nil || len(m[2]) > 18 {
		return equities.PricePoint{}, validationError(field, field+" must be an exact decimal with at most 18 fraction digits, such as 150.25")
	}
	mantissa, err := money.ParseQuantity(m[1] + m[2])
	if err != nil {
		return equities.PricePoint{}, validationError(field, field+" must be an exact decimal, such as 150.25")
	}
	return equities.PricePoint{Mantissa: mantissa, Scale: int32(len(m[2]))}, nil
}

func equityOptionalPricePoint(s *string, field string) (*equities.PricePoint, error) {
	if s == nil {
		return nil, nil
	}
	p, err := equityPricePoint(*s, field)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// equityActionSpec reads an announcement. Which fields a type carries is the
// service's rule; this only parses.
func equityActionSpec(b api.AdvancedEquityCorporateActionRequest) (equities.CorporateActionSpec, error) {
	iid, err := instruments.ParseInstrumentID(b.InstrumentId.String())
	if err != nil || iid.IsZero() {
		return equities.CorporateActionSpec{}, validationError("instrument_id", "instrument_id must be a canonical UUID")
	}
	spec := equities.CorporateActionSpec{
		InstrumentID: iid, Type: equities.CorporateActionType(b.Type), NewSymbol: deref(b.NewSymbol), Reason: b.Reason,
	}
	if b.Rounding != nil {
		spec.Rounding = equities.CorporateRounding(*b.Rounding)
	}
	if b.Ratio != nil {
		spec.Ratio = &equities.Ratio{Numerator: b.Ratio.Numerator, Denominator: b.Ratio.Denominator}
	}
	if spec.CashPerShare, err = equityOptionalPricePoint(b.DividendPerShare, "dividend_per_share"); err != nil {
		return equities.CorporateActionSpec{}, err
	}
	if spec.CashInLieuPrice, err = equityOptionalPricePoint(b.InLieuPrice, "in_lieu_price"); err != nil {
		return equities.CorporateActionSpec{}, err
	}
	for _, d := range []struct {
		src   *string
		field string
		dst   *equities.Date
	}{{b.EffectiveDate, "effective_date", &spec.EffectiveDate}, {b.ExDate, "ex_date", &spec.ExDate},
		{b.RecordDate, "record_date", &spec.RecordDate}, {b.PayDate, "pay_date", &spec.PayDate},
		{b.FinalDate, "final_date", &spec.FinalDate}} {
		if *d.dst, err = equityOptionalDate(d.src, d.field); err != nil {
			return equities.CorporateActionSpec{}, err
		}
	}
	return spec, nil
}

// --- rendering -----------------------------------------------------------------------

// equityPrice renders a price mantissa at its scale, exactly.
func equityPrice(mantissa money.Quantity, scale int32) string {
	return mantissa.ToDecimalString(uint8(scale)) // #nosec G115 -- every equity price scale is CHECKed 0..18
}

func equityPricePtr(mantissa *money.Quantity, scale int32) *string {
	if mantissa == nil {
		return nil
	}
	s := equityPrice(*mantissa, scale)
	return &s
}

func equityPricePointPtr(p *equities.PricePoint) *string {
	if p == nil {
		return nil
	}
	s := equityPrice(p.Mantissa, p.Scale)
	return &s
}

func equityDatePtr(d equities.Date) *string {
	if d.IsZero() {
		return nil
	}
	s := d.String()
	return &s
}

func toAPIEquityMark(m equities.Mark) api.AdvancedEquityMark {
	return api.AdvancedEquityMark{Price: equityPrice(m.Mantissa, m.Scale), Source: m.Source, ObservedAt: m.ObservedAt.UTC()}
}

func toAPIEquitySession(v EquitySessionView) api.AdvancedEquitySessionState {
	return api.AdvancedEquitySessionState{
		Calendar: v.Calendar, Timezone: v.Timezone, AsOf: v.At.UTC(), Session: api.AdvancedEquitySession(v.Session),
		TradeDate: v.TradeDate.String(), TradingDay: v.TradingDay, HolidayName: strPtr(v.HolidayName),
		NextTradingDay: v.NextTradingDay.String(), SettlementDate: equityDatePtr(v.SettlementDate),
	}
}

func toAPIEquityCalendar(v EquityCalendarView) api.AdvancedEquityCalendar {
	out := api.AdvancedEquityCalendar{
		State: toAPIEquitySession(v.State), SettlementCycleDays: v.SettlementCycleDays,
		Holidays: make([]api.AdvancedEquityHoliday, 0, len(v.Holidays)),
	}
	for _, h := range v.Holidays {
		out.Holidays = append(out.Holidays, api.AdvancedEquityHoliday{Date: h.Date.String(), Name: h.Name})
	}
	return out
}

func toAPIEquityListing(v EquityListingView) api.AdvancedEquityListing {
	l := v.Listing
	t := l.Terms.Terms
	out := api.AdvancedEquityListing{
		InstrumentId: toUUID(l.Instrument.ID), Kind: api.AdvancedEquityKind(l.Instrument.Type), Symbol: t.Symbol,
		ExchangeMic: t.ExchangeMIC, Name: l.ShareAsset.Name, Status: string(l.Instrument.Status), TermsVersion: l.Terms.Version,
		TermsHash: hex.EncodeToString(l.Terms.ContentHash), Venue: t.VenueCode, ShareAssetId: toUUID(l.ShareAsset.ID),
		ShareSymbol: l.ShareAsset.Symbol, ShareDecimals: int(l.ShareAsset.Decimals), SettlementAssetId: toUUID(l.CashAsset.ID),
		SettlementSymbol: l.CashAsset.Symbol, SettlementDecimals: int(l.CashAsset.Decimals), PriceScale: int(t.PriceScale),
		TickPrice: equityPrice(t.TickSize, t.PriceScale), LotSize: t.LotSize.String(), MinQuantity: t.MinQuantity.String(),
		OracleMaxAgeMs: t.OracleMaxAgeMS, TradingCalendar: t.TradingCalendar, ValueDomain: api.SIMULATED,
	}
	if v.Mark != nil {
		m := toAPIEquityMark(*v.Mark)
		out.Mark = &m
	}
	return out
}

func toAPIEquitySettlement(v EquitySettlementView) api.AdvancedEquitySettlement {
	st := v.Settlement
	out := api.AdvancedEquitySettlement{
		Id: toUUID(st.ID), FillId: toUUID(st.FillID), OrderId: toUUID(st.OrderID), AccountId: toUUID(st.AccountID),
		InstrumentId: toUUID(st.InstrumentID), AssetId: toUUID(st.AssetID), AssetSymbol: v.AssetSymbol,
		AssetDecimals: int(v.AssetDecimals), Quantity: st.Quantity.String(), TradeDate: st.TradeDate.String(),
		DueDate: st.DueDate.String(), State: api.EquitySettlementPending, SettledAt: timePtr(st.SettledAt), ValueDomain: api.SIMULATED,
	}
	if st.Settled() {
		out.State = api.EquitySettlementSettled
	}
	if st.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*st.JournalTransactionID)
	}
	return out
}

func toAPIEquityFill(v EquityFillView) api.AdvancedEquityFill {
	f, as := v.Fill, v.Assets
	out := api.AdvancedEquityFill{
		Id: toUUID(f.ID), OrderId: toUUID(f.OrderID), AccountId: toUUID(f.AccountID), InstrumentId: toUUID(f.InstrumentID),
		Side: api.AdvancedEquitySide(f.Side), Quantity: f.Quantity.String(), ShareDecimals: int(as.ShareDecimals),
		Price: equityPrice(f.Price, f.PriceScale), Mark: toAPIEquityMark(f.Mark), SlippageBps: int(f.SlippageBPS),
		Venue: f.Venue, VenueFillId: f.VenueFillID, ExecutedAt: f.ExecutedAt.UTC(), TradeDate: f.TradeDate.String(),
		SettlementDate: f.SettlementDate.String(), Notional: f.Charges.Notional.String(), Spread: f.Charges.Spread.String(),
		PlatformFees: f.Charges.PlatformFees.String(), VenueFees: f.Charges.VenueFees.String(), Total: f.Charges.Total.String(),
		SettlementAssetId: toUUID(as.SettlementAssetID), SettlementSymbol: as.SettlementSymbol,
		SettlementDecimals: int(as.SettlementDecimals), FeeScheduleVersion: f.FeeScheduleVersion,
		FeeScheduleHash: f.FeeScheduleHash, FeeDisclosureHash: f.FeeDisclosureHash,
		JournalTransactionId: toUUID(f.JournalTransactionID), EffectId: toUUID(f.EffectID), ValueDomain: api.SIMULATED,
	}
	if v.Settlement != nil {
		st := toAPIEquitySettlement(*v.Settlement)
		out.Settlement = &st
	}
	return out
}

func toAPIEquityOrder(v EquityOrderView) api.AdvancedEquityOrder {
	o := v.Order
	out := api.AdvancedEquityOrder{
		Id: toUUID(o.ID), AccountId: toUUID(o.AccountID), InstrumentId: toUUID(o.InstrumentID), IntentId: toUUID(o.IntentID),
		Symbol: o.Symbol, ExchangeMic: o.ExchangeMIC, ShareAssetId: toUUID(o.ShareAssetID), SettlementAssetId: toUUID(o.CashAssetID),
		Side: api.AdvancedEquitySide(o.Side), OrderType: api.AdvancedEquityOrderType(o.Type), Mode: api.AdvancedEquityMode(o.Mode),
		Quantity: o.Quantity.String(), ShareDecimals: int(o.QuantityDecimals), PriceScale: int(o.PriceScale),
		TickPrice: equityPrice(o.TickSize, o.PriceScale), LimitPrice: equityPricePtr(o.LimitPrice, o.PriceScale),
		ProtectionPrice: equityPricePtr(o.ProtectionPrice, o.PriceScale), ExtendedHours: o.ExtendedHours,
		Session: api.AdvancedEquitySession(o.Session), TradeDate: o.TradeDate.String(), ExpiresAt: o.ExpiresAt.UTC(),
		Status: api.AdvancedEquityOrderStatus(o.Status), RejectionCode: strPtr(string(o.RejectionCode)),
		RejectionReasons: append([]string{}, o.RejectionReasons...), ReservedQuantity: qtyPtr(o.ReservedQuantity),
		Venue: o.Broker, VenueOrderId: strPtr(o.BrokerOrderID), CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(),
		ValueDomain: api.SIMULATED,
	}
	if o.ReservationID != nil {
		out.ReservationId = uuidPtr(*o.ReservationID)
	}
	if o.RiskDecisionID != nil {
		out.RiskDecisionId = uuidPtr(*o.RiskDecisionID)
	}
	if v.Fill != nil {
		f := toAPIEquityFill(*v.Fill)
		out.Fill = &f
	}
	return out
}

// toAPIEquityPlacement says what placing the order did. A failure's detail is
// shown only when it is a coded refusal; anything else is reported by its
// code alone, as every INTERNAL detail is.
func toAPIEquityPlacement(p EquityPlacement) api.AdvancedEquityPlacement {
	switch {
	case p.Earlier:
		return api.AdvancedEquityPlacement{
			Status: api.EquityPlacementPlaced,
			Detail: "the order already had the venue's answer before this request; nothing was sent again",
		}
	case p.Placed && p.Report != nil:
		vs := api.AdvancedEquityVenueStatus(p.Report.Status)
		return api.AdvancedEquityPlacement{
			Status: api.EquityPlacementPlaced, VenueStatus: &vs, VenueReason: strPtr(p.Report.Reason),
			Detail: "placed at the simulated venue " + p.Report.Broker + "; its answer is recorded on the order",
		}
	}
	code, detail := equityFailure(p.Err)
	return api.AdvancedEquityPlacement{
		Status: api.EquityPlacementNotPlaced, ErrorCode: &code,
		Detail: "no answer of the venue was recorded (" + detail + "); the order stays ACCEPTED with its submission " +
			"PENDING, and an operator's venue run places it",
	}
}

// equityFailure is a failure's code and, for a coded refusal, its detail.
func equityFailure(err error) (string, string) {
	if e, ok := errs.As(err); ok && e.Code != errs.CodeInternal {
		return string(e.Code), e.Detail
	}
	return string(errs.CodeInternal), "internal error"
}

func toAPIEquityVenueRunItem(it EquityVenueRunItem) api.AdvancedEquityVenueRunItem {
	out := api.AdvancedEquityVenueRunItem{
		OrderId: toUUID(it.OrderID), Action: api.AdvancedEquityVenueAction(it.Action),
		StatusBefore: api.AdvancedEquityOrderStatus(it.Before),
	}
	if it.After != "" {
		after := api.AdvancedEquityOrderStatus(it.After)
		out.StatusAfter = &after
	}
	if it.Report != nil {
		vs := api.AdvancedEquityVenueStatus(it.Report.Status)
		out.VenueStatus, out.VenueReason = &vs, strPtr(it.Report.Reason)
	}
	if it.Err != nil {
		code, detail := equityFailure(it.Err)
		out.ErrorCode, out.Detail = &code, &detail
	}
	return out
}

func toAPIEquityAction(a equities.CorporateAction) api.AdvancedEquityCorporateAction {
	out := api.AdvancedEquityCorporateAction{
		Id: toUUID(a.ID), InstrumentId: toUUID(a.InstrumentID), Type: api.AdvancedEquityActionType(a.Type),
		Status: api.AdvancedEquityActionStatus(a.Status), EffectiveDate: a.EffectiveDate.String(),
		ShareAssetId: toUUID(a.ShareAssetID), SettlementAssetId: toUUID(a.CashAssetID),
		DividendPerShare: equityPricePointPtr(a.CashPerShare), InLieuPrice: equityPricePointPtr(a.CashInLieuPrice),
		ExDate: equityDatePtr(a.ExDate), RecordDate: equityDatePtr(a.RecordDate), PayDate: equityDatePtr(a.PayDate),
		FinalDate: equityDatePtr(a.FinalDate), NewSymbol: strPtr(a.NewSymbol), Rounding: api.AdvancedEquityRounding(a.Rounding),
		Reason: a.Reason, ContentHash: hex.EncodeToString(a.ContentHash), AnnouncedByActorType: string(a.AnnouncedBy.Type),
		AnnouncedByActorId: a.AnnouncedBy.ID, AppliedAt: timePtr(a.AppliedAt), CreatedAt: a.CreatedAt.UTC(),
	}
	if a.Ratio != nil {
		out.Ratio = &api.AdvancedEquityRatio{Numerator: a.Ratio.Numerator, Denominator: a.Ratio.Denominator}
	}
	return out
}

func toAPIEquityApplication(v EquityApplicationView) api.AdvancedEquityApplication {
	app, as := v.Application, v.Assets
	at := api.AdvancedEquityActionType(v.ActionType)
	out := api.AdvancedEquityApplication{
		Id: toUUID(app.ID), ActionId: toUUID(app.ActionID), ActionType: &at, InstrumentId: uuidPtr(v.InstrumentID),
		AccountId: toUUID(app.AccountID), EntitledQuantity: app.EntitledQuantity.String(), SharesDelta: app.SharesDelta.String(),
		SettlementAmount: app.CashAmount.String(), ExactAmountNumerator: app.ExactCashNumerator.String(),
		ExactAmountDenominator: app.ExactCashDenominator.String(), Rounding: api.AdvancedEquityRounding(app.Rounding),
		ShareSymbol: as.ShareSymbol, ShareDecimals: int(as.ShareDecimals), SettlementSymbol: as.SettlementSymbol,
		SettlementDecimals: int(as.SettlementDecimals), CreatedAt: app.CreatedAt.UTC(), ValueDomain: api.SIMULATED,
	}
	if app.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*app.JournalTransactionID)
	}
	if app.EffectID != nil {
		out.EffectId = uuidPtr(*app.EffectID)
	}
	return out
}
