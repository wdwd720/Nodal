package httpapi

import (
	"context"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/orders"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/treasury"
)

// The house book routes (internal/treasury). Handlers translate shapes only:
// the treasury decides every refusal, records it, and its code and reason
// codes reach the wire unchanged. Every route under /advanced/treasury/books
// is an operator's (sandbox:operate); the two customer reads are the crossing
// disclosures and the house trades against the caller's own account (authz.go).

const treasuryFeature = "the house book"

// --- reads of house books --------------------------------------------------------

// GetAdvancedTreasuryBooks pages the registered house books, newest first.
func (s *Server) GetAdvancedTreasuryBooks(ctx context.Context, request api.GetAdvancedTreasuryBooksRequestObject) (api.GetAdvancedTreasuryBooksResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	page, err := s.opts.Ports.Treasury.Books(ctx, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryBookPage{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryBook, 0, len(page.Items)), NextCursor: nextCursor(page.NextCursor)}
	for _, v := range page.Items {
		out.Items = append(out.Items, toAPITreasuryBook(v))
	}
	return api.GetAdvancedTreasuryBooks200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookId returns one house book with the signing
// policy and risk policy in force.
func (s *Server) GetAdvancedTreasuryBooksHouseBookId(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Treasury.Book(ctx, house)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedTreasuryBooksHouseBookId200JSONResponse(toAPITreasuryBookDetail(v)), nil
}

// GetAdvancedTreasuryBooksHouseBookIdReport returns the book's positions and
// PnL at fresh marks with every limit in use.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdReport(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdReportRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdReportResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Treasury.Report(ctx, house)
	if err != nil {
		return nil, err
	}
	out, err := toAPITreasuryReport(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdReport200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdBalances returns the book's inventory,
// reservations and venue flows, asset by asset.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdBalances(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdBalancesRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdBalancesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.Treasury.Balances(ctx, house)
	if err != nil {
		return nil, err
	}
	out := api.TreasuryBalanceList{ValueDomain: api.SIMULATED, HouseAccountId: toUUID(v.House), Items: make([]api.TreasuryBalance, 0, len(v.Items))}
	for _, b := range v.Items {
		ref, err := treasuryRef(v.Assets, b.Asset)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, api.TreasuryBalance{
			Asset: ref, Inventory: b.Inventory.String(), Reserved: b.Reserved.String(), Available: b.Available.String(),
			Funded:       b.Funded.String(),
			InternalBook: api.TreasuryVenueFlow{Bought: b.InternalBook.Bought.String(), Sold: b.InternalBook.Sold.String()},
			PaperVenue:   api.TreasuryVenueFlow{Bought: b.PaperVenue.Bought.String(), Sold: b.PaperVenue.Sold.String()},
			WorkingBuys:  b.WorkingBuys.String(), WorkingSells: b.WorkingSells.String(),
		})
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdBalances200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdReconciliation proves the position
// register against the ledger; an empty list means the books agree.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdReconciliation(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdReconciliationRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdReconciliationResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	found, err := s.opts.Ports.Treasury.Reconcile(ctx, house)
	if err != nil {
		return nil, err
	}
	out := api.TreasuryReconciliation{ValueDomain: api.SIMULATED, HouseAccountId: toUUID(house), Agrees: len(found) == 0,
		Discrepancies: make([]api.TreasuryDiscrepancy, 0, len(found))}
	for _, d := range found {
		out.Discrepancies = append(out.Discrepancies, api.TreasuryDiscrepancy{AssetId: toUUID(d.Asset), What: d.What,
			Expected: d.Expected.String(), Actual: d.Actual.String()})
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdReconciliation200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdEvents pages the position register,
// newest first.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdEvents(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdEventsRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdEventsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Treasury.Events(ctx, house, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryPositionEventPage{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryPositionEvent, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, e := range page.Items {
		ref, err := treasuryRef(page.Assets, e.Asset)
		if err != nil {
			return nil, err
		}
		m := e.Movement
		item := api.TreasuryPositionEvent{
			Id: toUUID(e.ID), Seq: e.Seq, Kind: api.TreasuryEventKind(e.Kind), Direction: api.TreasuryDirection(m.Direction), Asset: ref,
			Quantity: m.Quantity.String(), Value: m.Value.String(), Fees: m.Fees.String(), QtyBefore: m.Before.Quantity.String(),
			QtyAfter: m.After.Quantity.String(), CostBefore: m.Before.Cost.String(), CostAfter: m.After.Cost.String(),
			CostRemoved: m.CostRemoved.String(), RealizedPnl: m.Realized.String(), JournalTransactionId: toUUID(e.JournalTransactionID),
			OccurredAt: e.OccurredAt.UTC(),
		}
		if e.BookTradeID != nil {
			item.BookTradeId = uuidPtr(*e.BookTradeID)
		}
		if e.PaperFillID != nil {
			item.PaperFillId = uuidPtr(*e.PaperFillID)
		}
		if e.FundingID != nil {
			item.FundingId = uuidPtr(*e.FundingID)
		}
		if e.InstrumentID != nil {
			item.InstrumentId = uuidPtr(*e.InstrumentID)
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdEvents200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdActions lists the latest house actions.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdActions(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdActionsRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdActionsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	items, err := s.opts.Ports.Treasury.Actions(ctx, house, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryActionList{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryAction, 0, len(items))}
	for _, a := range items {
		out.Items = append(out.Items, toAPITreasuryAction(a))
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdActions200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdPaperFills pages the book's paper-venue
// fills with their verified disclosures.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdPaperFills(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdPaperFillsRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdPaperFillsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Treasury.PaperFills(ctx, house, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryPaperFillPage{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryPaperFill, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, f := range page.Items {
		item, err := toAPITreasuryPaperFill(f, page.Assets)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdPaperFills200JSONResponse(out), nil
}

// GetAdvancedTreasuryBooksHouseBookIdRiskDecisions pages the book's Risk
// Kernel V2 decisions with their limit evaluations.
func (s *Server) GetAdvancedTreasuryBooksHouseBookIdRiskDecisions(ctx context.Context, request api.GetAdvancedTreasuryBooksHouseBookIdRiskDecisionsRequestObject) (api.GetAdvancedTreasuryBooksHouseBookIdRiskDecisionsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Treasury.RiskDecisions(ctx, house, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryRiskDecisionPage{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryRiskDecisionRecord, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, d := range page.Items {
		item := api.TreasuryRiskDecisionRecord{
			Id: toUUID(d.ID), HouseAccountId: toUUID(d.HouseAccountID), IntentKind: d.IntentKind, AssetId: toUUID(d.Asset),
			RiskIncreasing: d.RiskIncreasing, Stage: d.Stage, Verdict: api.TreasuryVerdict(d.Verdict),
			ReasonCodes: treasuryStrings(d.ReasonCodes), TreasuryCodes: treasuryStrings(d.TreasuryCodes),
			Limits: toAPITreasuryLimitEvaluation(d.Limits), InputHash: d.InputHash, DecisionHash: d.DecisionHash,
			RiskPolicyVersion: d.RiskPolicyVersion, EvaluatorVersion: d.EvaluatorVersion, TreasuryPolicyVersion: d.TreasuryPolicyVersion,
			TreasuryPolicyHash: d.TreasuryPolicyHash, EvaluatedAt: d.EvaluatedAt.UTC(),
		}
		if d.Instrument != nil {
			item.InstrumentId = uuidPtr(*d.Instrument)
		}
		out.Items = append(out.Items, item)
	}
	return api.GetAdvancedTreasuryBooksHouseBookIdRiskDecisions200JSONResponse(out), nil
}

// GetAdvancedTreasuryCrossingDisclosures lists what customers are told about
// house books trading against them on one instrument's book. It is a
// disclosure: customers read it as well as operators.
func (s *Server) GetAdvancedTreasuryCrossingDisclosures(ctx context.Context, request api.GetAdvancedTreasuryCrossingDisclosuresRequestObject) (api.GetAdvancedTreasuryCrossingDisclosuresResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	inst, err := treasuryInstrument(request.Params.InstrumentId, "instrument_id")
	if err != nil {
		return nil, err
	}
	items, err := s.opts.Ports.Treasury.CrossingDisclosures(ctx, inst)
	if err != nil {
		return nil, err
	}
	out := api.TreasuryCrossingDisclosureList{ValueDomain: api.SIMULATED, InstrumentId: toUUID(inst),
		Items: make([]api.TreasuryCrossingDisclosure, 0, len(items))}
	for _, d := range items {
		out.Items = append(out.Items, toAPITreasuryCrossingDisclosure(d))
	}
	return api.GetAdvancedTreasuryCrossingDisclosures200JSONResponse(out), nil
}

// GetAdvancedTreasuryHouseTrades pages the trades in which a house book was
// the account's counterparty: the account's owner, or an operator who may read
// any account.
func (s *Server) GetAdvancedTreasuryHouseTrades(ctx context.Context, request api.GetAdvancedTreasuryHouseTradesRequestObject) (api.GetAdvancedTreasuryHouseTradesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	page, err := s.opts.Ports.Treasury.HouseTrades(ctx, account, marginCursorParam(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.TreasuryHouseTradePage{ValueDomain: api.SIMULATED, Items: make([]api.TreasuryHouseTrade, 0, len(page.Items)),
		NextCursor: nextCursor(page.NextCursor)}
	for _, t := range page.Items {
		out.Items = append(out.Items, api.TreasuryHouseTrade{
			TradeId: toUUID(t.TradeID), BookId: toUUID(t.BookID), InstrumentId: toUUID(t.Instrument), TradeSeq: t.TradeSeq,
			HouseAccountId: toUUID(t.House), HouseSide: api.TreasurySide(t.HouseSide), AccountSide: api.TreasurySide(t.AccountSide),
			Price: toAPITreasuryPrice(t.Price), BaseAsset: toAPITreasuryAssetRef(t.Base), QuoteAsset: toAPITreasuryAssetRef(t.Quote),
			BaseQuantity: t.BaseQuantity.String(), QuoteNotional: t.QuoteNotional.String(), AccountFee: t.AccountFee.String(),
			CrossingRuleVersion: t.CrossingRuleVersion, Disclosure: strPtr(t.Disclosure),
			JournalTransactionId: toUUID(t.JournalTransaction), TradedAt: t.TradedAt.UTC(),
		})
	}
	return api.GetAdvancedTreasuryHouseTrades200JSONResponse(out), nil
}

// --- administration of a house book ----------------------------------------------

// PostAdvancedTreasuryBooks registers a house book.
func (s *Server) PostAdvancedTreasuryBooks(ctx context.Context, request api.PostAdvancedTreasuryBooksRequestObject) (api.PostAdvancedTreasuryBooksResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	strategy, err := treasuryStrategy(b.Strategy, "strategy")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(b.Name) == "" {
		return nil, validationError("name", "a house book needs a name")
	}
	var valuation *assets.AssetID
	if b.ValuationAssetId != nil {
		aid, err := treasuryAsset(*b.ValuationAssetId, "valuation_asset_id")
		if err != nil {
			return nil, err
		}
		valuation = &aid
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := TreasuryRegister{
		Name: b.Name, Strategy: strategy, ValuationAsset: valuation,
		Signing:        treasury.SigningPolicy{ServiceActorID: b.ServiceActorId, OperatorsAllowed: b.OperatorsAllowed},
		IdempotencyKey: request.Params.IdempotencyKey, Reason: b.Reason, Actor: actor,
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryBookDetail, commandMeta, error) {
			v, rerr := s.opts.Ports.Treasury.Register(ctx, req)
			if rerr != nil {
				return api.TreasuryBookDetail{}, commandMeta{}, rerr
			}
			return toAPITreasuryBookDetail(v), commandMeta{Status: http.StatusOK, ResourceType: "treasury_house_book", ResourceID: v.Book.HouseAccountID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooks200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdActorPolicy records the next signing
// policy of a house book.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdActorPolicy(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdActorPolicyRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdActorPolicyResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := TreasurySigningChange{House: house, Policy: treasury.SigningPolicy{ServiceActorID: request.Body.ServiceActorId,
		OperatorsAllowed: request.Body.OperatorsAllowed}, IdempotencyKey: request.Params.IdempotencyKey, Reason: request.Body.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasurySigningPolicy, commandMeta, error) {
			v, serr := s.opts.Ports.Treasury.SetSigningPolicy(ctx, req)
			if serr != nil {
				return api.TreasurySigningPolicy{}, commandMeta{}, serr
			}
			return toAPITreasurySigningPolicy(v), commandMeta{Status: http.StatusOK, ResourceType: "treasury_signing_policy", ResourceID: v.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdActorPolicy200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdLimits records the next risk policy
// of a house book.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdLimits(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdLimitsRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdLimitsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	policy, err := treasuryPolicyFrom(*request.Body)
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := TreasuryRiskChange{House: house, Policy: policy, IdempotencyKey: request.Params.IdempotencyKey, Reason: request.Body.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryRiskPolicy, commandMeta, error) {
			v, perr := s.opts.Ports.Treasury.SetRiskPolicy(ctx, req)
			if perr != nil {
				return api.TreasuryRiskPolicy{}, commandMeta{}, perr
			}
			return toAPITreasuryRiskPolicy(v), commandMeta{Status: http.StatusOK, ResourceType: "treasury_policy", ResourceID: v.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdLimits200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdFundings injects simulated house
// capital into a house book.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdFundings(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdFundingsRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdFundingsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	asset, err := treasuryAsset(b.AssetId, "asset_id")
	if err != nil {
		return nil, err
	}
	qty, err := treasuryQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	mode, err := treasuryMode(b.Mode)
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := treasury.FundRequest{HouseAccountID: house, Asset: asset, Quantity: qty, Mode: mode, IdempotencyKey: request.Params.IdempotencyKey,
		Reason: b.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryFunding, commandMeta, error) {
			v, ferr := s.opts.Ports.Treasury.Fund(ctx, req)
			if ferr != nil {
				return api.TreasuryFunding{}, commandMeta{}, ferr
			}
			f := v.Funding
			out := api.TreasuryFunding{
				ValueDomain: api.SIMULATED, Id: toUUID(f.ID), HouseAccountId: toUUID(f.HouseAccountID), Asset: toAPITreasuryAssetRef(v.Asset),
				Quantity: f.Quantity.String(), CostBasis: f.CostBasis.String(), EffectId: toUUID(f.EffectID),
				JournalTransactionId: toUUID(f.JournalTransactionID), EvidenceRef: f.EvidenceRef, FundedAt: f.FundedAt.UTC(),
				Action: toAPITreasuryAction(f.Action),
			}
			if f.Mark != nil {
				p := toAPITreasuryPrice(*f.Mark)
				out.Mark = &p
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "treasury_funding", ResourceID: f.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdFundings200JSONResponse(res.Value), nil
}

// --- house orders ------------------------------------------------------------------

// PostAdvancedTreasuryBooksHouseBookIdOrders places a house order on the
// internal book after every treasury control.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdOrders(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdOrdersRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdOrdersResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	inst, err := treasuryInstrument(b.InstrumentId, "instrument_id")
	if err != nil {
		return nil, err
	}
	activity, err := treasuryStrategy(b.Activity, "activity")
	if err != nil {
		return nil, err
	}
	side, err := treasurySide(b.Side, "side")
	if err != nil {
		return nil, err
	}
	typ := orders.OrderType(b.OrderType)
	if !typ.Valid() {
		return nil, validationError("order_type", "order_type must be LIMIT or MARKET")
	}
	tif := orders.TimeInForce(b.TimeInForce)
	if !tif.Valid() {
		return nil, validationError("time_in_force", "time_in_force must be GTC, IOC or FOK")
	}
	if err := treasuryDecimal(b.Price, "price"); err != nil {
		return nil, err
	}
	qty, err := treasuryQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := TreasuryOrderCommand{
		House: house, Instrument: inst, Activity: activity, Side: side, Type: typ, TimeInForce: tif, Price: b.Price, Quantity: qty,
		PostOnly: treasuryBool(b.PostOnly), ReduceOnly: treasuryBool(b.ReduceOnly), IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID: observability.CorrelationID(ctx), Actor: actor,
	}
	if b.ClientOrderId != nil {
		req.ClientOrderID = *b.ClientOrderId
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryOrderOutcome, commandMeta, error) {
			o, perr := s.opts.Ports.Treasury.PlaceOrder(ctx, req)
			if perr != nil {
				return api.TreasuryOrderOutcome{}, commandMeta{}, perr
			}
			return toAPITreasuryOrderOutcome(o), commandMeta{Status: http.StatusOK, ResourceType: "treasury_action", ResourceID: o.Action.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdOrders200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdOrdersOrderIdCancel cancels a resting
// house order.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdOrdersOrderIdCancel(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdOrdersOrderIdCancelRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdOrdersOrderIdCancelResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	oid, err := orders.ParseOrderID(request.OrderId.String())
	if err != nil || oid.IsZero() {
		return nil, validationError("orderId", "orderId must be a canonical UUID")
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := treasury.CancelRequest{HouseAccountID: house, OrderID: oid, IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID: observability.CorrelationID(ctx), Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryOrderOutcome, commandMeta, error) {
			o, cerr := s.opts.Ports.Treasury.CancelOrder(ctx, req)
			if cerr != nil {
				return api.TreasuryOrderOutcome{}, commandMeta{}, cerr
			}
			return toAPITreasuryOrderOutcome(o), commandMeta{Status: http.StatusOK, ResourceType: "treasury_action", ResourceID: o.Action.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdOrdersOrderIdCancel200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdQuotes places a two-sided quote.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdQuotes(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdQuotesRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdQuotesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	inst, err := treasuryInstrument(b.InstrumentId, "instrument_id")
	if err != nil {
		return nil, err
	}
	activity, err := treasuryStrategy(b.Activity, "activity")
	if err != nil {
		return nil, err
	}
	if err := treasuryDecimal(b.BidPrice, "bid_price"); err != nil {
		return nil, err
	}
	if err := treasuryDecimal(b.AskPrice, "ask_price"); err != nil {
		return nil, err
	}
	bidQty, err := treasuryQuantity(b.BidQuantity, "bid_quantity")
	if err != nil {
		return nil, err
	}
	askQty, err := treasuryQuantity(b.AskQuantity, "ask_quantity")
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := TreasuryQuoteCommand{House: house, Instrument: inst, Activity: activity, BidPrice: b.BidPrice, BidQuantity: bidQty,
		AskPrice: b.AskPrice, AskQuantity: askQty, IdempotencyKey: request.Params.IdempotencyKey, CorrelationID: observability.CorrelationID(ctx),
		Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryQuoteOutcome, commandMeta, error) {
			o, qerr := s.opts.Ports.Treasury.Quote(ctx, req)
			if qerr != nil {
				return api.TreasuryQuoteOutcome{}, commandMeta{}, qerr
			}
			out := api.TreasuryQuoteOutcome{ValueDomain: api.SIMULATED, Bid: toAPITreasuryOrderOutcome(o.Bid), Ask: toAPITreasuryOrderOutcome(o.Ask)}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "treasury_action", ResourceID: o.Bid.Action.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdQuotes200JSONResponse(res.Value), nil
}

// --- the paper venue ----------------------------------------------------------------

// treasuryPaperCommand runs one paper-venue command under the idempotency
// contract and renders its outcome.
func treasuryPaperCommand(ctx context.Context, s *Server, key string, run func(ctx context.Context) (TreasuryPaperView, error)) (api.TreasuryPaperOutcome, error) {
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.TreasuryPaperOutcome, commandMeta, error) {
		v, perr := run(ctx)
		if perr != nil {
			return api.TreasuryPaperOutcome{}, commandMeta{}, perr
		}
		out, cerr := toAPITreasuryPaperOutcome(v)
		if cerr != nil {
			return api.TreasuryPaperOutcome{}, commandMeta{}, cerr
		}
		return out, commandMeta{Status: http.StatusOK, ResourceType: "treasury_action", ResourceID: v.Outcome.Action.ID.String()}, nil
	})
	if err != nil {
		return api.TreasuryPaperOutcome{}, err
	}
	return res.Value, nil
}

// PostAdvancedTreasuryBooksHouseBookIdHedges hedges on the paper venue.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdHedges(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdHedgesRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdHedgesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	req, err := treasuryPaperRequest(ctx, house, econ.HouseHedging, b.AssetId, b.Side, b.Quantity, b.Mode, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	out, err := treasuryPaperCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (TreasuryPaperView, error) {
		return s.opts.Ports.Treasury.Hedge(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdHedges200JSONResponse(out), nil
}

// PostAdvancedTreasuryBooksHouseBookIdPaperFills executes on the paper venue
// under a named activity.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdPaperFills(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdPaperFillsRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdPaperFillsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	activity, err := treasuryStrategy(b.Activity, "activity")
	if err != nil {
		return nil, err
	}
	req, err := treasuryPaperRequest(ctx, house, activity, b.AssetId, b.Side, b.Quantity, b.Mode, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	out, err := treasuryPaperCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (TreasuryPaperView, error) {
		return s.opts.Ports.Treasury.ExecutePaper(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdPaperFills200JSONResponse(out), nil
}

// PostAdvancedTreasuryBooksHouseBookIdConversions converts between the
// valuation asset and one other sandbox asset.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdConversions(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdConversionsRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdConversionsResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	from, err := treasuryAsset(b.FromAssetId, "from_asset_id")
	if err != nil {
		return nil, err
	}
	to, err := treasuryAsset(b.ToAssetId, "to_asset_id")
	if err != nil {
		return nil, err
	}
	qty, err := treasuryQuantity(b.Quantity, "quantity")
	if err != nil {
		return nil, err
	}
	mode, err := treasuryMode(b.Mode)
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := treasury.ConvertRequest{HouseAccountID: house, From: from, To: to, Quantity: qty, Mode: mode,
		IdempotencyKey: request.Params.IdempotencyKey, CorrelationID: observability.CorrelationID(ctx), Actor: actor}
	out, err := treasuryPaperCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (TreasuryPaperView, error) {
		return s.opts.Ports.Treasury.Convert(ctx, req)
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdConversions200JSONResponse(out), nil
}

// PostAdvancedTreasuryBooksHouseBookIdRebalances moves the book's holding of
// an asset to a target on the paper venue.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdRebalances(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdRebalancesRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdRebalancesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	asset, err := treasuryAsset(b.AssetId, "asset_id")
	if err != nil {
		return nil, err
	}
	target, err := money.ParseQuantity(b.Target)
	if err != nil {
		return nil, validationError("target", "target must be an integer string of the asset's base units")
	}
	mode, err := treasuryMode(b.Mode)
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := treasury.RebalanceRequest{HouseAccountID: house, Asset: asset, Target: target, Mode: mode,
		IdempotencyKey: request.Params.IdempotencyKey, CorrelationID: observability.CorrelationID(ctx), Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryRebalanceOutcome, commandMeta, error) {
			v, rerr := s.opts.Ports.Treasury.Rebalance(ctx, req)
			if rerr != nil {
				return api.TreasuryRebalanceOutcome{}, commandMeta{}, rerr
			}
			out := api.TreasuryRebalanceOutcome{ValueDomain: api.SIMULATED, NoOp: v.NoOp}
			meta := commandMeta{Status: http.StatusOK, ResourceType: "treasury_house_book", ResourceID: house.String()}
			if !v.NoOp {
				p, cerr := toAPITreasuryPaperOutcome(v)
				if cerr != nil {
					return api.TreasuryRebalanceOutcome{}, commandMeta{}, cerr
				}
				out.Action, out.Fill, out.Decision = &p.Action, p.Fill, p.Decision
				meta.ResourceType, meta.ResourceID = "treasury_action", v.Outcome.Action.ID.String()
			}
			return out, meta, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdRebalances200JSONResponse(res.Value), nil
}

// --- crossing rules, sync, marking ----------------------------------------------------

// PostAdvancedTreasuryCrossingRules changes whether house books may trade
// with customers on one instrument's book, with its customer disclosure.
func (s *Server) PostAdvancedTreasuryCrossingRules(ctx context.Context, request api.PostAdvancedTreasuryCrossingRulesRequestObject) (api.PostAdvancedTreasuryCrossingRulesResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	inst, err := treasuryInstrument(b.InstrumentId, "instrument_id")
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	req := treasury.CrossingRuleRequest{InstrumentID: inst, Allow: b.Allow, Disclosure: b.Disclosure, Reason: b.Reason, Actor: actor}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasuryCrossingDisclosure, commandMeta, error) {
			d, cerr := s.opts.Ports.Treasury.SetCrossingRule(ctx, req)
			if cerr != nil {
				return api.TreasuryCrossingDisclosure{}, commandMeta{}, cerr
			}
			return toAPITreasuryCrossingDisclosure(d), commandMeta{Status: http.StatusOK, ResourceType: "treasury_crossing_disclosure", ResourceID: d.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryCrossingRules200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdSync ingests the book's unrecorded
// internal-book trades.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdSync(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdSyncRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdSyncResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TreasurySyncResult, commandMeta, error) {
			n, serr := s.opts.Ports.Treasury.Sync(ctx, house)
			if serr != nil {
				return api.TreasurySyncResult{}, commandMeta{}, serr
			}
			return api.TreasurySyncResult{ValueDomain: api.SIMULATED, HouseAccountId: toUUID(house), Ingested: n},
				commandMeta{Status: http.StatusOK, ResourceType: "treasury_house_book", ResourceID: house.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdSync200JSONResponse(res.Value), nil
}

// PostAdvancedTreasuryBooksHouseBookIdMarkToMarket persists the book's PnL
// report as a hashed snapshot.
func (s *Server) PostAdvancedTreasuryBooksHouseBookIdMarkToMarket(ctx context.Context, request api.PostAdvancedTreasuryBooksHouseBookIdMarkToMarketRequestObject) (api.PostAdvancedTreasuryBooksHouseBookIdMarkToMarketResponseObject, error) {
	if s.opts.Ports.Treasury == nil {
		return nil, errNotWired(treasuryFeature)
	}
	house, err := treasuryHouse(request.HouseBookId)
	if err != nil {
		return nil, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return nil, err
	}
	key := request.Params.IdempotencyKey
	res, err := runCommand(ctx, s, key,
		func(ctx context.Context) (api.TreasurySnapshot, commandMeta, error) {
			v, merr := s.opts.Ports.Treasury.MarkToMarket(ctx, house, key, actor)
			if merr != nil {
				return api.TreasurySnapshot{}, commandMeta{}, merr
			}
			rep, cerr := toAPITreasuryReport(v.Report)
			if cerr != nil {
				return api.TreasurySnapshot{}, commandMeta{}, cerr
			}
			out := api.TreasurySnapshot{ValueDomain: api.SIMULATED, Id: toUUID(v.Snapshot.ID), DocumentHash: v.Snapshot.DocumentHash,
				Report: rep, Action: toAPITreasuryAction(v.Snapshot.Action)}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "treasury_pnl_snapshot", ResourceID: v.Snapshot.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedTreasuryBooksHouseBookIdMarkToMarket200JSONResponse(res.Value), nil
}

// --- request helpers -----------------------------------------------------------------

// treasuryActor is the operator acting for the house book. The treasury's
// signing policy decides whether that actor may take the action.
func treasuryActor(ctx context.Context) (econ.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return econ.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

func treasuryHouse(u api.UUID) (econ.HouseAccountID, error) {
	h, err := econ.ParseHouseAccountID(u.String())
	if err != nil || h.IsZero() {
		return econ.HouseAccountID{}, validationError("houseBookId", "houseBookId must be a canonical UUID")
	}
	return h, nil
}

func treasuryAsset(u api.UUID, field string) (assets.AssetID, error) {
	a, err := assets.ParseAssetID(u.String())
	if err != nil || a.IsZero() {
		return assets.AssetID{}, validationError(field, field+" must be a canonical UUID")
	}
	return a, nil
}

func treasuryInstrument(u api.UUID, field string) (instruments.InstrumentID, error) {
	i, err := instruments.ParseInstrumentID(u.String())
	if err != nil || i.IsZero() {
		return instruments.InstrumentID{}, validationError(field, field+" must be a canonical UUID")
	}
	return i, nil
}

func treasuryQuantity(s, field string) (money.Quantity, error) {
	q, err := money.ParseQuantity(s)
	if err != nil {
		return money.Quantity{}, validationError(field, field+" must be an integer string of base units")
	}
	return q, nil
}

// treasuryDecimal checks a price is a plain decimal string; the adapter states
// it in the right asset.
func treasuryDecimal(s, field string) error {
	if _, err := money.PriceFromDecimalString(s, "validation", "validation", time.Unix(1, 0)); err != nil {
		return validationError(field, field+" must be a plain decimal string such as 100.25")
	}
	return nil
}

func treasuryStrategy(s api.TreasuryStrategy, field string) (econ.HouseStrategy, error) {
	h := econ.HouseStrategy(s)
	for _, known := range econ.AllHouseStrategies() {
		if known == h {
			return h, nil
		}
	}
	return "", validationError(field, field+" is not one of the house strategies the contract declares")
}

func treasurySide(s api.TreasurySide, field string) (orders.Side, error) {
	side := orders.Side(s)
	if !side.Valid() {
		return "", validationError(field, field+" must be BUY or SELL")
	}
	return side, nil
}

// treasuryMode reads an optional mode; absent is the service's default (PAPER).
func treasuryMode(m *api.AdvancedMode) (gates.Mode, error) {
	if m == nil {
		return "", nil
	}
	mode := gates.Mode(*m)
	if !mode.Valid() {
		return "", validationError("mode", "mode must be one of PAPER, SANDBOX, DEVNET, LIVE")
	}
	return mode, nil
}

func treasuryBool(b *bool) bool { return b != nil && *b }

func treasuryPaperRequest(ctx context.Context, house econ.HouseAccountID, activity econ.HouseStrategy, assetID api.UUID, side api.TreasurySide,
	quantity string, mode *api.AdvancedMode, key string) (treasury.PaperRequest, error) {
	asset, err := treasuryAsset(assetID, "asset_id")
	if err != nil {
		return treasury.PaperRequest{}, err
	}
	sd, err := treasurySide(side, "side")
	if err != nil {
		return treasury.PaperRequest{}, err
	}
	qty, err := treasuryQuantity(quantity, "quantity")
	if err != nil {
		return treasury.PaperRequest{}, err
	}
	m, err := treasuryMode(mode)
	if err != nil {
		return treasury.PaperRequest{}, err
	}
	actor, err := treasuryActor(ctx)
	if err != nil {
		return treasury.PaperRequest{}, err
	}
	return treasury.PaperRequest{HouseAccountID: house, Activity: activity, Asset: asset, Side: sd, Quantity: qty, Mode: m,
		IdempotencyKey: key, CorrelationID: observability.CorrelationID(ctx), Actor: actor}, nil
}

// treasuryPolicyFrom reads a risk policy request. The valuation asset is the
// book's, stated by the adapter; the treasury validates the policy.
func treasuryPolicyFrom(b api.TreasuryRiskPolicyRequest) (treasury.Policy, error) {
	limit, err := treasuryQuantity(b.DailyLossLimit, "daily_loss_limit")
	if err != nil {
		return treasury.Policy{}, err
	}
	if b.MaxMarkAgeMs < 1 {
		return treasury.Policy{}, validationError("max_mark_age_ms", "max_mark_age_ms must be positive")
	}
	p := treasury.Policy{
		Label: b.Label, DailyLossLimit: limit, MaxMarkAge: time.Duration(b.MaxMarkAgeMs) * time.Millisecond,
		PaperSpreadBPS: money.BPS(b.PaperSpreadBps), PaperFeeBPS: money.BPS(b.PaperFeeBps),
	}
	for _, l := range b.MaxInventory {
		a, err := treasuryAsset(l.AssetId, "max_inventory.asset_id")
		if err != nil {
			return treasury.Policy{}, err
		}
		q, err := treasuryQuantity(l.MaxQuantity, "max_inventory.max_quantity")
		if err != nil {
			return treasury.Policy{}, err
		}
		p.MaxInventory = append(p.MaxInventory, treasury.AssetLimit{Asset: a, MaxQuantity: q})
	}
	for _, l := range b.MaxInstrumentExposure {
		i, err := treasuryInstrument(l.InstrumentId, "max_instrument_exposure.instrument_id")
		if err != nil {
			return treasury.Policy{}, err
		}
		q, err := treasuryQuantity(l.MaxExposure, "max_instrument_exposure.max_exposure")
		if err != nil {
			return treasury.Policy{}, err
		}
		p.MaxInstrumentExposure = append(p.MaxInstrumentExposure, treasury.InstrumentLimit{Instrument: i, MaxExposure: q})
	}
	return p, nil
}

// --- response shapes -------------------------------------------------------------------

func treasuryStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return append([]string{}, in...)
}

// treasuryRef names an asset a view describes. The adapter describes every
// asset its records name; a missing one is an internal fault, never a guess.
func treasuryRef(known TreasuryAssets, aid assets.AssetID) (api.TreasuryAssetRef, error) {
	a, ok := known[aid]
	if !ok {
		return api.TreasuryAssetRef{}, errs.New(errs.CodeInternal, "a treasury record names an asset the read did not describe").
			WithField("asset_id", aid.String())
	}
	return toAPITreasuryAssetRef(a), nil
}

func toAPITreasuryAssetRef(a TreasuryAsset) api.TreasuryAssetRef {
	return api.TreasuryAssetRef{AssetId: toUUID(a.ID), Symbol: a.Symbol, Decimals: int(a.Decimals)}
}

func toAPITreasuryPrice(p money.Price) api.TreasuryPrice {
	out := api.TreasuryPrice{Mantissa: p.Mantissa.String(), Scale: int(p.Scale), Source: strPtr(p.Source)}
	if !p.At.IsZero() {
		at := p.At.UTC()
		out.ObservedAt = &at
	}
	return out
}

func toAPITreasuryMark(m *treasury.MarkRecord) *api.TreasuryPrice {
	if m == nil {
		return nil
	}
	p := toAPITreasuryPrice(money.Price{Mantissa: m.Mantissa, Scale: m.Scale, Source: m.Source, At: m.At})
	return &p
}

func toAPITreasuryBook(v TreasuryBookView) api.TreasuryBook {
	b := v.Book
	activities := treasury.PermittedActivities(b.Strategy)
	out := api.TreasuryBook{
		ValueDomain: api.SIMULATED, HouseAccountId: toUUID(b.HouseAccountID), Name: b.Name, Strategy: api.TreasuryStrategy(b.Strategy),
		Class: api.TreasuryClass(b.Class), Status: api.TreasuryBookStatus(b.Status), ValuationAsset: toAPITreasuryAssetRef(v.ValuationAsset),
		Version: b.Version, PermittedActivities: make([]api.TreasuryStrategy, 0, len(activities)),
		RegisteredByActorType: string(b.RegisteredBy.Type), RegisteredByActorId: b.RegisteredBy.ID, RegisteredAt: b.RegisteredAt.UTC(),
	}
	for _, a := range activities {
		out.PermittedActivities = append(out.PermittedActivities, api.TreasuryStrategy(a))
	}
	return out
}

func toAPITreasuryBookDetail(v TreasuryBookView) api.TreasuryBookDetail {
	b := toAPITreasuryBook(v)
	out := api.TreasuryBookDetail{
		ValueDomain: b.ValueDomain, HouseAccountId: b.HouseAccountId, Name: b.Name, Strategy: b.Strategy, Class: b.Class, Status: b.Status,
		ValuationAsset: b.ValuationAsset, Version: b.Version, PermittedActivities: b.PermittedActivities,
		RegisteredByActorType: b.RegisteredByActorType, RegisteredByActorId: b.RegisteredByActorId, RegisteredAt: b.RegisteredAt,
	}
	if v.Signing != nil {
		out.SigningPolicy = toAPITreasurySigningPolicy(*v.Signing)
	}
	if v.Risk != nil {
		r := toAPITreasuryRiskPolicy(*v.Risk)
		out.RiskPolicy = &r
	}
	return out
}

func toAPITreasurySigningPolicy(v treasury.SigningPolicyVersion) api.TreasurySigningPolicy {
	return api.TreasurySigningPolicy{
		HouseAccountId: toUUID(v.HouseAccountID), Version: v.Version, KeyCustody: treasury.KeyCustodyNone,
		ServiceActorId: v.Policy.ServiceActorID, OperatorsAllowed: v.Policy.OperatorsAllowed, PolicyHash: v.Hash,
		ActorType: string(v.Actor.Type), ActorId: v.Actor.ID, Reason: v.Reason, EffectiveAt: v.EffectiveAt.UTC(),
	}
}

func toAPITreasuryRiskPolicy(v treasury.PolicyVersion) api.TreasuryRiskPolicy {
	p := v.Policy
	out := api.TreasuryRiskPolicy{
		HouseAccountId: toUUID(v.HouseAccountID), Version: v.Version, Label: p.Label, PolicyHash: v.Hash,
		ValuationAssetId: toUUID(p.ValuationAsset), DailyLossLimit: p.DailyLossLimit.String(), MaxMarkAgeMs: p.MaxMarkAge.Milliseconds(),
		MaxInventory:          make([]api.TreasuryAssetLimit, 0, len(p.MaxInventory)),
		MaxInstrumentExposure: make([]api.TreasuryInstrumentLimit, 0, len(p.MaxInstrumentExposure)),
		PaperSpreadBps:        int(p.PaperSpreadBPS), PaperFeeBps: int(p.PaperFeeBPS),
		ActorType: string(v.Actor.Type), ActorId: v.Actor.ID, Reason: v.Reason, EffectiveAt: v.EffectiveAt.UTC(),
	}
	for _, l := range p.MaxInventory {
		out.MaxInventory = append(out.MaxInventory, api.TreasuryAssetLimit{AssetId: toUUID(l.Asset), MaxQuantity: l.MaxQuantity.String()})
	}
	for _, l := range p.MaxInstrumentExposure {
		out.MaxInstrumentExposure = append(out.MaxInstrumentExposure, api.TreasuryInstrumentLimit{InstrumentId: toUUID(l.Instrument),
			MaxExposure: l.MaxExposure.String()})
	}
	return out
}

func toAPITreasuryAction(a treasury.Action) api.TreasuryAction {
	out := api.TreasuryAction{
		Id: toUUID(a.ID), HouseAccountId: toUUID(a.HouseAccountID), Kind: api.TreasuryActionKind(a.Kind),
		Outcome: api.TreasuryOutcome(a.Outcome), RejectionCode: strPtr(a.RejectionCode), ReasonCodes: treasuryStrings(a.ReasonCodes),
		ActorType: string(a.Actor.Type), ActorId: a.Actor.ID, SigningPolicyVersion: a.SigningPolicyVersion,
		IdempotencyKey: a.IdempotencyKey, EvidenceRef: strPtr(a.EvidenceRef), CorrelationId: strPtr(a.CorrelationID),
		OccurredAt: a.OccurredAt.UTC(),
	}
	if a.Activity != "" {
		act := api.TreasuryStrategy(a.Activity)
		out.Activity = &act
	}
	if a.RiskPolicyVersion > 0 {
		v := a.RiskPolicyVersion
		out.RiskPolicyVersion = &v
	}
	if a.RiskDecisionID != nil {
		out.RiskDecisionId = uuidPtr(*a.RiskDecisionID)
	}
	if a.OrderID != nil {
		out.OrderId = uuidPtr(*a.OrderID)
	}
	if a.PaperFillID != nil {
		out.PaperFillId = uuidPtr(*a.PaperFillID)
	}
	if a.FundingID != nil {
		out.FundingId = uuidPtr(*a.FundingID)
	}
	if a.PolicyID != nil {
		out.PolicyId = uuidPtr(*a.PolicyID)
	}
	if a.SigningPolicyID != nil {
		out.SigningPolicyId = uuidPtr(*a.SigningPolicyID)
	}
	if a.SnapshotID != nil {
		out.SnapshotId = uuidPtr(*a.SnapshotID)
	}
	if a.EffectID != nil {
		out.EffectId = uuidPtr(*a.EffectID)
	}
	if len(a.EvidenceSHA256) > 0 {
		h := hex.EncodeToString(a.EvidenceSHA256)
		out.EvidenceSha256 = &h
	}
	return out
}

func toAPITreasuryHouseOrder(o orders.OrderView) api.TreasuryHouseOrder {
	out := api.TreasuryHouseOrder{
		Id: toUUID(o.ID), BookId: toUUID(o.BookID), InstrumentId: toUUID(o.InstrumentID), ClientOrderId: strPtr(o.ClientOrderID),
		Side: api.TreasurySide(o.Side), OrderType: api.TreasuryOrderType(o.Type), TimeInForce: api.TreasuryTimeInForce(o.TimeInForce),
		PostOnly: o.PostOnly, ReduceOnly: o.ReduceOnly, Mode: api.AdvancedMode(o.Mode), Price: o.Price, Quantity: o.Quantity.String(),
		FilledQuantity: o.FilledQuantity.String(), RemainingQuantity: o.RemainingQuantity.String(),
		CancelledQuantity: o.CancelledQuantity.String(), Status: api.TreasuryOrderStatus(o.Status), RejectReason: strPtr(o.RejectReason),
		CancelReason: strPtr(o.CancelReason), CreatedAt: o.CreatedAt.UTC(), UpdatedAt: o.UpdatedAt.UTC(),
	}
	if o.HouseAccountID != nil {
		out.HouseAccountId = toUUID(*o.HouseAccountID)
	}
	return out
}

func toAPITreasuryTrade(t orders.Trade) api.TreasuryTrade {
	return api.TreasuryTrade{
		Id: toUUID(t.ID), BookId: toUUID(t.BookID), TradeSeq: t.TradeSeq, Price: toAPITreasuryPrice(t.Price),
		BaseQuantity: t.BaseQuantity.String(), QuoteNotional: t.QuoteNotional.String(), MakerOrderId: toUUID(t.MakerOrderID),
		TakerOrderId: toUUID(t.TakerOrderID), MakerOwnerType: api.TreasuryOwnerType(t.MakerOwnerType),
		TakerOwnerType: api.TreasuryOwnerType(t.TakerOwnerType), AggressorSide: api.TreasurySide(t.AggressorSide),
		MakerFee: t.MakerFee.String(), TakerFee: t.TakerFee.String(), CrossingRuleVersion: t.CrossingRuleVersion,
		JournalTransactionId: toUUID(t.JournalTransactionID),
	}
}

func toAPITreasuryLimitEvaluation(l treasury.LimitEvaluation) api.TreasuryLimitEvaluation {
	return api.TreasuryLimitEvaluation{
		RiskIncreasing: l.RiskIncreasing, Codes: treasuryStrings(l.Codes), CircuitBreakers: treasuryStrings(l.Breakers),
		LossMeasure: l.LossMeasure.String(), DailyLossLimit: l.DailyLossLimit.String(), MissingMarks: treasuryStrings(l.MissingMarks),
		InventoryAfter: l.InventoryAfter.String(), InventoryCap: qtyPtr(l.InventoryCap), ExposureAfter: qtyPtr(l.ExposureAfter),
		ExposureCap: qtyPtr(l.ExposureCap),
	}
}

func toAPITreasuryDecision(d *treasury.RiskDecision) *api.TreasuryRiskDecision {
	if d == nil {
		return nil
	}
	out := api.TreasuryRiskDecision{
		Id: toUUID(d.ID), Verdict: api.TreasuryVerdict(d.Verdict), ReasonCodes: treasuryStrings(d.ReasonCodes),
		TreasuryCodes: treasuryStrings(d.TreasuryCodes), DecisionHash: d.DecisionHash, InputHash: d.InputHash,
	}
	// A replayed decision is read back without its limit evaluation; a fresh
	// one carries what it was decided on.
	if d.Limits.Codes != nil {
		l := toAPITreasuryLimitEvaluation(d.Limits)
		out.Limits = &l
	}
	return &out
}

func toAPITreasuryOrderOutcome(o treasury.OrderOutcome) api.TreasuryOrderOutcome {
	out := api.TreasuryOrderOutcome{ValueDomain: api.SIMULATED, Action: toAPITreasuryAction(o.Action),
		Trades: make([]api.TreasuryTrade, 0, len(o.Trades)), Decision: toAPITreasuryDecision(o.Decision)}
	if o.Order != nil {
		v := toAPITreasuryHouseOrder(*o.Order)
		out.Order = &v
	}
	for _, t := range o.Trades {
		out.Trades = append(out.Trades, toAPITreasuryTrade(t))
	}
	return out
}

func toAPITreasuryPaperFill(f treasury.PaperFill, known TreasuryAssets) (api.TreasuryPaperFill, error) {
	base, err := treasuryRef(known, f.BaseAsset)
	if err != nil {
		return api.TreasuryPaperFill{}, err
	}
	quote, err := treasuryRef(known, f.QuoteAsset)
	if err != nil {
		return api.TreasuryPaperFill{}, err
	}
	return api.TreasuryPaperFill{
		Id: toUUID(f.ID), HouseAccountId: toUUID(f.HouseAccountID), Activity: api.TreasuryStrategy(f.Activity), VenueLabel: f.VenueLabel,
		Side: api.TreasurySide(f.Side), BaseAsset: base, QuoteAsset: quote, BaseQuantity: f.BaseQuantity.String(),
		ReferencePrice: toAPITreasuryPrice(f.ReferencePrice), NotionalAtReference: f.NotionalAtReference.String(),
		SpreadBps: int(f.SpreadBPS), SpreadAmount: f.SpreadAmount.String(), FeeAmount: f.FeeAmount.String(),
		TotalQuote: f.TotalQuote.String(), ScheduleHash: f.ScheduleHash, DisclosureHash: f.DisclosureHash,
		Disclosure: toAPIFeeDisclosure(f.Disclosure), EffectId: toUUID(f.EffectID), JournalTransactionId: toUUID(f.JournalTransactionID),
		EvidenceRef: f.EvidenceRef, FilledAt: f.FilledAt.UTC(),
	}, nil
}

func toAPITreasuryPaperOutcome(v TreasuryPaperView) (api.TreasuryPaperOutcome, error) {
	o := v.Outcome
	out := api.TreasuryPaperOutcome{ValueDomain: api.SIMULATED, Action: toAPITreasuryAction(o.Action), Decision: toAPITreasuryDecision(o.Decision)}
	if o.Fill != nil {
		f, err := toAPITreasuryPaperFill(*o.Fill, v.Assets)
		if err != nil {
			return api.TreasuryPaperOutcome{}, err
		}
		out.Fill = &f
	}
	return out, nil
}

func toAPITreasuryCrossingDisclosure(d treasury.CrossingDisclosure) api.TreasuryCrossingDisclosure {
	return api.TreasuryCrossingDisclosure{
		Id: toUUID(d.ID), InstrumentId: toUUID(d.InstrumentID), BookId: toUUID(d.BookID), RuleVersion: d.RuleVersion, Allow: d.Allow,
		Disclosure: d.Disclosure, Reason: d.Reason, ActorType: string(d.Actor.Type), ActorId: d.Actor.ID, EvidenceRef: d.EvidenceRef,
		EvidenceSha256: hex.EncodeToString(d.EvidenceSHA256), DisclosedAt: d.DisclosedAt.UTC(),
	}
}

// treasuryUUID renders an identifier the treasury report carries as text. The
// treasury only ever writes canonical UUIDs; anything else is an internal
// fault, never a fabricated id.
func treasuryUUID(s string) (api.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return api.UUID{}, errs.New(errs.CodeInternal, "the treasury reported an identifier that is not a UUID")
	}
	return u, nil
}

func toAPITreasuryReport(v TreasuryReportView) (api.TreasuryReport, error) {
	r := v.Report
	house, err := treasuryUUID(r.HouseAccountID)
	if err != nil {
		return api.TreasuryReport{}, err
	}
	out := api.TreasuryReport{
		ValueDomain: api.SIMULATED, HouseAccountId: house, ValuationAsset: toAPITreasuryAssetRef(v.Valuation), At: r.At.UTC(),
		TradingDay: r.TradingDay, ThroughEventSeq: r.ThroughEventSeq, Assets: make([]api.TreasuryAssetPnL, 0, len(r.Assets)),
		RealizedTotal: r.RealizedTotal.String(), RealizedToday: r.RealizedToday.String(), UnrealizedTotal: r.UnrealizedTotal.String(),
		OpenLosses: r.OpenLosses.String(), LossMeasure: r.LossMeasure.String(), MissingMarks: make([]api.UUID, 0, len(r.MissingMarks)),
		Rounding: map[string]string{},
	}
	for k, mode := range r.Rounding {
		out.Rounding[k] = mode
	}
	for _, line := range r.Assets {
		aid, err := assets.ParseAssetID(line.Asset)
		if err != nil {
			return api.TreasuryReport{}, errs.New(errs.CodeInternal, "the treasury reported an asset id that is not a UUID")
		}
		ref, err := treasuryRef(v.Assets, aid)
		if err != nil {
			return api.TreasuryReport{}, err
		}
		out.Assets = append(out.Assets, api.TreasuryAssetPnL{
			Asset: ref, Quantity: line.Quantity.String(), Cost: line.Cost.String(), Mark: toAPITreasuryMark(line.Mark),
			MarketValue: qtyPtr(line.MarketValue), Unrealized: qtyPtr(line.Unrealized), RealizedTotal: line.RealizedTotal.String(),
			RealizedToday: line.RealizedToday.String(),
		})
	}
	for _, m := range r.MissingMarks {
		u, err := treasuryUUID(m)
		if err != nil {
			return api.TreasuryReport{}, err
		}
		out.MissingMarks = append(out.MissingMarks, u)
	}
	if v.RiskPolicyVersion > 0 {
		version := v.RiskPolicyVersion
		out.RiskPolicyVersion = &version
		limits := make([]api.TreasuryLimitUse, 0, len(v.Limits))
		for _, l := range v.Limits {
			use := api.TreasuryLimitUse{Kind: api.TreasuryLimitKind(l.Kind), Cap: qtyPtr(l.Cap), Used: qtyPtr(l.Used), Codes: treasuryStrings(l.Codes)}
			if l.Asset != nil {
				use.AssetId = uuidPtr(*l.Asset)
			}
			if l.Instrument != nil {
				use.InstrumentId = uuidPtr(*l.Instrument)
			}
			limits = append(limits, use)
		}
		out.Limits = &limits
	}
	return out, nil
}
