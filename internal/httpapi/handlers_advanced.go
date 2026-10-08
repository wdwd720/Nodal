package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"

	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/security"
)

// GetAdvancedCapabilities lists every advanced capability and whether each of
// its modes may run in this deployment. LIVE is always refused.
func (s *Server) GetAdvancedCapabilities(ctx context.Context, _ api.GetAdvancedCapabilitiesRequestObject) (api.GetAdvancedCapabilitiesResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("advanced capabilities")
	}
	env, views, err := s.opts.Ports.Advanced.Capabilities(ctx)
	if err != nil {
		return nil, err
	}
	out := api.AdvancedCapabilityList{Environment: env, ValueDomain: api.SIMULATED, Items: make([]api.AdvancedCapability, 0, len(views))}
	for _, v := range views {
		sp := v.Spec
		c := api.AdvancedCapability{
			Capability: string(sp.Capability), HighRisk: gates.IsHighRisk(sp.Capability),
			UngatedEnvironments: append([]string{}, sp.UngatedEnvironments...), LiveImplemented: sp.LiveImplemented,
			LiveEvidence: append([]string{}, sp.LiveEvidence...), MinApprovers: sp.MinApprovers,
			MaxLiveValiditySeconds: int64(sp.MaxLiveValidity.Seconds()), MaxLiveCapital: sp.MaxLiveCapital.String(),
			Jurisdictions: append([]string{}, sp.Jurisdictions...), ProviderReadiness: sp.ProviderReadiness,
			GateActive: v.GateActive, KillSwitched: v.KillSwitched,
			SimulatedModes: make([]api.AdvancedMode, 0, len(sp.SimulatedModes)),
		}
		for _, m := range sp.SimulatedModes {
			c.SimulatedModes = append(c.SimulatedModes, api.AdvancedMode(m))
		}
		// Every mode is reported, LIVE included, in gates.AllModes order: a
		// mode the build does not implement is shown refused with its reason
		// rather than left out, so the refusal itself is visible.
		for _, m := range gates.AllModes() {
			d, ok := v.Decisions[m]
			if !ok {
				return nil, errs.New(errs.CodeInternal, "capability state is missing a mode decision").
					WithField("capability", string(sp.Capability)).WithField("mode", string(m))
			}
			c.Decisions = append(c.Decisions, api.AdvancedModeDecision{
				Mode: api.AdvancedMode(m), Allowed: d.Allowed, Reasons: append([]string{}, d.Reasons...),
			})
		}
		out.Items = append(out.Items, c)
	}
	return api.GetAdvancedCapabilities200JSONResponse(out), nil
}

// GetAdvancedInstruments lists canonical instruments with their current terms.
func (s *Server) GetAdvancedInstruments(ctx context.Context, request api.GetAdvancedInstrumentsRequestObject) (api.GetAdvancedInstrumentsResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("advanced instruments")
	}
	var kind instruments.InstrumentType
	if request.Params.Kind != nil {
		kind = instruments.InstrumentType(*request.Params.Kind)
		if !kind.Valid() {
			return nil, validationError("kind", "unknown instrument kind")
		}
	}
	views, err := s.opts.Ports.Advanced.Instruments(ctx, kind, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.AdvancedInstrumentList{Items: make([]api.AdvancedInstrument, 0, len(views))}
	for _, v := range views {
		out.Items = append(out.Items, toAPIAdvancedInstrument(v))
	}
	return api.GetAdvancedInstruments200JSONResponse(out), nil
}

