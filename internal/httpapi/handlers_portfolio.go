package httpapi

import (
	"context"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
)

// portfolioDecisionLimit is how many of the newest Risk Kernel V2 decisions
// the portfolio shows.
const portfolioDecisionLimit = 20

// GetAdvancedPortfolio answers one account's simulated portfolio.
//
// Its own reads -- the balances, asset by asset, and what stands in the way
// of new risk -- come from PortfolioPort. Every other section is a
// subsystem's own read, answered by the handler that subsystem's route uses
// (or, for a prediction market's positions, by that subsystem's port and
// converters), so a figure on the portfolio is the figure on the subsystem's
// own page and nothing here computes one. Each composed read is
// account-scoped by the same rule and held to the same policy as this one
// (TestPortfolio_ComposesOnlyReadsUnderItsOwnPolicy), so the portfolio is
// never a way around a subsystem's own refusal. A subsystem this deployment
// does not run is reported NOT_CONFIGURED, and a read that refused is
// reported with its stable code; an internal fault fails the whole read.
func (s *Server) GetAdvancedPortfolio(ctx context.Context, request api.GetAdvancedPortfolioRequestObject) (api.GetAdvancedPortfolioResponseObject, error) {
	if s.opts.Ports.Portfolio == nil {
		return nil, errNotWired("the advanced portfolio")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	snap, err := s.opts.Ports.Portfolio.Snapshot(ctx, account)
	if err != nil {
		return nil, err
	}
	if snap.AccountID != account {
		return nil, errs.New(errs.CodeInternal, "the portfolio read answered for another account")
	}
	id := toUUID(account)
	out := api.AdvancedPortfolio{
		AccountId: id, AsOf: snap.AsOf.UTC(), QuoteAssetId: toUUID(snap.Quote.ID), QuoteSymbol: snap.Quote.Symbol,
		QuoteDecimals: int(snap.Quote.Decimals), QuoteProvisioned: snap.QuoteProvisioned, MaxMarkAgeMs: snap.MaxMarkAge.Milliseconds(),
		Balances: make([]api.AdvancedPortfolioBalance, 0, len(snap.Balances)), ValueDomain: api.SIMULATED,
	}
	for _, b := range snap.Balances {
		out.Balances = append(out.Balances, toAPIPortfolioBalance(b, snap.AsOf))
	}
	if out.BuyingPower, err = s.portfolioBuyingPower(ctx, id); err != nil {
		return nil, err
	}
	if out.Positions.Margin, err = s.portfolioMargin(ctx, id); err != nil {
		return nil, err
	}
	if out.Positions.Derivatives, err = s.portfolioDerivatives(ctx, id); err != nil {
		return nil, err
	}
	if out.Positions.PredictionMarkets, err = s.portfolioPredictionMarkets(ctx, account, snap); err != nil {
		return nil, err
	}
	if out.Positions.Equities, err = s.portfolioEquities(ctx, id, snap.Balances); err != nil {
		return nil, err
	}
	if out.Positions.OrderBooks, err = s.portfolioOrderBooks(ctx, id); err != nil {
		return nil, err
	}
	if out.Risk, err = s.portfolioRisk(ctx, id, snap); err != nil {
		return nil, err
	}
	return api.GetAdvancedPortfolio200JSONResponse(out), nil
}

// portfolioRefusal reads a composed read's error. A refusal is the stable
// code and detail the subsystem's own route answers with; an internal fault
// is no state of a section and is returned, to fail the whole read where the
// error path records it.
func portfolioRefusal(ctx context.Context, err error) (*api.AdvancedPortfolioProblem, error) {
	p := errs.ToProblem(classify(ctx, err), "", "")
	if p.Code == errs.CodeInternal {
		return nil, err
	}
	return &api.AdvancedPortfolioProblem{Code: string(p.Code), Detail: p.Detail}, nil
}

// portfolioAnswer is a composed handler's success, which is its 200 answer
// and nothing else.
func portfolioAnswer[T any](what string, res any) (T, error) {
	body, ok := res.(T)
	if !ok {
		var zero T
		return zero, errs.Newf(errs.CodeInternal, "the %s read answered with an unexpected response", what)
	}
	return body, nil
}

func (s *Server) portfolioBuyingPower(ctx context.Context, id api.UUID) (api.AdvancedPortfolioBuyingPower, error) {
	out := api.AdvancedPortfolioBuyingPower{State: api.PortfolioSectionNotConfigured, ValueDomain: api.SIMULATED}
	if s.opts.Ports.Advanced == nil {
		return out, nil
	}
	res, err := s.GetAdvancedBuyingPower(ctx, api.GetAdvancedBuyingPowerRequestObject{Params: api.GetAdvancedBuyingPowerParams{AccountId: id}})
	if err != nil {
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	body, err := portfolioAnswer[api.GetAdvancedBuyingPower200JSONResponse]("buying power", res)
	if err != nil {
		return out, err
	}
	bp := api.BuyingPowerV2(body)
	out.State, out.BuyingPower = api.PortfolioSectionPresent, &bp
	return out, nil
}

// portfolioMargin is the margin account as /advanced/margin/account answers
// it. NOT_FOUND there means the account has no margin account, which is
// NONE here, not a refusal.
func (s *Server) portfolioMargin(ctx context.Context, id api.UUID) (api.AdvancedPortfolioMargin, error) {
	out := api.AdvancedPortfolioMargin{State: api.PortfolioSectionNotConfigured, ValueDomain: api.SIMULATED}
	if s.opts.Ports.Margin == nil {
		return out, nil
	}
	res, err := s.GetAdvancedMarginAccount(ctx, api.GetAdvancedMarginAccountRequestObject{Params: api.GetAdvancedMarginAccountParams{AccountId: id}})
	if err != nil {
		if classify(ctx, err).Code == errs.CodeNotFound {
			out.State = api.PortfolioSectionNone
			return out, nil
		}
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	body, err := portfolioAnswer[api.GetAdvancedMarginAccount200JSONResponse]("margin account", res)
	if err != nil {
		return out, err
	}
	detail := api.MarginAccountDetail(body)
	out.State, out.Account = api.PortfolioSectionPresent, &detail
	return out, nil
}

// portfolioDerivatives is the OPEN positions and the book's valuation, as
// /advanced/derivatives/positions?status=OPEN answers them.
func (s *Server) portfolioDerivatives(ctx context.Context, id api.UUID) (api.AdvancedPortfolioDerivatives, error) {
	out := api.AdvancedPortfolioDerivatives{State: api.PortfolioSectionNotConfigured, ValueDomain: api.SIMULATED}
	if s.opts.Ports.Derivatives == nil {
		return out, nil
	}
	status, limit := api.DerivativePositionOpen, api.Limit(portfolioListBound)
	res, err := s.GetAdvancedDerivativesPositions(ctx, api.GetAdvancedDerivativesPositionsRequestObject{
		Params: api.GetAdvancedDerivativesPositionsParams{AccountId: id, Status: &status, Limit: &limit},
	})
	if err != nil {
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	body, err := portfolioAnswer[api.GetAdvancedDerivativesPositions200JSONResponse]("derivative positions", res)
	if err != nil {
		return out, err
	}
	page := api.DerivativePositionPage(body)
	out.State, out.Positions = api.PortfolioSectionPresent, &page
	return out, nil
}

// portfolioPredictionMarkets is the account's shares in each market the
// snapshot found it holding shares in or owed a held payout by: the
// prediction market port's own read of each, rendered by that subsystem's
// converters.
func (s *Server) portfolioPredictionMarkets(ctx context.Context, account accounts.AccountID, snap PortfolioSnapshot) (api.AdvancedPortfolioPredictionMarkets, error) {
	out := api.AdvancedPortfolioPredictionMarkets{
		State: api.PortfolioSectionNotConfigured, Markets: []api.AdvancedPortfolioPredictionHolding{}, ValueDomain: api.SIMULATED,
	}
	port := s.opts.Ports.PredictionMarkets
	if port == nil {
		return out, nil
	}
	markets := make([]api.AdvancedPortfolioPredictionHolding, 0, len(snap.PredictionMarkets))
	for _, mid := range snap.PredictionMarkets {
		v, err := port.Positions(ctx, mid, account)
		if err != nil {
			out.State = api.PortfolioSectionUnavailable
			out.Problem, err = portfolioRefusal(ctx, err)
			return out, err
		}
		positions, err := portfolioPredictionPositions(v, account)
		if err != nil {
			return out, err
		}
		markets = append(markets, api.AdvancedPortfolioPredictionHolding{Market: toAPIPredictionMarket(v.Record), Positions: positions})
	}
	out.State, out.Markets, out.Truncated = api.PortfolioSectionPresent, markets, snap.PredictionMarketsTruncated
	return out, nil
}

// portfolioPredictionPositions renders one market's positions exactly as
// GetAdvancedPredictionMarketsMarketIdPositions renders them (a test holds
// the two to the same bytes), refusing, as it does, a row of another account.
func portfolioPredictionPositions(v PredictionPositionsView, account accounts.AccountID) (api.PredictionPositionList, error) {
	out := api.PredictionPositionList{
		ValueDomain: predmarketDomain, MarketId: toUUID(v.Record.Market.ID), AccountId: toUUID(account),
		ShareDecimals: int(predmarket.ShareDecimals), Items: make([]api.PredictionPosition, 0, len(v.Positions)),
	}
	for _, p := range v.Positions {
		if p.AccountID != account {
			return api.PredictionPositionList{}, errs.New(errs.CodeInternal, "the positions read returned another account's shares")
		}
		out.Items = append(out.Items, api.PredictionPosition{
			OutcomeIndex: p.OutcomeIndex, OutcomeSymbol: p.OutcomeSymbol, ShareAssetId: toUUID(p.ShareAssetID),
			Quantity: p.Quantity.String(), AsOf: p.AsOf.UTC(),
		})
	}
	if v.HeldPayout != nil {
		if v.HeldPayout.AccountID != account {
			return api.PredictionPositionList{}, errs.New(errs.CodeInternal, "the positions read returned another account's held payout")
		}
		h := toAPIPredictionHeldPayout(*v.HeldPayout)
		out.HeldPayout = &h
	}
	return out, nil
}

// portfolioEquities is the equity listings whose shares the account holds,
// as the listings route answers them, with the share balances from the
// balances read, and the PENDING settlements as the settlements route
// answers them.
func (s *Server) portfolioEquities(ctx context.Context, id api.UUID, balances []PortfolioBalance) (api.AdvancedPortfolioEquities, error) {
	out := api.AdvancedPortfolioEquities{
		State: api.PortfolioSectionNotConfigured, Holdings: []api.AdvancedPortfolioEquityHolding{}, ValueDomain: api.SIMULATED,
	}
	if s.opts.Ports.Equities == nil {
		return out, nil
	}
	listingLimit := api.Limit(maxPageLimit)
	res, err := s.GetAdvancedEquitiesListings(ctx, api.GetAdvancedEquitiesListingsRequestObject{
		Params: api.GetAdvancedEquitiesListingsParams{Limit: &listingLimit},
	})
	if err != nil {
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	listings, err := portfolioAnswer[api.GetAdvancedEquitiesListings200JSONResponse]("equity listings", res)
	if err != nil {
		return out, err
	}
	pending, limit := api.EquitySettlementFilterPending, api.Limit(portfolioListBound)
	sres, err := s.GetAdvancedEquitiesSettlements(ctx, api.GetAdvancedEquitiesSettlementsRequestObject{
		Params: api.GetAdvancedEquitiesSettlementsParams{AccountId: id, State: &pending, Limit: &limit},
	})
	if err != nil {
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	settlements, err := portfolioAnswer[api.GetAdvancedEquitiesSettlements200JSONResponse]("equity settlements", sres)
	if err != nil {
		return out, err
	}
	held := make(map[string]PortfolioBalance, len(balances))
	for _, b := range balances {
		held[b.Asset.ID.String()] = b
	}
	for _, l := range listings.Items {
		b, ok := held[l.ShareAssetId.String()]
		if !ok {
			continue
		}
		unsettled := portfolioHeldUnder(b, ledger.CodeUnsettledReceivable)
		if b.Wallet.IsZero() && unsettled.IsZero() {
			continue
		}
		out.Holdings = append(out.Holdings, api.AdvancedPortfolioEquityHolding{Listing: l, Settled: b.Wallet.String(), Unsettled: unsettled.String()})
	}
	page := api.AdvancedEquitySettlementPage(settlements)
	out.State, out.PendingSettlements, out.ListingsTruncated = api.PortfolioSectionPresent, &page, len(listings.Items) >= maxPageLimit
	return out, nil
}

// portfolioHeldUnder is a balance's non-wallet quantity under one code, zero
// when it holds none there.
func portfolioHeldUnder(b PortfolioBalance, code ledger.Code) money.Quantity {
	for _, e := range b.Elsewhere {
		if e.Code == code {
			return e.Quantity
		}
	}
	return money.Quantity{}
}

// portfolioOrderBooks is the WORKING orders as /advanced/books/orders answers
// them.
func (s *Server) portfolioOrderBooks(ctx context.Context, id api.UUID) (api.AdvancedPortfolioOrderBooks, error) {
	out := api.AdvancedPortfolioOrderBooks{State: api.PortfolioSectionNotConfigured, ValueDomain: api.SIMULATED}
	if s.opts.Ports.Books == nil {
		return out, nil
	}
	scope, limit := api.BookOrderScopeWorking, api.Limit(portfolioListBound)
	res, err := s.GetAdvancedBooksOrders(ctx, api.GetAdvancedBooksOrdersRequestObject{
		Params: api.GetAdvancedBooksOrdersParams{AccountId: id, Scope: &scope, Limit: &limit},
	})
	if err != nil {
		out.State = api.PortfolioSectionUnavailable
		out.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	body, err := portfolioAnswer[api.GetAdvancedBooksOrders200JSONResponse]("book orders", res)
	if err != nil {
		return out, err
	}
	page := api.AdvancedBookOrderPage(body)
	out.State, out.Orders = api.PortfolioSectionPresent, &page
	return out, nil
}

// portfolioRisk is what stands in the way of new risk, from the snapshot, and
// the newest Risk Kernel V2 decisions as /advanced/risk/decisions answers
// them.
func (s *Server) portfolioRisk(ctx context.Context, id api.UUID, snap PortfolioSnapshot) (api.AdvancedPortfolioRisk, error) {
	out := api.AdvancedPortfolioRisk{
		AccountStatus: string(snap.AccountStatus), BlocksNewRisk: snap.BlocksNewRisk,
		Decisions:               api.AdvancedPortfolioRiskDecisions{State: api.PortfolioSectionNotConfigured, Items: []api.AdvancedRiskDecision{}},
		Reconciliation:          make([]api.AdvancedPortfolioReconciliationRecord, 0, len(snap.Reconciliation)),
		ReconciliationTruncated: snap.ReconciliationTruncated,
		UnknownEffects:          make([]api.EconomicEffect, 0, len(snap.UnknownEffects)),
		UnknownEffectsTruncated: snap.UnknownEffectsTruncated,
	}
	for _, r := range snap.Reconciliation {
		out.Reconciliation = append(out.Reconciliation, toAPIPortfolioRecord(r))
	}
	for _, e := range snap.UnknownEffects {
		out.UnknownEffects = append(out.UnknownEffects, toAPIEconomicEffect(e))
	}
	if s.opts.Ports.Advanced == nil {
		return out, nil
	}
	limit := api.Limit(portfolioDecisionLimit)
	res, err := s.GetAdvancedRiskDecisions(ctx, api.GetAdvancedRiskDecisionsRequestObject{
		Params: api.GetAdvancedRiskDecisionsParams{AccountId: id, Limit: &limit},
	})
	if err != nil {
		out.Decisions.State = api.PortfolioSectionUnavailable
		out.Decisions.Problem, err = portfolioRefusal(ctx, err)
		return out, err
	}
	body, err := portfolioAnswer[api.GetAdvancedRiskDecisions200JSONResponse]("risk decisions", res)
	if err != nil {
		return out, err
	}
	out.Decisions.State, out.Decisions.Items = api.PortfolioSectionPresent, api.AdvancedRiskDecisionList(body).Items
	return out, nil
}

func toAPIPortfolioRecord(r reconciliation.Record) api.AdvancedPortfolioReconciliationRecord {
	return api.AdvancedPortfolioReconciliationRecord{
		Id: toUUID(r.ID), Kind: string(r.Kind), Status: string(r.Status), Material: r.Material, BlocksNewRisk: r.BlocksNewRisk,
		ScopeType: r.ScopeType, AssetId: uuidPtr(r.AssetID), OpenedAt: r.OpenedAt.UTC(),
	}
}

func toAPIPortfolioBalance(b PortfolioBalance, asOf time.Time) api.AdvancedPortfolioBalance {
	out := api.AdvancedPortfolioBalance{
		AssetId: toUUID(b.Asset.ID), Symbol: b.Asset.Symbol, Name: b.Asset.Name, Chain: b.Asset.Chain, Decimals: int(b.Asset.Decimals),
		Wallet: b.Wallet.String(), Reserved: b.Reserved.String(), WithdrawalHolds: b.WithdrawalHolds.String(),
		Available: b.Available.String(), Total: b.Total.String(),
		Reservations:  make([]api.AdvancedPortfolioReservation, 0, len(b.Reservations)),
		HeldElsewhere: make([]api.AdvancedPortfolioLedgerBalance, 0, len(b.Elsewhere)),
		Owed:          make([]api.AdvancedPortfolioLedgerBalance, 0, len(b.Owed)),
		Valuation:     toAPIPortfolioValuation(b.Valuation, asOf), ValueDomain: api.SIMULATED,
	}
	for _, r := range b.Reservations {
		out.Reservations = append(out.Reservations, api.AdvancedPortfolioReservation{
			Purpose: api.AdvancedPortfolioReservationPurpose(r.Purpose), Capability: strPtr(r.Capability),
			Quantity: r.Quantity.String(), Reservations: r.Count,
		})
	}
	for _, e := range b.Elsewhere {
		out.HeldElsewhere = append(out.HeldElsewhere, api.AdvancedPortfolioLedgerBalance{Code: api.AdvancedPortfolioLedgerCode(e.Code), Quantity: e.Quantity.String()})
	}
	for _, o := range b.Owed {
		out.Owed = append(out.Owed, api.AdvancedPortfolioLedgerBalance{Code: api.AdvancedPortfolioLedgerCode(o.Code), Quantity: o.Quantity.String()})
	}
	return out
}

func toAPIPortfolioValuation(v PortfolioValuation, asOf time.Time) api.AdvancedPortfolioValuation {
	out := api.AdvancedPortfolioValuation{
		Status: api.AdvancedPortfolioValuationStatus(v.Status), Value: qtyPtr(v.Value), OwedValue: qtyPtr(v.OwedValue),
	}
	if v.Mark != nil {
		m := *v.Mark
		out.Mark = &api.AdvancedPortfolioMark{
			Price:    m.Mantissa.ToDecimalString(uint8(m.Scale)), // #nosec G115 -- money.Price.Validate bounds the scale to MaxPriceScale
			Mantissa: m.Mantissa.String(), Scale: int(m.Scale), Source: m.Source, ObservedAt: m.At.UTC(),
			AgeMs: asOf.Sub(m.At).Milliseconds(),
		}
	}
	if v.Problem != nil {
		p := errs.ToProblem(v.Problem, "", "")
		out.Problem = &api.AdvancedPortfolioProblem{Code: string(p.Code), Detail: p.Detail}
	}
	return out
}
