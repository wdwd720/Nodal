package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/derivatives"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/ledger"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// Futures, perpetuals and options (internal/derivatives, ADR-0020). These
// handlers translate shapes, scope every account and record to its owner,
// and run every command under the Idempotency-Key contract. Every financial
// rule -- what may be traded, at what price, with what margin, when a run or
// a settlement may happen -- is the service's.

const derivativesWhat = "advanced derivatives"

// --- reference data ---------------------------------------------------------

// GetAdvancedDerivativesPolicy returns the policy every derivative decision
// records by version and hash.
func (s *Server) GetAdvancedDerivativesPolicy(ctx context.Context, _ api.GetAdvancedDerivativesPolicyRequestObject) (api.GetAdvancedDerivativesPolicyResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	p, err := port.Policy(ctx)
	if err != nil {
		return nil, err
	}
	out := api.DerivativePolicy{
		Version: p.Version, Hash: p.Hash(), Linear: make([]api.DerivativeClassRates, 0, len(p.Linear)),
		FundingBufferIntervals: p.FundingBufferIntervals, MaxFundingRateE8: p.MaxFundingRateE8,
		FundingRateScale: derivatives.FundingRateScale, OptionShocksBps: make([]int64, 0, len(p.OptionShocksBPS)),
		OptionMinMarginBps: int64(p.OptionMinMarginBPS), OptionMaintenanceRatioBps: int64(p.OptionMaintenanceRatioBPS),
		ExpiryGapWindowMs: p.ExpiryGapWindowMS, ExpiryGapShockBps: int64(p.ExpiryGapShockBPS),
		PaperSlippageBps: int64(p.PaperSlippageBPS), VariationMarginIntervalMs: p.VariationMarginIntervalMS,
	}
	for _, c := range p.Linear {
		out.Linear = append(out.Linear, api.DerivativeClassRates{
			RiskClass: string(c.RiskClass), InitialBps: int64(c.InitialBPS), MaintenanceBps: int64(c.MaintenanceBPS),
		})
	}
	for _, shock := range p.OptionShocksBPS {
		out.OptionShocksBps = append(out.OptionShocksBps, int64(shock))
	}
	return api.GetAdvancedDerivativesPolicy200JSONResponse(out), nil
}