// GetAdvancedInstrumentsInstrumentId returns one instrument with its terms
// history.
func (s *Server) GetAdvancedInstrumentsInstrumentId(ctx context.Context, request api.GetAdvancedInstrumentsInstrumentIdRequestObject) (api.GetAdvancedInstrumentsInstrumentIdResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("advanced instruments")
	}
	iid, err := instruments.ParseInstrumentID(request.InstrumentId.String())
	if err != nil {
		return nil, validationError("instrumentId", "instrumentId must be a canonical UUID")
	}
	v, err := s.opts.Ports.Advanced.Instrument(ctx, iid)
	if err != nil {
		return nil, err
	}
	base := toAPIAdvancedInstrument(v)
	detail := api.AdvancedInstrumentDetail{
		Id: base.Id, Kind: base.Kind, CanonicalName: base.CanonicalName, Status: base.Status, RiskClass: base.RiskClass,
		TermsVersion: base.TermsVersion, TermsHash: base.TermsHash, Terms: base.Terms,
		Versions: make([]api.InstrumentTermsVersion, 0, len(v.History)),
	}
	for _, h := range v.History {
		detail.Versions = append(detail.Versions, api.InstrumentTermsVersion{
			Version: h.Version, TermsHash: hex.EncodeToString(h.ContentHash), Reason: h.Reason, ActorType: h.ActorType,
			ActorId: h.ActorID, CreatedAt: h.CreatedAt,
		})
	}
	return api.GetAdvancedInstrumentsInstrumentId200JSONResponse(detail), nil
}

// GetAdvancedIntents pages an account's economic intents.
func (s *Server) GetAdvancedIntents(ctx context.Context, request api.GetAdvancedIntentsRequestObject) (api.GetAdvancedIntentsResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("economic intents")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	items, next, err := s.opts.Ports.Advanced.Intents(ctx, account, cursor, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.EconomicIntentPage{ValueDomain: api.SIMULATED, Items: make([]api.EconomicIntent, 0, len(items)), NextCursor: nextCursor(next)}
	for _, in := range items {
		page.Items = append(page.Items, toAPIEconomicIntent(in))
	}
	return api.GetAdvancedIntents200JSONResponse(page), nil
}

// GetAdvancedIntentsIntentId returns one intent to its account's owner (or an
// operator who may read any account). An intent of another account answers
// NOT_FOUND, never FORBIDDEN, so its existence does not leak.
func (s *Server) GetAdvancedIntentsIntentId(ctx context.Context, request api.GetAdvancedIntentsIntentIdRequestObject) (api.GetAdvancedIntentsIntentIdResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("economic intents")
	}
	iid, err := econ.ParseIntentID(request.IntentId.String())
	if err != nil {
		return nil, validationError("intentId", "intentId must be a canonical UUID")
	}
	in, err := s.opts.Ports.Advanced.Intent(ctx, iid)
	if err != nil {
		return nil, err
	}
	// NOT_FOUND rather than FORBIDDEN: a distinguishable refusal would tell a
	// stranger that the id exists (F-41).
	if err := security.RequireAccount(ctx, in.AccountID.String()); err != nil {
		return nil, errs.New(errs.CodeNotFound, "no such economic intent").WithField("intent_id", iid.String())
	}
	return api.GetAdvancedIntentsIntentId200JSONResponse(toAPIEconomicIntent(in)), nil
}

// GetAdvancedEffects pages an account's economic effects.
func (s *Server) GetAdvancedEffects(ctx context.Context, request api.GetAdvancedEffectsRequestObject) (api.GetAdvancedEffectsResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("economic effects")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	items, next, err := s.opts.Ports.Advanced.Effects(ctx, account, cursor, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	page := api.EconomicEffectPage{Items: make([]api.EconomicEffect, 0, len(items)), NextCursor: nextCursor(next)}
	for _, e := range items {
		page.Items = append(page.Items, toAPIEconomicEffect(e))
	}
	return api.GetAdvancedEffects200JSONResponse(page), nil
}

// GetAdvancedRiskDecisions lists an account's Risk Kernel V2 decisions.
func (s *Server) GetAdvancedRiskDecisions(ctx context.Context, request api.GetAdvancedRiskDecisionsRequestObject) (api.GetAdvancedRiskDecisionsResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("advanced risk decisions")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	recs, err := s.opts.Ports.Advanced.RiskDecisions(ctx, account, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.AdvancedRiskDecisionList{Items: make([]api.AdvancedRiskDecision, 0, len(recs))}
	for _, r := range recs {
		codes := append([]string{}, r.ReasonCodes...)
		out.Items = append(out.Items, api.AdvancedRiskDecision{
			Id: toUUID(r.ID), IntentId: parseUUIDText(r.IntentID), IntentKind: r.IntentKind, Stage: string(r.Stage),
			Verdict: api.AdvancedRiskDecisionVerdict(r.Verdict), ReasonCodes: codes, InputHash: r.InputHash,
			PolicyVersion: r.PolicyVersion, PolicyHash: r.PolicyHash, EvaluatorVersion: r.EvaluatorVersion,
			DecisionHash: r.DecisionHash, EvaluatedAt: r.EvaluatedAt,
		})
	}
	return api.GetAdvancedRiskDecisions200JSONResponse(out), nil
}

// GetAdvancedBuyingPower returns buying power V2 with its explanation.
func (s *Server) GetAdvancedBuyingPower(ctx context.Context, request api.GetAdvancedBuyingPowerRequestObject) (api.GetAdvancedBuyingPowerResponseObject, error) {
	if s.opts.Ports.Advanced == nil {
		return nil, errNotWired("advanced buying power")
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	res, err := s.opts.Ports.Advanced.BuyingPower(ctx, account)
	if err != nil {
		return nil, err
	}
	bp := res.V2
	out := api.BuyingPowerV2{
		ValueDomain: api.SIMULATED, QuoteAssetId: toUUID(res.Quote.ID), QuoteSymbol: res.Quote.Symbol, GrossEquity: bp.GrossEquity.String(), EligibleCollateral: bp.EligibleCollateral.String(),
		Haircuts: bp.Haircuts.String(), BorrowLiabilities: bp.BorrowLiabilities.String(), InitialMarginUsed: bp.InitialMarginUsed.String(),
		MaintenanceMargin: bp.MaintenanceMargin.String(), OpenOrderReservations: bp.OpenOrderReservations.String(),
		PendingTransferReservations: bp.PendingTransferReservations.String(), PendingWithdrawalReservations: bp.PendingWithdrawalReservations.String(),
		StrategyReservations: bp.StrategyReservations.String(), PendingBridgeHaircut: bp.PendingBridgeHaircut.String(),
		EligibleEquity: bp.EligibleEquity.String(), MarginAvailable: bp.MarginAvailable.String(), Withdrawable: bp.Withdrawable.String(),
		TradeBuyingPower: bp.TradeBuyingPower.String(), RiskIncreasingBuyingPower: bp.RiskIncreasingBuyingPower.String(),
		PolicyVersion: bp.PolicyVersion, AsOf: bp.AsOf,
		Reasons: make([]api.BuyingPowerV2Reason, 0, len(bp.Reasons)), Lines: make([]api.BuyingPowerV2Line, 0, len(bp.Lines)),
	}
	for _, r := range bp.Reasons {
		out.Reasons = append(out.Reasons, api.BuyingPowerV2Reason{Code: r.Code, Detail: r.Detail, Amount: r.Amount.String()})
	}
	for _, l := range bp.Lines {
		out.Lines = append(out.Lines, api.BuyingPowerV2Line{Field: l.Field, Component: l.Component, AssetId: strPtr(l.AssetID),
			AmountUsd: l.AmountUSD.String(), Detail: l.Detail})
	}
	return api.GetAdvancedBuyingPower200JSONResponse(out), nil
}

func toAPIAdvancedInstrument(v AdvancedInstrumentView) api.AdvancedInstrument {
	t := v.Current.Terms
	terms := api.InstrumentTerms{
		Kind: api.InstrumentKind(t.Kind), Symbol: t.Symbol, VenueCode: t.VenueCode, ExchangeMic: strPtr(t.ExchangeMIC),
		SettlementAssetId: toUUID(t.SettlementAssetID), ContractMultiplier: t.ContractMultiplier.String(),
		PriceScale: int(t.PriceScale), TickSize: t.TickSize.String(), LotSize: t.LotSize.String(),
		MinQuantity: t.MinQuantity.String(), QuantityDecimals: int(t.QuantityDecimals), Expiry: timePtr(t.Expiry),
		SettlementStyle: api.InstrumentTermsSettlementStyle(t.SettlementStyle), TradingCalendar: t.TradingCalendar,
		JurisdictionTags: append([]string{}, t.JurisdictionTags...), OracleMaxAgeMs: t.OracleMaxAgeMS,
		OracleSources: append([]string{}, t.OracleSources...), MarginModel: api.InstrumentTermsMarginModel(t.MarginModel),
		CorporateActionBehavior: api.InstrumentTermsCorporateActionBehavior(t.CorporateActionBehavior),
		Transferability:         api.InstrumentTermsTransferability(t.Transferability),
	}
	if t.BaseAssetID != nil {
		terms.BaseAssetId = uuidPtr(*t.BaseAssetID)
	}
	if t.QuoteAssetID != nil {
		terms.QuoteAssetId = uuidPtr(*t.QuoteAssetID)
	}
	if t.UnderlyingInstrumentID != nil {
		terms.UnderlyingInstrumentId = uuidPtr(*t.UnderlyingInstrumentID)
	}
	terms.StrikeMantissa = qtyPtr(t.StrikeMantissa)
	if t.OptionRight != "" {
		r := api.InstrumentTermsOptionRight(t.OptionRight)
		terms.OptionRight = &r
	}
	if t.ExerciseStyle != "" {
		e := api.InstrumentTermsExerciseStyle(t.ExerciseStyle)
		terms.ExerciseStyle = &e
	}
	if t.FundingIntervalMS != 0 {
		f := t.FundingIntervalMS
		terms.FundingIntervalMs = &f
	}
	if len(t.Outcomes) > 0 {
		o := append([]string{}, t.Outcomes...)
		terms.Outcomes = &o
	}
	return api.AdvancedInstrument{
		Id: toUUID(v.Instrument.ID), Kind: api.InstrumentKind(v.Instrument.Type), CanonicalName: v.Instrument.CanonicalName,
		Status: string(v.Instrument.Status), RiskClass: string(v.Instrument.RiskClass),
		TermsVersion: v.Current.Version, TermsHash: hex.EncodeToString(v.Current.ContentHash), Terms: terms,
	}
}

func toAPIEconomicIntent(in econ.Intent) api.EconomicIntent {
	terms := map[string]interface{}{}
	if len(in.Terms) > 0 {
		// Terms are stored as a validated JSON object; a decode failure here
		// would mean the row was not written by econ, and the field is then
		// returned empty rather than guessed at.
		_ = json.Unmarshal(in.Terms, &terms)
	}
	out := api.EconomicIntent{
		Id: toUUID(in.ID), AccountId: toUUID(in.AccountID), Kind: string(in.Kind), Mode: api.AdvancedMode(in.Mode),
		Capability: string(in.Capability), ActorType: string(in.ActorType), AgentId: parseUUIDText(in.AgentID),
		StrategyRef: strPtr(in.StrategyRef), InstrumentId: parseUUIDText(in.InstrumentID), Quantity: qtyPtr(in.Quantity),
		NotionalUsd: usdPtr(in.NotionalUSD), Terms: terms, ContentHash: hex.EncodeToString(in.ContentHash),
		Status: string(in.Status), RejectionCode: strPtr(in.RejectionCode), CreatedAt: in.CreatedAt, UpdatedAt: in.UpdatedAt,
		TerminalAt: timePtr(in.TerminalAt),
	}
	if in.AssetID != nil {
		out.AssetId = uuidPtr(*in.AssetID)
	}
	return out
}

func toAPIEconomicEffect(e econ.Effect) api.EconomicEffect {
	out := api.EconomicEffect{
		Id: toUUID(e.ID), EffectKey: e.Key, Subsystem: string(e.Subsystem), EffectKind: e.Kind,
		AccountId: parseUUIDText(e.AccountID), PostsLedger: e.PostsLedger, Status: api.EconomicEffectStatus(e.Status),
		ExternalRef: strPtr(e.ExternalRef), EvidenceRef: strPtr(e.EvidenceRef), FailureCode: strPtr(e.FailureCode),
		CreatedAt: e.CreatedAt, UpdatedAt: e.UpdatedAt,
	}
	if e.IntentID != nil {
		out.IntentId = uuidPtr(*e.IntentID)
	}
	if e.JournalTransactionID != nil {
		out.JournalTransactionId = uuidPtr(*e.JournalTransactionID)
	}
	if e.CompensationJournalTransactionID != nil {
		out.CompensationJournalTransactionId = uuidPtr(*e.CompensationJournalTransactionID)
	}
	if len(e.EvidenceSHA256) > 0 {
		h := hex.EncodeToString(e.EvidenceSHA256)
		out.EvidenceSha256 = &h
	}
	return out
}