// GetAdvancedDerivativesContracts lists derivative contracts with the terms
// the service trades them on and their latest mark.
func (s *Server) GetAdvancedDerivativesContracts(ctx context.Context, request api.GetAdvancedDerivativesContractsRequestObject) (api.GetAdvancedDerivativesContractsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	var kind instruments.InstrumentType
	if request.Params.Kind != nil {
		if !request.Params.Kind.Valid() {
			return nil, validationError("kind", "kind must be FUTURE, PERPETUAL or OPTION")
		}
		kind = instruments.InstrumentType(*request.Params.Kind)
	}
	views, err := port.Contracts(ctx, kind, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.DerivativeContractList{ValueDomain: api.SIMULATED, Items: make([]api.DerivativeContract, 0, len(views))}
	for _, v := range views {
		c, err := toAPIDerivativeContract(v)
		if err != nil {
			return nil, err
		}
		out.Items = append(out.Items, c)
	}
	return api.GetAdvancedDerivativesContracts200JSONResponse(out), nil
}

// GetAdvancedDerivativesContractsInstrumentId returns one derivative contract.
func (s *Server) GetAdvancedDerivativesContractsInstrumentId(ctx context.Context, request api.GetAdvancedDerivativesContractsInstrumentIdRequestObject) (api.GetAdvancedDerivativesContractsInstrumentIdResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	iid, err := parseDerivativeInstrument(request.InstrumentId.String())
	if err != nil {
		return nil, err
	}
	v, err := port.Contract(ctx, iid)
	if err != nil {
		return nil, err
	}
	c, err := toAPIDerivativeContract(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDerivativesContractsInstrumentId200JSONResponse(c), nil
}

// GetAdvancedDerivativesContractsInstrumentIdMarks pages a contract's marks.
func (s *Server) GetAdvancedDerivativesContractsInstrumentIdMarks(ctx context.Context, request api.GetAdvancedDerivativesContractsInstrumentIdMarksRequestObject) (api.GetAdvancedDerivativesContractsInstrumentIdMarksResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	iid, err := parseDerivativeInstrument(request.InstrumentId.String())
	if err != nil {
		return nil, err
	}
	marks, next, err := port.Marks(ctx, iid, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.DerivativeMarkPage{ValueDomain: api.SIMULATED, Items: make([]api.DerivativeMark, 0, len(marks)), NextCursor: nextCursor(next)}
	for _, m := range marks {
		page.Items = append(page.Items, toAPIDerivativeMark(m))
	}
	return api.GetAdvancedDerivativesContractsInstrumentIdMarks200JSONResponse(page), nil
}

// GetAdvancedDerivativesContractsInstrumentIdAnalytics returns an option's
// reference analytics at its latest mark, labelled by the service as model
// values, with the inputs the model used.
func (s *Server) GetAdvancedDerivativesContractsInstrumentIdAnalytics(ctx context.Context, request api.GetAdvancedDerivativesContractsInstrumentIdAnalyticsRequestObject) (api.GetAdvancedDerivativesContractsInstrumentIdAnalyticsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	iid, err := parseDerivativeInstrument(request.InstrumentId.String())
	if err != nil {
		return nil, err
	}
	req := derivatives.AnalyticsRequest{InstrumentID: iid, VolBPS: money.BPS(request.Params.VolBps)}
	if request.Params.RateBps != nil {
		req.RateBPS = money.BPS(*request.Params.RateBps)
	}
	if request.Params.DividendYieldBps != nil {
		req.DividendYieldBPS = money.BPS(*request.Params.DividendYieldBps)
	}
	v, err := port.Analytics(ctx, req)
	if err != nil {
		return nil, err
	}
	a := v.Analytics
	out := api.DerivativeOptionAnalytics{
		ValueDomain: api.SIMULATED, Label: a.Label, Model: a.Model, InstrumentId: toUUID(iid), Right: a.Right, Style: a.Style,
		Regime: a.Regime, Spot: a.Spot, Strike: a.Strike, TimeYears: a.TimeYears, Vol: a.Vol, Price: a.Price, Delta: a.Delta,
		Gamma: a.Gamma, ThetaPerYear: a.Theta, ThetaPerDay: a.ThetaPerDay, Vega: a.Vega, VegaPerVolPoint: a.VegaPerPoint,
		Intrinsic: a.Intrinsic, TimeValue: strPtr(a.TimeValue), ImpliedVol: strPtr(a.ImpliedVol),
		ImpliedVolReason: strPtr(a.ImpliedVolReason), AsOf: a.AsOf.UTC(),
		Inputs: api.DerivativeAnalyticsInputs{
			VolBps: int64(v.Inputs.VolBPS), RateBps: int64(v.Inputs.RateBPS), DividendYieldBps: int64(v.Inputs.DividendYieldBPS),
		},
		Mark: toAPIDerivativeMark(v.Mark),
	}
	return api.GetAdvancedDerivativesContractsInstrumentIdAnalytics200JSONResponse(out), nil
}

// --- positions and the flow register ------------------------------------------

// GetAdvancedDerivativesPositions pages an account's positions with the
// service's valuation of its open book.
func (s *Server) GetAdvancedDerivativesPositions(ctx context.Context, request api.GetAdvancedDerivativesPositionsRequestObject) (api.GetAdvancedDerivativesPositionsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var status derivatives.PositionStatus
	if request.Params.Status != nil {
		if !request.Params.Status.Valid() {
			return nil, validationError("status", "status must be OPEN, CLOSED or SETTLED")
		}
		status = derivatives.PositionStatus(*request.Params.Status)
	}
	pv, err := port.Positions(ctx, account, status, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.DerivativePositionPage{
		ValueDomain: api.SIMULATED, Items: make([]api.DerivativePosition, 0, len(pv.Items)), NextCursor: nextCursor(pv.Next),
	}
	for _, v := range pv.Items {
		p, err := toAPIDerivativePosition(v)
		if err != nil {
			return nil, err
		}
		page.Items = append(page.Items, p)
	}
	switch {
	case pv.Book != nil:
		page.BookStatus = api.DerivativeBookValued
		b := toAPIDerivativeBook(*pv.Book, pv.Quote)
		page.Book = &b
	case pv.BookRefusal != nil:
		page.BookStatus = api.DerivativeBookRefused
		page.BookRefusal = &api.DerivativeRefusal{Code: string(pv.BookRefusal.Code), Detail: pv.BookRefusal.Detail}
	default:
		return nil, errs.New(errs.CodeInternal, "the book was neither valued nor refused")
	}
	return api.GetAdvancedDerivativesPositions200JSONResponse(page), nil
}

// GetAdvancedDerivativesPositionsPositionId returns one position to its
// account's owner or an operator who may read any account. Anyone else gets
// exactly the NOT_FOUND an absent id gets.
func (s *Server) GetAdvancedDerivativesPositionsPositionId(ctx context.Context, request api.GetAdvancedDerivativesPositionsPositionIdRequestObject) (api.GetAdvancedDerivativesPositionsPositionIdResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	pid, err := parseDerivativePosition(request.PositionId.String())
	if err != nil {
		return nil, err
	}
	v, err := port.Position(ctx, pid)
	if err != nil {
		return nil, maskDerivativePositionNotFound(err, pid)
	}
	// NOT_FOUND rather than FORBIDDEN: a distinguishable refusal would tell a
	// stranger that the id exists (F-41).
	if err := security.RequireAccount(ctx, v.Position.AccountID.String()); err != nil {
		return nil, derivativePositionNotFound(pid)
	}
	p, err := toAPIDerivativePosition(v)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedDerivativesPositionsPositionId200JSONResponse(p), nil
}

// GetAdvancedDerivativesFlows pages an account's clearing-house flow register.
func (s *Server) GetAdvancedDerivativesFlows(ctx context.Context, request api.GetAdvancedDerivativesFlowsRequestObject) (api.GetAdvancedDerivativesFlowsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var filter DerivativeFlowFilter
	if request.Params.PositionId != nil {
		pid, err := derivatives.ParsePositionID(request.Params.PositionId.String())
		if err != nil || pid.IsZero() {
			return nil, validationError("position_id", "position_id must be a canonical UUID")
		}
		filter.PositionID = &pid
	}
	if request.Params.Kind != nil {
		if !request.Params.Kind.Valid() {
			return nil, validationError("kind", "kind is not a flow kind")
		}
		filter.Kind = derivatives.CashFlowKind(*request.Params.Kind)
	}
	items, next, err := port.Flows(ctx, account, filter, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.DerivativeFlowPage{ValueDomain: api.SIMULATED, Items: make([]api.DerivativeFlow, 0, len(items)), NextCursor: nextCursor(next)}
	for _, v := range items {
		f := v.Flow
		page.Items = append(page.Items, api.DerivativeFlow{
			Id: toUUID(f.ID), PositionId: toUUID(f.PositionID), AccountId: toUUID(f.AccountID), InstrumentId: toUUID(f.InstrumentID),
			Symbol: v.Symbol, Kind: api.DerivativeFlowKind(f.Kind), Amount: f.Amount.String(), Shortfall: f.Shortfall.String(),
			SettlementAsset: toAPIDerivativeAsset(v.Settlement), JournalTransactionId: journalPtr(f.JournalTransactionID),
			ReferenceType: f.ReferenceType, ReferenceId: f.ReferenceID, OccurredAt: f.OccurredAt.UTC(),
		})
	}
	return api.GetAdvancedDerivativesFlows200JSONResponse(page), nil
}

// GetAdvancedDerivativesFundingAccruals pages the funding accrued to an
// account's positions.
func (s *Server) GetAdvancedDerivativesFundingAccruals(ctx context.Context, request api.GetAdvancedDerivativesFundingAccrualsRequestObject) (api.GetAdvancedDerivativesFundingAccrualsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	var position *derivatives.PositionID
	if request.Params.PositionId != nil {
		pid, err := derivatives.ParsePositionID(request.Params.PositionId.String())
		if err != nil || pid.IsZero() {
			return nil, validationError("position_id", "position_id must be a canonical UUID")
		}
		position = &pid
	}
	items, next, err := port.FundingAccruals(ctx, account, position, deref(request.Params.Cursor), pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.DerivativeFundingAccrualPage{
		ValueDomain: api.SIMULATED, Items: make([]api.DerivativeFundingAccrual, 0, len(items)), NextCursor: nextCursor(next),
	}
	for _, a := range items {
		row := api.DerivativeFundingAccrual{
			Id: toUUID(a.ID), FundingRateId: toUUID(a.FundingRateID), PositionId: toUUID(a.PositionID), InstrumentId: toUUID(a.InstrumentID),
			Symbol: a.Symbol, IntervalStart: a.IntervalStart.UTC(), IntervalEnd: a.IntervalEnd.UTC(), Quantity: a.Quantity.String(),
			Direction: api.DerivativeFundingDirection(a.Direction), Amount: a.Amount.String(), RateE8: a.RateE8,
			PremiumIndexE8: a.PremiumIndexE8, Clamped: a.Clamped, PerContractAmount: a.PerContract.String(),
			MarkMantissa: a.MarkMantissa.String(), PriceScale: int(a.PriceScale), SettlementAsset: toAPIDerivativeAsset(a.Settlement),
		}
		if a.FlowID != nil {
			row.FlowId = uuidPtr(*a.FlowID)
		}
		page.Items = append(page.Items, row)
	}
	return api.GetAdvancedDerivativesFundingAccruals200JSONResponse(page), nil
}

// --- the customer's commands ------------------------------------------------

// PostAdvancedDerivativesPositions opens or adds to a position in one of the
// caller's own accounts. A refusal the service records is committed and
// reaches the client with its stable code; the intent stays readable as
// REJECTED.
func (s *Server) PostAdvancedDerivativesPositions(ctx context.Context, request api.PostAdvancedDerivativesPositionsRequestObject) (api.PostAdvancedDerivativesPositionsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	account, err := accountScopeWrite(ctx, b.AccountId)
	if err != nil {
		return nil, err
	}
	iid, err := instruments.ParseInstrumentID(b.InstrumentId.String())
	if err != nil || iid.IsZero() {
		return nil, validationError("instrument_id", "instrument_id must be a canonical UUID")
	}
	if !b.Side.Valid() {
		return nil, validationError("side", "side must be LONG or SHORT")
	}
	qty, err := parseDerivativeQuantity("quantity", b.Quantity)
	if err != nil {
		return nil, err
	}
	if !b.Mode.Valid() {
		return nil, validationError("mode", "mode is not an advanced mode")
	}
	key := request.Params.IdempotencyKey
	cmd := DerivativeOpenCommand{
		AccountID: account, InstrumentID: iid, Side: derivatives.Side(b.Side), Quantity: qty, Mode: gates.Mode(b.Mode),
		IdempotencyKey: key, CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.DerivativeTradeResult, commandMeta, error) {
		v, oerr := port.Open(ctx, cmd)
		if oerr != nil {
			return api.DerivativeTradeResult{}, commandMeta{}, oerr
		}
		out, cerr := toAPIDerivativeTrade(v)
		if cerr != nil {
			return api.DerivativeTradeResult{}, commandMeta{}, cerr
		}
		status := http.StatusCreated
		if v.Outcome.Replay {
			// The service had already carried out this key: nothing new exists.
			status = http.StatusOK
		}
		return out, commandMeta{Status: status, ResourceType: "derivative_intent", ResourceID: v.Outcome.Intent.ID.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedDerivativesPositions200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedDerivativesPositions201JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesPositionsPositionIdClose reduces or closes one of
// the caller's own open positions.
func (s *Server) PostAdvancedDerivativesPositionsPositionIdClose(ctx context.Context, request api.PostAdvancedDerivativesPositionsPositionIdCloseRequestObject) (api.PostAdvancedDerivativesPositionsPositionIdCloseResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	cmd, err := s.derivativeReduceCommand(ctx, request.PositionId.String(), request.Body.Quantity, request.Body.Mode, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, cmd.IdempotencyKey, func(ctx context.Context) (api.DerivativeTradeResult, commandMeta, error) {
		return derivativeTradeCommand(port.Close(ctx, cmd))
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesPositionsPositionIdClose200JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesPositionsPositionIdExercise exercises held American
// options of one of the caller's own positions.
func (s *Server) PostAdvancedDerivativesPositionsPositionIdExercise(ctx context.Context, request api.PostAdvancedDerivativesPositionsPositionIdExerciseRequestObject) (api.PostAdvancedDerivativesPositionsPositionIdExerciseResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	cmd, err := s.derivativeReduceCommand(ctx, request.PositionId.String(), request.Body.Quantity, request.Body.Mode, request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, cmd.IdempotencyKey, func(ctx context.Context) (api.DerivativeTradeResult, commandMeta, error) {
		return derivativeTradeCommand(port.Exercise(ctx, cmd))
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesPositionsPositionIdExercise200JSONResponse(res.Value), nil
}

// derivativeReduceCommand validates a close or exercise and scopes it to the
// position's own account: only its owner may act on it, and anyone else --
// an operator included, as on every write -- gets the NOT_FOUND an absent id
// gets.
func (s *Server) derivativeReduceCommand(ctx context.Context, rawID string, rawQty *api.Quantity, mode api.AdvancedMode, key string) (DerivativeReduceCommand, error) {
	pid, err := parseDerivativePosition(rawID)
	if err != nil {
		return DerivativeReduceCommand{}, err
	}
	var qty *money.Quantity
	if rawQty != nil {
		q, err := parseDerivativeQuantity("quantity", *rawQty)
		if err != nil {
			return DerivativeReduceCommand{}, err
		}
		qty = &q
	}
	if !mode.Valid() {
		return DerivativeReduceCommand{}, validationError("mode", "mode is not an advanced mode")
	}
	account, err := s.opts.Ports.Derivatives.PositionAccount(ctx, pid)
	if err != nil {
		return DerivativeReduceCommand{}, maskDerivativePositionNotFound(err, pid)
	}
	if err := security.RequireAccountOwner(ctx, account.String()); err != nil {
		return DerivativeReduceCommand{}, derivativePositionNotFound(pid)
	}
	return DerivativeReduceCommand{
		PositionID: pid, AccountID: account, Quantity: qty, Mode: gates.Mode(mode), IdempotencyKey: key,
		CorrelationID: observability.CorrelationID(ctx),
	}, nil
}

// derivativeTradeCommand shapes a close or exercise for runCommand.
func derivativeTradeCommand(v DerivativeTradeView, err error) (api.DerivativeTradeResult, commandMeta, error) {
	if err != nil {
		return api.DerivativeTradeResult{}, commandMeta{}, err
	}
	out, err := toAPIDerivativeTrade(v)
	if err != nil {
		return api.DerivativeTradeResult{}, commandMeta{}, err
	}
	return out, commandMeta{Status: http.StatusOK, ResourceType: "derivative_intent", ResourceID: v.Outcome.Intent.ID.String()}, nil
}

// --- the operator's commands ------------------------------------------------

// PostAdvancedDerivativesContractsInstrumentIdMarks records a sandbox mark.
func (s *Server) PostAdvancedDerivativesContractsInstrumentIdMarks(ctx context.Context, request api.PostAdvancedDerivativesContractsInstrumentIdMarksRequestObject) (api.PostAdvancedDerivativesContractsInstrumentIdMarksResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	b := request.Body
	iid, err := parseDerivativeInstrument(request.InstrumentId.String())
	if err != nil {
		return nil, err
	}
	mark, err := parseDerivativePrice("mark", b.Mark)
	if err != nil {
		return nil, err
	}
	cmd := DerivativeMarkCommand{InstrumentID: iid, Mark: mark, Source: b.Source}
	if b.Index != nil {
		idx, err := parseDerivativePrice("index", *b.Index)
		if err != nil {
			return nil, err
		}
		cmd.Index = &idx
	}
	if b.ObservedAt != nil {
		cmd.ObservedAt = b.ObservedAt.UTC()
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.DerivativeMark, commandMeta, error) {
		m, merr := port.RecordMark(ctx, cmd)
		if merr != nil {
			return api.DerivativeMark{}, commandMeta{}, merr
		}
		return toAPIDerivativeMark(m), commandMeta{Status: http.StatusOK, ResourceType: "derivative_mark", ResourceID: m.ID.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesContractsInstrumentIdMarks200JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesContractsInstrumentIdVariationMarginRuns settles one
// variation-margin interval.
func (s *Server) PostAdvancedDerivativesContractsInstrumentIdVariationMarginRuns(ctx context.Context, request api.PostAdvancedDerivativesContractsInstrumentIdVariationMarginRunsRequestObject) (api.PostAdvancedDerivativesContractsInstrumentIdVariationMarginRunsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	cmd, err := derivativeRunCommand(ctx, request.InstrumentId.String(), request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.DerivativeRunResult, commandMeta, error) {
		v, rerr := port.RunVariationMargin(ctx, cmd)
		if rerr != nil {
			return api.DerivativeRunResult{}, commandMeta{}, rerr
		}
		out, cerr := toAPIDerivativeRun(api.DerivativeRunVariationMargin, v)
		return out, commandMeta{Status: http.StatusOK, ResourceType: "variation_margin_run", ResourceID: v.Result.ID}, cerr
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesContractsInstrumentIdVariationMarginRuns200JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesContractsInstrumentIdFundingRuns charges one funding
// interval of a perpetual.
func (s *Server) PostAdvancedDerivativesContractsInstrumentIdFundingRuns(ctx context.Context, request api.PostAdvancedDerivativesContractsInstrumentIdFundingRunsRequestObject) (api.PostAdvancedDerivativesContractsInstrumentIdFundingRunsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	cmd, err := derivativeRunCommand(ctx, request.InstrumentId.String(), request.Body)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.DerivativeRunResult, commandMeta, error) {
		v, rerr := port.RunFunding(ctx, cmd)
		if rerr != nil {
			return api.DerivativeRunResult{}, commandMeta{}, rerr
		}
		out, cerr := toAPIDerivativeRun(api.DerivativeRunFunding, v)
		return out, commandMeta{Status: http.StatusOK, ResourceType: "funding_rate", ResourceID: v.Result.ID}, cerr
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesContractsInstrumentIdFundingRuns200JSONResponse(res.Value), nil
}

func derivativeRunCommand(ctx context.Context, rawID string, body *api.DerivativeRunRequest) (DerivativeRunCommand, error) {
	if body == nil {
		return DerivativeRunCommand{}, validationError("body", "a request body is required")
	}
	iid, err := parseDerivativeInstrument(rawID)
	if err != nil {
		return DerivativeRunCommand{}, err
	}
	if body.IntervalStart.IsZero() {
		return DerivativeRunCommand{}, validationError("interval_start", "interval_start is required")
	}
	return DerivativeRunCommand{InstrumentID: iid, IntervalStart: body.IntervalStart.UTC(), CorrelationID: observability.CorrelationID(ctx)}, nil
}

// PostAdvancedDerivativesContractsInstrumentIdSettlement settles an expired
// future or option once and for all.
func (s *Server) PostAdvancedDerivativesContractsInstrumentIdSettlement(ctx context.Context, request api.PostAdvancedDerivativesContractsInstrumentIdSettlementRequestObject) (api.PostAdvancedDerivativesContractsInstrumentIdSettlementResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	iid, err := parseDerivativeInstrument(request.InstrumentId.String())
	if err != nil {
		return nil, err
	}
	cmd := DerivativeSettleCommand{InstrumentID: iid, PriceSource: deref(request.Body.PriceSource), CorrelationID: observability.CorrelationID(ctx)}
	if request.Body.Price != nil {
		p, err := parseDerivativePrice("price", *request.Body.Price)
		if err != nil {
			return nil, err
		}
		cmd.Price = &p
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.DerivativeSettlementResult, commandMeta, error) {
		v, serr := port.SettleExpiry(ctx, cmd)
		if serr != nil {
			return api.DerivativeSettlementResult{}, commandMeta{}, serr
		}
		out := toAPIDerivativeSettlement(v)
		return out, commandMeta{Status: http.StatusOK, ResourceType: "derivative_settlement", ResourceID: v.Result.ID.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesContractsInstrumentIdSettlement200JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesClearingHouseFunding capitalizes the clearing house.
func (s *Server) PostAdvancedDerivativesClearingHouseFunding(ctx context.Context, request api.PostAdvancedDerivativesClearingHouseFundingRequestObject) (api.PostAdvancedDerivativesClearingHouseFundingResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	asset, err := parseOptionalDerivativeAsset(request.Body.AssetId)
	if err != nil {
		return nil, err
	}
	qty, err := parseDerivativeQuantity("quantity", request.Body.Quantity)
	if err != nil {
		return nil, err
	}
	key := request.Params.IdempotencyKey
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.DerivativeClearingFundingResult, commandMeta, error) {
		v, ferr := port.FundClearingHouse(ctx, DerivativeClearingFundCommand{AssetID: asset, Quantity: qty, IdempotencyKey: key})
		if ferr != nil {
			return api.DerivativeClearingFundingResult{}, commandMeta{}, ferr
		}
		out := api.DerivativeClearingFundingResult{
			ValueDomain: api.SIMULATED, Asset: toAPIDerivativeAsset(v.Asset), Quantity: v.Quantity.String(),
			JournalTransactionIds: journalIDs(v.Journals),
		}
		return out, commandMeta{Status: http.StatusOK, ResourceType: "clearing_house_funding", ResourceID: v.Asset.ID.String()}, nil
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesClearingHouseFunding200JSONResponse(res.Value), nil
}

// PostAdvancedDerivativesClearingHouseReconciliations runs reconciliation
// kind CLEARING for one asset. It is a command because a mismatch opens a
// reconciliation record.
func (s *Server) PostAdvancedDerivativesClearingHouseReconciliations(ctx context.Context, request api.PostAdvancedDerivativesClearingHouseReconciliationsRequestObject) (api.PostAdvancedDerivativesClearingHouseReconciliationsResponseObject, error) {
	port := s.opts.Ports.Derivatives
	if port == nil {
		return nil, errNotWired(derivativesWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	asset, err := parseOptionalDerivativeAsset(request.Body.AssetId)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey, func(ctx context.Context) (api.DerivativeClearingReport, commandMeta, error) {
		v, rerr := port.ReconcileClearing(ctx, asset)
		if rerr != nil {
			return api.DerivativeClearingReport{}, commandMeta{}, rerr
		}
		out, cerr := toAPIDerivativeClearingReport(v)
		return out, commandMeta{Status: http.StatusOK, ResourceType: "clearing_reconciliation", ResourceID: v.Report.ClearingHouseID}, cerr
	})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedDerivativesClearingHouseReconciliations200JSONResponse(res.Value), nil
}

// --- parsing ------------------------------------------------------------------

// derivativeDecimal is DerivativeDecimal of the contract: a non-negative
// decimal written out exactly, at most 38 digits on either side.
var derivativeDecimal = regexp.MustCompile(`^[0-9]{1,38}(\.[0-9]{1,38})?$`)

// parseDerivativePrice reads a decimal exactly as written: its digits are the
// mantissa and its fraction digits the scale. Nothing is rounded here; the
// service rescales it to the contract's price scale exactly or refuses it.
func parseDerivativePrice(field, raw string) (DerivativePriceInput, error) {
	if !derivativeDecimal.MatchString(raw) {
		return DerivativePriceInput{}, validationError(field, field+" must be a non-negative decimal such as 2500.5, written out exactly")
	}
	whole, frac, _ := strings.Cut(raw, ".")
	q, err := money.ParseQuantity(whole + frac)
	if err != nil {
		return DerivativePriceInput{}, validationError(field, field+" is not a representable decimal")
	}
	return DerivativePriceInput{Mantissa: q, Scale: int32(len(frac))}, nil // #nosec G115 -- at most 38 by the pattern
}

func parseDerivativeQuantity(field string, raw api.Quantity) (money.Quantity, error) {
	q, err := money.ParseQuantity(raw)
	if err != nil {
		return money.Quantity{}, validationError(field, field+" must be an integer string of contracts or base units")
	}
	return q, nil
}

func parseDerivativeInstrument(raw string) (instruments.InstrumentID, error) {
	iid, err := instruments.ParseInstrumentID(raw)
	if err != nil || iid.IsZero() {
		return instruments.InstrumentID{}, validationError("instrumentId", "instrumentId must be a canonical UUID")
	}
	return iid, nil
}

func parseDerivativePosition(raw string) (derivatives.PositionID, error) {
	pid, err := derivatives.ParsePositionID(raw)
	if err != nil || pid.IsZero() {
		return derivatives.PositionID{}, validationError("positionId", "positionId must be a canonical UUID")
	}
	return pid, nil
}

func parseOptionalDerivativeAsset(raw *api.UUID) (assets.AssetID, error) {
	if raw == nil {
		return assets.AssetID{}, nil
	}
	id, err := assets.ParseAssetID(raw.String())
	if err != nil || id.IsZero() {
		return assets.AssetID{}, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	return id, nil
}

// derivativePositionNotFound is the one answer for an absent position and for
// a position the caller may not see: the two must be indistinguishable.
func derivativePositionNotFound(pid derivatives.PositionID) *errs.Error {
	return errs.New(errs.CodeNotFound, "derivative position not found").WithField("position_id", pid.String())
}

// maskDerivativePositionNotFound replaces any NOT_FOUND from the port with the
// one canonical answer, so its wording never differs from the masked one.
func maskDerivativePositionNotFound(err error, pid derivatives.PositionID) error {
	if errs.CodeOf(err) == errs.CodeNotFound {
		return derivativePositionNotFound(pid)
	}
	return err
}

// --- shapes -------------------------------------------------------------------

func toAPIDerivativeAsset(a DerivativeAssetView) api.DerivativeAsset {
	return api.DerivativeAsset{Id: toUUID(a.ID), Symbol: a.Symbol, Decimals: int(a.Decimals)}
}

func toAPIDerivativeMark(m derivatives.Mark) api.DerivativeMark {
	out := api.DerivativeMark{
		ValueDomain: api.SIMULATED,
		Id:          toUUID(m.ID), InstrumentId: toUUID(m.InstrumentID), MarkMantissa: m.MarkMantissa.String(), PriceScale: int(m.MarkScale),
		HasIndex: m.HasIndex, Source: m.Source, ObservedAt: m.ObservedAt.UTC(), RecordedAt: m.RecordedAt.UTC(),
	}
	if m.HasIndex {
		idx := m.IndexMantissa
		scale := int(m.IndexScale)
		out.IndexMantissa, out.IndexScale, out.IndexSource = qtyPtr(&idx), &scale, strPtr(m.IndexSource)
	}
	return out
}

func toAPIDerivativeContract(v DerivativeContractView) (api.DerivativeContract, error) {
	if !v.Instrument.Type.IsDerivative() {
		return api.DerivativeContract{}, errs.New(errs.CodeInternal, "a derivative contract view holds a non-derivative instrument")
	}
	base := toAPIAdvancedInstrument(AdvancedInstrumentView{Instrument: v.Instrument, Current: v.Current})
	out := api.DerivativeContract{
		ValueDomain: api.SIMULATED, Id: base.Id, Kind: api.DerivativeKind(v.Instrument.Type), CanonicalName: base.CanonicalName,
		Status: base.Status, RiskClass: base.RiskClass, TermsVersion: base.TermsVersion, TermsHash: base.TermsHash, Terms: base.Terms,
		SettlementAsset: toAPIDerivativeAsset(v.Settlement), Marked: v.LatestMark != nil, Settled: v.Settled != nil,
	}
	if v.LatestMark != nil {
		m := toAPIDerivativeMark(*v.LatestMark)
		out.LatestMark = &m
	}
	if st := v.Settled; st != nil {
		out.Settlement = &api.DerivativeSettlement{
			Id: toUUID(st.ID), Kind: api.DerivativeSettlementKind(st.Kind), PriceMantissa: st.PriceMantissa.String(),
			PriceScale: int(st.PriceScale), PriceSource: st.PriceSource, Positions: st.Positions, SettledAt: st.SettledAt.UTC(),
		}
	}
	return out, nil
}

// toAPIDerivativePosition shapes a position and states, without ambiguity,
// whether it was valued: an open position of a computed answer carries
// exactly one of the service's valuation and its refusal.
func toAPIDerivativePosition(v DerivativePositionView) (api.DerivativePosition, error) {
	p := v.Position
	out := api.DerivativePosition{
		Id: toUUID(p.ID), AccountId: toUUID(p.AccountID), InstrumentId: toUUID(p.InstrumentID), Symbol: v.Symbol,
		Kind: api.DerivativeKind(v.Kind), Status: api.DerivativePositionStatus(p.Status), Quantity: p.Quantity.String(),
		PriceScale: int(v.PriceScale), SettlementAsset: toAPIDerivativeAsset(v.Settlement), CostValue: p.CostValue.String(),
		SettledValue: p.SettledValue.String(), RealizedPnl: p.RealizedPnL.String(), FundingPnl: p.FundingPnL.String(),
		MarginPosted: p.MarginPosted.String(), Shortfall: p.Shortfall.String(), Version: p.Version, OpenedAt: p.OpenedAt.UTC(),
		ClosedAt: timePtr(p.ClosedAt),
	}
	switch {
	case p.Quantity.IsPositive():
		side := api.DerivativeSideLong
		out.Side = &side
	case p.Quantity.IsNegative():
		side := api.DerivativeSideShort
		out.Side = &side
	}
	switch {
	case !v.Computed:
		out.ValuationStatus = api.DerivativeValuationNotComputed
	case p.Status != derivatives.PositionOpen:
		out.ValuationStatus = api.DerivativeValuationNotApplicable
	case v.Valuation != nil:
		r := v.Valuation
		out.ValuationStatus = api.DerivativeValuationValued
		markID := parseUUIDText(r.MarkID)
		if markID == nil || r.PositionID != p.ID.String() {
			return api.DerivativePosition{}, errs.New(errs.CodeInternal, "a position carries another position's valuation")
		}
		out.Valuation = &api.DerivativePositionValuation{
			MarkId: *markID, MarkMantissa: r.Mark.String(), MarkObservedAt: r.MarkObservedAt.UTC(),
			AverageEntryPriceMantissa: r.AverageEntryPrice.String(), UnrealizedPnl: r.UnrealizedPnL.String(),
			EquityAdjustment: r.EquityAdjustment.String(),
			Requirement: api.DerivativeRequirement{
				Notional: r.Requirement.Notional.String(), Initial: r.Requirement.Initial.String(),
				Maintenance: r.Requirement.Maintenance.String(), ScenarioLoss: r.Requirement.ScenarioLoss.String(),
				FundingBuffer: r.Requirement.FundingBuffer.String(),
			},
		}
	case v.Refusal != nil:
		out.ValuationStatus = api.DerivativeValuationRefused
		out.ValuationRefusal = &api.DerivativeRefusal{Code: string(v.Refusal.Code), Detail: v.Refusal.Detail}
	default:
		return api.DerivativePosition{}, errs.New(errs.CodeInternal, "an open position was neither valued nor refused")
	}
	return out, nil
}

func toAPIDerivativeBook(rc derivatives.RiskContribution, quote DerivativeAssetView) api.DerivativeBook {
	return api.DerivativeBook{
		AsOf: rc.AsOf.UTC(), PolicyVersion: rc.PolicyVersion, PolicyHash: rc.PolicyHash, QuoteAsset: toAPIDerivativeAsset(quote),
		GrossNotional: rc.GrossNotional.String(), InitialMargin: rc.InitialMargin.String(),
		MaintenanceMargin: rc.MaintenanceMargin.String(), UnrealizedPnl: rc.UnrealizedPnL.String(),
		EquityAdjustment: rc.EquityAdjustment.String(), MaxInitialBps: int64(rc.MaxInitialBPS), Positions: len(rc.Positions),
	}
}

func toAPIDerivativeFill(f derivatives.FillRecord) api.DerivativeFill {
	out := api.DerivativeFill{
		Id: toUUID(f.ID), PositionId: toUUID(f.PositionID), AccountId: toUUID(f.AccountID), InstrumentId: toUUID(f.InstrumentID),
		Source: api.DerivativeFillSource(f.Source), Side: api.DerivativeFillSide(f.Side), Quantity: f.Quantity.String(),
		PriceMantissa: f.PriceMantissa.String(), PriceScale: int(f.PriceScale), Value: f.Value.String(),
		RealizedPnl: f.RealizedPnL.String(), AmountSettled: f.CashSettled.String(), SlippageBps: int64(f.SlippageBPS),
		IntentId: parseUUIDText(f.IntentID), RiskDecisionId: parseUUIDText(f.RiskDecisionID), OccurredAt: f.OccurredAt.UTC(),
	}
	if f.MarkID != nil {
		out.MarkId = uuidPtr(*f.MarkID)
	}
	return out
}

func toAPIDerivativeTrade(v DerivativeTradeView) (api.DerivativeTradeResult, error) {
	o := v.Outcome
	out := api.DerivativeTradeResult{
		ValueDomain: api.SIMULATED, Intent: toAPIEconomicIntent(o.Intent), Replay: o.Replay,
		JournalTransactionIds: journalIDs(o.Journals),
	}
	if o.DecisionID != nil {
		out.RiskDecisionId = uuidPtr(*o.DecisionID)
	}
	if o.Fill != nil {
		f := toAPIDerivativeFill(*o.Fill)
		out.Fill = &f
	}
	if v.Position != nil {
		p, err := toAPIDerivativePosition(*v.Position)
		if err != nil {
			return api.DerivativeTradeResult{}, err
		}
		out.Position = &p
	}
	if o.ExerciseID != nil {
		out.ExerciseId = uuidPtr(*o.ExerciseID)
	}
	return out, nil
}

// toAPIDerivativeRun shapes a run. On a replay the service writes nothing
// and reports no totals, so none are invented: paid, received and shortfall
// are present only for the run that did the work.
func toAPIDerivativeRun(kind api.DerivativeRunKind, v DerivativeRunView) (api.DerivativeRunResult, error) {
	r := v.Result
	runID := parseUUIDText(r.ID)
	if runID == nil {
		return api.DerivativeRunResult{}, errs.New(errs.CodeInternal, "the run has no identifier")
	}
	out := api.DerivativeRunResult{
		ValueDomain: api.SIMULATED, Kind: kind, RunId: *runID, InstrumentId: toUUID(v.InstrumentID), Replay: r.Replay,
		IntervalStart: r.IntervalStart.UTC(), IntervalEnd: r.IntervalEnd.UTC(), SettlementAsset: toAPIDerivativeAsset(v.Settlement),
		JournalTransactionIds: journalIDs(r.Journals),
	}
	// A funding replay does not report how many positions it covered.
	if kind == api.DerivativeRunVariationMargin || !r.Replay {
		n := r.Positions
		out.Positions = &n
	}
	if !r.Replay {
		out.MarkId = uuidPtr(r.MarkID)
		paid, received, shortfall := r.Paid, r.Received, r.Shortfall
		out.Paid, out.Received, out.Shortfall = qtyPtr(&paid), qtyPtr(&received), qtyPtr(&shortfall)
	}
	if kind == api.DerivativeRunFunding {
		premium, rate, clamped, per := r.PremiumIndexE8, r.RateE8, r.Clamped, r.PerContract
		out.PremiumIndexE8, out.RateE8, out.Clamped, out.PerContractAmount = &premium, &rate, &clamped, qtyPtr(&per)
	}
	return out, nil
}

// toAPIDerivativeSettlement shapes a final settlement; a replay reports what
// the first settlement recorded and no totals.
func toAPIDerivativeSettlement(v DerivativeSettlementView) api.DerivativeSettlementResult {
	r := v.Result
	out := api.DerivativeSettlementResult{
		ValueDomain: api.SIMULATED, SettlementId: toUUID(r.ID), InstrumentId: toUUID(v.InstrumentID),
		Kind: api.DerivativeSettlementKind(r.Kind), Replay: r.Replay, PriceMantissa: r.Price.String(), PriceScale: int(v.PriceScale),
		Positions: r.Positions, ExerciseIds: make([]api.UUID, 0, len(r.Exercises)), SettlementAsset: toAPIDerivativeAsset(v.Settlement),
		JournalTransactionIds: journalIDs(r.Journals),
	}
	for _, e := range r.Exercises {
		out.ExerciseIds = append(out.ExerciseIds, toUUID(e))
	}
	if !r.Replay {
		paid, received, shortfall := r.Paid, r.Received, r.Shortfall
		out.Paid, out.Received, out.Shortfall = qtyPtr(&paid), qtyPtr(&received), qtyPtr(&shortfall)
		if r.Kind == derivatives.SettleOption {
			per := r.IntrinsicPerContract
			out.IntrinsicPerContract = qtyPtr(&per)
		}
	}
	return out
}

func toAPIDerivativeClearingReport(v DerivativeClearingReportView) (api.DerivativeClearingReport, error) {
	r := v.Report
	chID := parseUUIDText(r.ClearingHouseID)
	if chID == nil {
		return api.DerivativeClearingReport{}, errs.New(errs.CodeInternal, "the clearing report names no clearing house")
	}
	out := api.DerivativeClearingReport{
		ValueDomain: api.SIMULATED, ClearingHouseId: *chID, Asset: toAPIDerivativeAsset(v.Asset),
		Holdings: r.ClearingCash.String(), Capital: r.ClearingCapital.String(), Result: r.ClearingResult.String(),
		RegisterNet: r.RegisterNet.String(), RegisterShortfall: r.RegisterShortfall.String(), Flows: r.Flows,
		ExpectedResult: r.ExpectedResult.String(), Difference: r.Difference.String(),
		NetOpenInterest:   make([]api.DerivativeOpenInterest, 0, len(r.NetOpenInterest)),
		InternallyMatched: r.InternallyMatched, Matched: r.Matched, RecordId: parseUUIDText(r.RecordID),
	}
	ids := make([]string, 0, len(r.NetOpenInterest))
	for id := range r.NetOpenInterest {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		iid := parseUUIDText(id)
		if iid == nil {
			return api.DerivativeClearingReport{}, errs.New(errs.CodeInternal, "open interest names no instrument")
		}
		out.NetOpenInterest = append(out.NetOpenInterest, api.DerivativeOpenInterest{InstrumentId: *iid, Quantity: r.NetOpenInterest[id].String()})
	}
	return out, nil
}

func journalIDs(in []ledger.TransactionID) []api.UUID {
	out := make([]api.UUID, 0, len(in))
	for _, j := range in {
		out = append(out, toUUID(j))
	}
	return out
}

func journalPtr(j *ledger.TransactionID) *api.UUID {
	if j == nil {
		return nil
	}
	return uuidPtr(*j)
}
