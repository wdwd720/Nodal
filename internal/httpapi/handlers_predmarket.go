package httpapi

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/econ"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gates"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/predmarket"
	"github.com/nodal/controlplane/internal/reconciliation"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/valuedomain"
)

// Prediction markets (internal/predmarket). These handlers translate shapes
// and nothing else: the service decides every lifecycle step, every
// refusal and every dual-control rule (the creator never resolves or voids,
// a disputer and the disputed resolution's author never rule, a void needs a
// second operator's approval, an observation's recorder never resolves), and
// its errors reach the client with their codes.
//
// Enum values are written as conversions from the service's own strings, never
// as the generated constant names: oapi-codegen renames those whenever another
// schema gains the same value, and a merge must not be able to break this file.

const predmarketWhat = "prediction markets"

// predmarketDomain is the value domain every response here reports.
var predmarketDomain = api.ValueDomainSimulated(valuedomain.Simulated)

// predmarketIntentKey is the economic intent's idempotency key for a client's
// Idempotency-Key. Economic intents are unique per (account, key) across every
// subsystem, so a client key is namespaced: it can never collide with another
// subsystem's intent, or with the keys the service derives for settlement.
// The same client key retried after the HTTP record has expired still finds
// the first intent, so a set is never issued twice.
func predmarketIntentKey(action, key string) string {
	return "http:predmarket:" + action + ":" + key
}

func parseMarketID(u api.UUID) (predmarket.MarketID, error) {
	id, err := predmarket.ParseMarketID(u.String())
	if err != nil || id.IsZero() {
		return predmarket.MarketID{}, validationError("marketId", "marketId must be a canonical UUID")
	}
	return id, nil
}

// predmarketActor is the authenticated principal as the service's actor. The
// service, not this layer, decides whether that actor may take the action: an
// operator action refuses anyone who is not an operator, and dual control
// compares actor ids.
func predmarketActor(ctx context.Context) (econ.Actor, error) {
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return econ.Actor{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return econ.Actor{Type: p.ActorType, ID: p.SubjectID}, nil
}

// evidenceOf passes free-form evidence through, and a missing document as no
// evidence at all: a nil map inside an interface would not compare equal to
// nil, and the service's "a dispute carries its evidence" check would pass on
// a document that was never sent.
func evidenceOf(m map[string]interface{}) any {
	if m == nil {
		return nil
	}
	return m
}

// --- reads -------------------------------------------------------------------

// GetAdvancedPredictionMarkets lists the markets in one lifecycle status.
func (s *Server) GetAdvancedPredictionMarkets(ctx context.Context, request api.GetAdvancedPredictionMarketsRequestObject) (api.GetAdvancedPredictionMarketsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	status := predmarket.MarketStatus(request.Params.Status)
	if !status.Valid() {
		return nil, validationError("status", "status must be one of DRAFT, OPEN, CLOSED, RESOLVING, RESOLVED, DISPUTED, SETTLED, VOIDED")
	}
	recs, err := s.opts.Ports.PredictionMarkets.Markets(ctx, status, pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	out := api.PredictionMarketList{
		ValueDomain: predmarketDomain, Status: api.PredictionMarketStatus(status),
		Items: make([]api.PredictionMarket, 0, len(recs)),
	}
	for _, r := range recs {
		out.Items = append(out.Items, toAPIPredictionMarket(r))
	}
	return api.GetAdvancedPredictionMarkets200JSONResponse(out), nil
}

// GetAdvancedPredictionMarketsOracle names the oracle this deployment resolves
// markets from. Without one it answers PROVIDER_UNAVAILABLE.
func (s *Server) GetAdvancedPredictionMarketsOracle(ctx context.Context, _ api.GetAdvancedPredictionMarketsOracleRequestObject) (api.GetAdvancedPredictionMarketsOracleResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	v, err := s.opts.Ports.PredictionMarkets.Oracle(ctx)
	if err != nil {
		return nil, err
	}
	return api.GetAdvancedPredictionMarketsOracle200JSONResponse(api.PredictionOracle{
		Kind: api.PredictionOracleKind("SANDBOX"), Source: v.Source, PublicKey: hex.EncodeToString(v.PublicKey),
	}), nil
}

// GetAdvancedPredictionMarketsMarketId returns one market with its resolution
// history, disputes and settlement record. A market is reference data: it
// names no account.
func (s *Server) GetAdvancedPredictionMarketsMarketId(ctx context.Context, request api.GetAdvancedPredictionMarketsMarketIdRequestObject) (api.GetAdvancedPredictionMarketsMarketIdResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.PredictionMarkets.Market(ctx, id)
	if err != nil {
		return nil, err
	}
	m := v.Record.Market
	out := api.PredictionMarketDetail{
		ValueDomain: predmarketDomain, Market: toAPIPredictionMarket(v.Record),
		Resolutions:   make([]api.PredictionResolution, 0, len(v.Resolutions)),
		Disputes:      make([]api.PredictionDispute, 0, len(v.Disputes)),
		VoidProposals: make([]api.PredictionVoidProposal, 0, len(v.VoidProposals)),
	}
	for _, r := range v.Resolutions {
		out.Resolutions = append(out.Resolutions, toAPIPredictionResolution(m, r))
	}
	for _, d := range v.Disputes {
		out.Disputes = append(out.Disputes, toAPIPredictionDispute(d))
	}
	for _, p := range v.VoidProposals {
		out.VoidProposals = append(out.VoidProposals, toAPIPredictionVoidProposal(p))
	}
	if v.Settlement != nil {
		st := toAPIPredictionSettlement(m, *v.Settlement)
		out.Settlement = &st
	}
	return api.GetAdvancedPredictionMarketsMarketId200JSONResponse(out), nil
}

// GetAdvancedPredictionMarketsMarketIdBooks returns the market's escrow books
// from one snapshot and whether they satisfy the escrow identity.
func (s *Server) GetAdvancedPredictionMarketsMarketIdBooks(ctx context.Context, request api.GetAdvancedPredictionMarketsMarketIdBooksRequestObject) (api.GetAdvancedPredictionMarketsMarketIdBooksResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.PredictionMarkets.Books(ctx, id)
	if err != nil {
		return nil, err
	}
	m, b := v.Record.Market, v.Books
	if len(b.SharesIssued) != len(m.Outcomes) || len(b.SharesOutstanding) != len(m.Outcomes) || len(b.HeldByCustomers) != len(m.Outcomes) {
		return nil, errs.New(errs.CodeInternal, "the escrow books do not name every outcome of the market").
			WithField("market_id", m.ID.String())
	}
	out := api.PredictionEscrowBooks{
		ValueDomain: predmarketDomain, MarketId: toUUID(m.ID), MarketStatus: api.PredictionMarketStatus(m.Status),
		CollateralAssetId: toUUID(m.CollateralAssetID), CollateralSymbol: v.Record.Collateral.Symbol,
		CollateralDecimals: int(v.Record.Collateral.Decimals), ShareDecimals: int(predmarket.ShareDecimals),
		Escrow: b.Escrow.String(), Collateral: b.Collateral.String(), Residual: v.Residual.String(), Held: b.Held.String(),
		Outcomes:   make([]api.PredictionBookOutcome, 0, len(m.Outcomes)),
		Reconciles: len(v.Violations) == 0, Violations: append([]string{}, v.Violations...),
	}
	for i, o := range m.Outcomes {
		out.Outcomes = append(out.Outcomes, api.PredictionBookOutcome{
			Index: o.Index, Symbol: o.Symbol, ShareAssetId: toUUID(o.ShareAssetID),
			SharesIssued: b.SharesIssued[i].String(), SharesOutstanding: b.SharesOutstanding[i].String(),
			HeldByCustomers: b.HeldByCustomers[i].String(),
		})
	}
	return api.GetAdvancedPredictionMarketsMarketIdBooks200JSONResponse(out), nil
}

// GetAdvancedPredictionMarketsMarketIdPositions returns one account's outcome
// shares in a market: the owner's, or any account's for an operator holding
// account:read_any. The account is scoped before the market is looked up.
func (s *Server) GetAdvancedPredictionMarketsMarketIdPositions(ctx context.Context, request api.GetAdvancedPredictionMarketsMarketIdPositionsRequestObject) (api.GetAdvancedPredictionMarketsMarketIdPositionsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	account, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	v, err := s.opts.Ports.PredictionMarkets.Positions(ctx, id, account)
	if err != nil {
		return nil, err
	}
	out := api.PredictionPositionList{
		ValueDomain: predmarketDomain, MarketId: toUUID(v.Record.Market.ID), AccountId: toUUID(account),
		ShareDecimals: int(predmarket.ShareDecimals), Items: make([]api.PredictionPosition, 0, len(v.Positions)),
	}
	for _, p := range v.Positions {
		if p.AccountID != account {
			// The port answers for one account; a row of another would be a
			// fault, and is refused rather than shown.
			return nil, errs.New(errs.CodeInternal, "the positions read returned another account's shares")
		}
		out.Items = append(out.Items, api.PredictionPosition{
			OutcomeIndex: p.OutcomeIndex, OutcomeSymbol: p.OutcomeSymbol, ShareAssetId: toUUID(p.ShareAssetID),
			Quantity: p.Quantity.String(), AsOf: p.AsOf.UTC(),
		})
	}
	if v.HeldPayout != nil {
		if v.HeldPayout.AccountID != account {
			return nil, errs.New(errs.CodeInternal, "the positions read returned another account's held payout")
		}
		h := toAPIPredictionHeldPayout(*v.HeldPayout)
		out.HeldPayout = &h
	}
	return api.GetAdvancedPredictionMarketsMarketIdPositions200JSONResponse(out), nil
}

// --- the customer's complete sets -----------------------------------------------

// predmarketSetCommand builds the service request for an issuance or a
// redemption on an account the handler has already scoped to its owner.
func predmarketSetCommand(ctx context.Context, account accounts.AccountID, rawMarket api.UUID, quantity, action, key string) (predmarket.PositionRequest, error) {
	id, err := parseMarketID(rawMarket)
	if err != nil {
		return predmarket.PositionRequest{}, err
	}
	q, err := money.ParseQuantity(quantity)
	if err != nil {
		return predmarket.PositionRequest{}, validationError("quantity", "quantity must be an integer string of share base units")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return predmarket.PositionRequest{}, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	return predmarket.PositionRequest{
		MarketID: id, AccountID: account, Quantity: q, IdempotencyKey: predmarketIntentKey(action, key),
		ActorType: p.ActorType, ActorID: p.SubjectID, CorrelationID: observability.CorrelationID(ctx),
	}, nil
}

// PostAdvancedPredictionMarketsMarketIdIssuances issues complete sets on the
// caller's own account.
func (s *Server) PostAdvancedPredictionMarketsMarketIdIssuances(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdIssuancesRequestObject) (api.PostAdvancedPredictionMarketsMarketIdIssuancesResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	// Ownership only: an operator's account:read_any never lets them issue
	// sets on a customer's account.
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := predmarketSetCommand(ctx, account, request.MarketId, request.Body.Quantity, "issue", request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionSetResult, commandMeta, error) {
			r, cerr := s.opts.Ports.PredictionMarkets.Issue(ctx, req)
			if cerr != nil {
				return api.PredictionSetResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionSetResult(req, "ISSUE", r), commandMeta{
				Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.Intent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdIssuances200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdRedemptions redeems complete sets on
// the caller's own account.
func (s *Server) PostAdvancedPredictionMarketsMarketIdRedemptions(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdRedemptionsRequestObject) (api.PostAdvancedPredictionMarketsMarketIdRedemptionsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	req, err := predmarketSetCommand(ctx, account, request.MarketId, request.Body.Quantity, "redeem", request.Params.IdempotencyKey)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionSetResult, commandMeta, error) {
			r, cerr := s.opts.Ports.PredictionMarkets.Redeem(ctx, req)
			if cerr != nil {
				return api.PredictionSetResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionSetResult(req, "REDEEM", r), commandMeta{
				Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.Intent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdRedemptions200JSONResponse(res.Value), nil
}

// --- the operator's lifecycle ---------------------------------------------------

// maxDisputeWindowMillis is the largest millisecond count a time.Duration can
// hold. Anything beyond it is refused here rather than wrapped into a negative
// duration; the service decides what range is acceptable.
const maxDisputeWindowMillis = math.MaxInt64 / int64(time.Millisecond)

// PostAdvancedPredictionMarkets defines a market in DRAFT, collateralised in
// the sandbox dollar. The same definition under the same symbol is a replay.
func (s *Server) PostAdvancedPredictionMarkets(ctx context.Context, request api.PostAdvancedPredictionMarketsRequestObject) (api.PostAdvancedPredictionMarketsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	body := request.Body
	if body == nil {
		return nil, validationError("body", "a request body is required")
	}
	pub, err := hex.DecodeString(body.OraclePublicKey)
	if err != nil {
		return nil, validationError("oracle_public_key", "oracle_public_key must be the oracle's Ed25519 public key in hex")
	}
	if body.DisputeWindowMs > maxDisputeWindowMillis || body.DisputeWindowMs < -maxDisputeWindowMillis {
		return nil, validationError("dispute_window_ms", "dispute_window_ms is out of range")
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	spec := predmarket.MarketSpec{
		Symbol: body.Symbol, Question: body.Question, Outcomes: append([]string(nil), body.Outcomes...),
		// The collateral is not the client's choice: every market of this API
		// is collateralised in the platform's one sandbox dollar.
		CollateralAssetID: econ.SandboxDollarID(),
		Mode:              gates.Mode(body.Mode),
		OpenAt:            body.OpenAt, CloseAt: body.CloseAt,
		DisputeWindow: time.Duration(body.DisputeWindowMs) * time.Millisecond,
		OracleSource:  body.OracleSource, OraclePublicKey: pub,
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionMarket, commandMeta, error) {
			rec, existing, cerr := s.opts.Ports.PredictionMarkets.Create(ctx, spec, actor)
			if cerr != nil {
				return api.PredictionMarket{}, commandMeta{}, cerr
			}
			status := http.StatusCreated
			if existing {
				status = http.StatusOK
			}
			return toAPIPredictionMarket(rec), commandMeta{
				Status: status, ResourceType: "prediction_market", ResourceID: rec.Market.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed || res.Meta.Status == http.StatusOK {
		return api.PostAdvancedPredictionMarkets200JSONResponse(res.Value), nil
	}
	return api.PostAdvancedPredictionMarkets201JSONResponse(res.Value), nil
}

// marketStep runs one lifecycle step that answers with the market.
func (s *Server) marketStep(ctx context.Context, rawMarket api.UUID, key string,
	step func(ctx context.Context, id predmarket.MarketID, by econ.Actor) (PredictionMarketRecord, error)) (api.PredictionMarket, error) {
	id, err := parseMarketID(rawMarket)
	if err != nil {
		return api.PredictionMarket{}, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return api.PredictionMarket{}, err
	}
	res, err := runCommand(ctx, s, key, func(ctx context.Context) (api.PredictionMarket, commandMeta, error) {
		rec, cerr := step(ctx, id, actor)
		if cerr != nil {
			return api.PredictionMarket{}, commandMeta{}, cerr
		}
		return toAPIPredictionMarket(rec), commandMeta{
			Status: http.StatusOK, ResourceType: "prediction_market", ResourceID: rec.Market.ID.String(),
		}, nil
	})
	if err != nil {
		return api.PredictionMarket{}, err
	}
	return res.Value, nil
}

// PostAdvancedPredictionMarketsMarketIdOpen opens a DRAFT market.
func (s *Server) PostAdvancedPredictionMarketsMarketIdOpen(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdOpenRequestObject) (api.PostAdvancedPredictionMarketsMarketIdOpenResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	out, err := s.marketStep(ctx, request.MarketId, request.Params.IdempotencyKey, s.opts.Ports.PredictionMarkets.Open)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdOpen200JSONResponse(out), nil
}

// PostAdvancedPredictionMarketsMarketIdClose closes an OPEN market.
func (s *Server) PostAdvancedPredictionMarketsMarketIdClose(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdCloseRequestObject) (api.PostAdvancedPredictionMarketsMarketIdCloseResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	out, err := s.marketStep(ctx, request.MarketId, request.Params.IdempotencyKey, s.opts.Ports.PredictionMarkets.Close)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdClose200JSONResponse(out), nil
}

// PostAdvancedPredictionMarketsMarketIdRequestResolution moves a CLOSED market
// to RESOLVING.
func (s *Server) PostAdvancedPredictionMarketsMarketIdRequestResolution(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdRequestResolutionRequestObject) (api.PostAdvancedPredictionMarketsMarketIdRequestResolutionResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	out, err := s.marketStep(ctx, request.MarketId, request.Params.IdempotencyKey, s.opts.Ports.PredictionMarkets.RequestResolution)
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdRequestResolution200JSONResponse(out), nil
}

// PostAdvancedPredictionMarketsMarketIdOracleObservations records, as the
// requesting operator, what the sandbox oracle observed for a market, and
// returns the signed report the service stored.
func (s *Server) PostAdvancedPredictionMarketsMarketIdOracleObservations(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdOracleObservationsRequestObject) (api.PostAdvancedPredictionMarketsMarketIdOracleObservationsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	body := request.Body
	if body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	req := predmarket.ObservationRequest{
		MarketID: id, Outcome: body.Outcome, ObservedAt: body.ObservedAt, Evidence: []byte(body.Evidence), Actor: actor,
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionOracleObservation, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.Observe(ctx, req)
			if cerr != nil {
				return api.PredictionOracleObservation{}, commandMeta{}, cerr
			}
			o := v.Observation
			wire, cerr := toAPIOracleReport(o.Report)
			if cerr != nil {
				return api.PredictionOracleObservation{}, commandMeta{}, cerr
			}
			return api.PredictionOracleObservation{
				ValueDomain: predmarketDomain, ObservationId: toUUID(o.ID), Report: wire, ReportHash: hex.EncodeToString(o.ReportHash),
				RecordedBy: toAPIPredictionActor(o.RecordedBy), RecordedAt: o.RecordedAt.UTC(), Replay: v.Replay,
			}, commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_oracle_observation", ResourceID: o.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdOracleObservations200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdResolve resolves a market from the
// configured oracle's report, which the service verifies against the market's
// registered key.
func (s *Server) PostAdvancedPredictionMarketsMarketIdResolve(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdResolveRequestObject) (api.PostAdvancedPredictionMarketsMarketIdResolveResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	cmd := PredictionResolveCommand{MarketID: id, Actor: actor}
	if request.Body.Reason != nil {
		cmd.Reason = *request.Body.Reason
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionResolutionResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.Resolve(ctx, cmd)
			if cerr != nil {
				return api.PredictionResolutionResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionResolutionResult(v), commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_resolution", ResourceID: v.Resolution.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdResolve200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdDispute disputes a RESOLVED market's
// oracle resolution.
func (s *Server) PostAdvancedPredictionMarketsMarketIdDispute(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdDisputeRequestObject) (api.PostAdvancedPredictionMarketsMarketIdDisputeResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	body := request.Body
	if body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	req := predmarket.DisputeRequest{MarketID: id, Actor: actor, Reason: body.Reason, Evidence: evidenceOf(body.Evidence)}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionDisputeResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.Dispute(ctx, req)
			if cerr != nil {
				return api.PredictionDisputeResult{}, commandMeta{}, cerr
			}
			return api.PredictionDisputeResult{
				ValueDomain: predmarketDomain, Market: toAPIPredictionMarket(v.Record), Dispute: toAPIPredictionDispute(v.Dispute),
			}, commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_dispute", ResourceID: v.Dispute.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdDispute200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdUphold closes a dispute keeping the
// disputed outcome.
func (s *Server) PostAdvancedPredictionMarketsMarketIdUphold(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdUpholdRequestObject) (api.PostAdvancedPredictionMarketsMarketIdUpholdResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	body := request.Body
	if body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	req := predmarket.RulingRequest{MarketID: id, Actor: actor, Reason: body.Reason}
	if body.Evidence != nil {
		req.Evidence = evidenceOf(*body.Evidence)
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionResolutionResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.Uphold(ctx, req)
			if cerr != nil {
				return api.PredictionResolutionResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionResolutionResult(v), commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_resolution", ResourceID: v.Resolution.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdUphold200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdVoidProposals records one operator's
// proposal to void a market. Nothing is refunded until a different operator
// approves it.
func (s *Server) PostAdvancedPredictionMarketsMarketIdVoidProposals(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdVoidProposalsRequestObject) (api.PostAdvancedPredictionMarketsMarketIdVoidProposalsResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	body := request.Body
	if body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	req := predmarket.VoidProposalRequest{MarketID: id, Actor: actor, Reason: body.Reason, Evidence: evidenceOf(body.Evidence)}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionVoidProposalResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.ProposeVoid(ctx, req)
			if cerr != nil {
				return api.PredictionVoidProposalResult{}, commandMeta{}, cerr
			}
			return api.PredictionVoidProposalResult{
				ValueDomain: predmarketDomain, Market: toAPIPredictionMarket(v.Record), Proposal: toAPIPredictionVoidProposal(v.Proposal),
			}, commandMeta{
				Status: http.StatusCreated, ResourceType: "prediction_void_proposal", ResourceID: v.Proposal.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdVoidProposals201JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdVoidProposalsProposalIdApprove approves
// a void proposal as a second operator, which voids the market and refunds
// every share.
func (s *Server) PostAdvancedPredictionMarketsMarketIdVoidProposalsProposalIdApprove(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdVoidProposalsProposalIdApproveRequestObject) (api.PostAdvancedPredictionMarketsMarketIdVoidProposalsProposalIdApproveResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	pid, err := predmarket.ParseVoidProposalID(request.ProposalId.String())
	if err != nil || pid.IsZero() {
		return nil, validationError("proposalId", "proposalId must be a canonical UUID")
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	req := predmarket.VoidApprovalRequest{MarketID: id, ProposalID: pid, Actor: actor}
	if request.Body.Reason != nil {
		req.Reason = *request.Body.Reason
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionSettlementResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.ApproveVoid(ctx, req)
			if cerr != nil {
				return api.PredictionSettlementResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionSettlementResult(v), commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_settlement", ResourceID: v.Settlement.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdVoidProposalsProposalIdApprove200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdHeldPayoutsClaim claims, for the
// caller's own account, the payout a settled or voided market holds for it.
func (s *Server) PostAdvancedPredictionMarketsMarketIdHeldPayoutsClaim(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdHeldPayoutsClaimRequestObject) (api.PostAdvancedPredictionMarketsMarketIdHeldPayoutsClaimResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	// Ownership only: an operator's account:read_any never lets them claim a
	// customer's payout.
	account, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok || p.SubjectID == "" {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	req := predmarket.ClaimRequest{
		MarketID: id, AccountID: account, IdempotencyKey: predmarketIntentKey("claim", request.Params.IdempotencyKey),
		ActorType: p.ActorType, ActorID: p.SubjectID, CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionClaimResult, commandMeta, error) {
			r, cerr := s.opts.Ports.PredictionMarkets.Claim(ctx, req)
			if cerr != nil {
				return api.PredictionClaimResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionClaimResult(req, r), commandMeta{
				Status: http.StatusOK, ResourceType: "economic_intent", ResourceID: r.Action.Intent.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdHeldPayoutsClaim200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdSettle settles a RESOLVED market from
// its escrow. Settling a SETTLED market pays nothing and says so.
func (s *Server) PostAdvancedPredictionMarketsMarketIdSettle(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdSettleRequestObject) (api.PostAdvancedPredictionMarketsMarketIdSettleResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionSettlementResult, commandMeta, error) {
			v, cerr := s.opts.Ports.PredictionMarkets.Settle(ctx, predmarket.SettleRequest{MarketID: id, Actor: actor})
			if cerr != nil {
				return api.PredictionSettlementResult{}, commandMeta{}, cerr
			}
			return toAPIPredictionSettlementResult(v), commandMeta{
				Status: http.StatusOK, ResourceType: "prediction_settlement", ResourceID: v.Settlement.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdSettle200JSONResponse(res.Value), nil
}

// PostAdvancedPredictionMarketsMarketIdReconcile runs the PREDICTION_ESCROW
// reconciliation for one market and returns the record it wrote or refreshed.
func (s *Server) PostAdvancedPredictionMarketsMarketIdReconcile(ctx context.Context, request api.PostAdvancedPredictionMarketsMarketIdReconcileRequestObject) (api.PostAdvancedPredictionMarketsMarketIdReconcileResponseObject, error) {
	if s.opts.Ports.PredictionMarkets == nil {
		return nil, errNotWired(predmarketWhat)
	}
	id, err := parseMarketID(request.MarketId)
	if err != nil {
		return nil, err
	}
	actor, err := predmarketActor(ctx)
	if err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PredictionEscrowReconciliation, commandMeta, error) {
			rec, cerr := s.opts.Ports.PredictionMarkets.Reconcile(ctx, id, reconciliation.Actor{Type: actor.Type, ID: actor.ID})
			if cerr != nil {
				return api.PredictionEscrowReconciliation{}, commandMeta{}, cerr
			}
			out, cerr := toAPIPredictionReconciliation(rec)
			if cerr != nil {
				return api.PredictionEscrowReconciliation{}, commandMeta{}, cerr
			}
			return out, commandMeta{Status: http.StatusOK, ResourceType: "reconciliation_record", ResourceID: rec.ID.String()}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostAdvancedPredictionMarketsMarketIdReconcile200JSONResponse(res.Value), nil
}

// --- shapes --------------------------------------------------------------------

func toAPIPredictionActor(a econ.Actor) api.PredictionActor {
	return api.PredictionActor{ActorType: string(a.Type), ActorId: a.ID}
}

// outcomeSymbolOf names an outcome index of a market; an index the market
// does not have is named by nothing rather than guessed at.
func outcomeSymbolOf(m predmarket.Market, index int) string {
	for _, o := range m.Outcomes {
		if o.Index == index {
			return o.Symbol
		}
	}
	return ""
}

func toAPIPredictionMarket(r PredictionMarketRecord) api.PredictionMarket {
	m := r.Market
	out := api.PredictionMarket{
		ValueDomain: predmarketDomain, Id: toUUID(m.ID), Symbol: m.Symbol, Question: m.Question,
		Mode: api.PredictionMarketMode(m.Mode), Status: api.PredictionMarketStatus(m.Status),
		FacilityId: toUUID(m.FacilityID), InstrumentId: toUUID(m.InstrumentID),
		CollateralAssetId: toUUID(m.CollateralAssetID), CollateralSymbol: r.Collateral.Symbol,
		CollateralDecimals: int(r.Collateral.Decimals), UnitQuantity: m.UnitQuantity.String(),
		ShareDecimals: int(predmarket.ShareDecimals), Outcomes: make([]api.PredictionOutcome, 0, len(m.Outcomes)),
		OpenAt: m.OpenAt.UTC(), CloseAt: m.CloseAt.UTC(), DisputeWindowMs: m.DisputeWindow.Milliseconds(),
		OracleSource: m.OracleSource, OraclePublicKey: hex.EncodeToString(m.OraclePublicKey),
		DefinitionHash: hex.EncodeToString(m.DefinitionHash), CreatedBy: toAPIPredictionActor(m.CreatedBy),
		CreatedAt: m.CreatedAt.UTC(), UpdatedAt: m.UpdatedAt.UTC(),
	}
	if m.CurrentResolutionID != nil {
		out.CurrentResolutionId = uuidPtr(*m.CurrentResolutionID)
	}
	for _, o := range m.Outcomes {
		out.Outcomes = append(out.Outcomes, api.PredictionOutcome{
			Index: o.Index, Symbol: o.Symbol, ShareAssetId: toUUID(o.ShareAssetID), InstrumentId: toUUID(o.InstrumentID),
		})
	}
	return out
}

func toAPIPredictionResolution(m predmarket.Market, r predmarket.Resolution) api.PredictionResolution {
	out := api.PredictionResolution{
		Id: toUUID(r.ID), MarketId: toUUID(r.MarketID), Kind: api.PredictionResolutionKind(r.Kind),
		OutcomeIndex: r.OutcomeIndex, OutcomeSymbol: outcomeSymbolOf(m, r.OutcomeIndex),
		ReportSource: strPtr(r.ReportSource), ReportObservedAt: timePtr(r.ReportObservedAt),
		EvidenceRef: r.EvidenceRef, EvidenceSha256: hex.EncodeToString(r.EvidenceSHA256),
		AcceptedBy: toAPIPredictionActor(r.AcceptedBy), Reason: r.Reason, AcceptedAt: r.AcceptedAt.UTC(),
	}
	if r.SupersedesID != nil {
		out.SupersedesId = uuidPtr(*r.SupersedesID)
	}
	if r.DisputeID != nil {
		out.DisputeId = uuidPtr(*r.DisputeID)
	}
	if len(r.ReportHash) > 0 {
		h := hex.EncodeToString(r.ReportHash)
		out.ReportHash = &h
	}
	return out
}

func toAPIPredictionDispute(d predmarket.Dispute) api.PredictionDispute {
	return api.PredictionDispute{
		Id: toUUID(d.ID), MarketId: toUUID(d.MarketID), ResolutionId: toUUID(d.ResolutionID),
		RaisedBy: toAPIPredictionActor(d.RaisedBy), Reason: d.Reason, EvidenceRef: d.EvidenceRef,
		EvidenceSha256: hex.EncodeToString(d.EvidenceSHA256), RaisedAt: d.RaisedAt.UTC(),
	}
}

func toAPIPredictionSettlement(m predmarket.Market, st predmarket.Settlement) api.PredictionSettlement {
	out := api.PredictionSettlement{
		Id: toUUID(st.ID), MarketId: toUUID(st.MarketID), Kind: api.PredictionSettlementKind(st.Kind),
		OutcomeCount: st.OutcomeCount, UnitQuantity: st.UnitQuantity.String(), RefundRatePerShare: qtyPtr(st.RefundRatePerShare),
		RoundingPolicy: api.PredictionRoundingPolicy(st.RoundingPolicy), EscrowBefore: st.EscrowBefore.String(),
		TotalPaid: st.TotalPaid.String(), TotalHeld: st.TotalHeld.String(), Residual: st.Residual.String(), HolderCount: st.HolderCount,
		EvidenceRef: st.EvidenceRef, EvidenceSha256: hex.EncodeToString(st.EvidenceSHA256),
		SettledBy: toAPIPredictionActor(st.SettledBy), SettledAt: st.SettledAt.UTC(),
	}
	if st.ResolutionID != nil {
		out.ResolutionId = uuidPtr(*st.ResolutionID)
	}
	if st.WinningOutcomeIndex != nil {
		w := *st.WinningOutcomeIndex
		out.WinningOutcomeIndex = &w
		out.WinningOutcomeSymbol = strPtr(outcomeSymbolOf(m, w))
	}
	return out
}

func toAPIPredictionResolutionResult(v PredictionResolveView) api.PredictionResolutionResult {
	return api.PredictionResolutionResult{
		ValueDomain: predmarketDomain, Market: toAPIPredictionMarket(v.Record),
		Resolution: toAPIPredictionResolution(v.Record.Market, v.Resolution), Replay: v.Replay,
	}
}

func toAPIPredictionSettlementResult(v PredictionSettleView) api.PredictionSettlementResult {
	m := v.Record.Market
	out := api.PredictionSettlementResult{
		ValueDomain: predmarketDomain, Market: toAPIPredictionMarket(v.Record),
		Settlement: toAPIPredictionSettlement(m, v.Settlement), Replay: v.Replay,
		Lines:       make([]api.PredictionPayoutLine, 0, len(v.Plan.Lines)),
		HeldPayouts: make([]api.PredictionHeldPayout, 0, len(v.Held)),
	}
	for _, h := range v.Held {
		out.HeldPayouts = append(out.HeldPayouts, toAPIPredictionHeldPayout(h))
	}
	for i, line := range v.Plan.Lines {
		l := api.PredictionPayoutLine{
			AccountId: toUUID(line.AccountID), Amount: line.Amount.String(),
			Shares: make([]api.PredictionShareCount, 0, len(line.Shares)),
		}
		for j, q := range line.Shares {
			l.Shares = append(l.Shares, api.PredictionShareCount{OutcomeSymbol: outcomeSymbolOf(m, j), Quantity: q.String()})
		}
		// The service runs one economic action per plan line, in plan order:
		// a delivered payout's intent COMPLETED, a held one's REJECTED (its
		// journal retired the shares and the amount stayed in the escrow).
		if i < len(v.Lines) {
			l.IntentId = toUUID(v.Lines[i].Intent.ID)
			l.Held = v.Lines[i].Intent.Status == econ.StatusRejected
			if !v.Lines[i].JournalTransactionID.IsZero() {
				l.JournalTransactionId = uuidPtr(v.Lines[i].JournalTransactionID)
			}
		}
		out.Lines = append(out.Lines, l)
	}
	return out
}

func toAPIPredictionVoidProposal(p predmarket.VoidProposal) api.PredictionVoidProposal {
	out := api.PredictionVoidProposal{
		Id: toUUID(p.ID), MarketId: toUUID(p.MarketID), MarketStatus: api.PredictionMarketStatus(p.MarketStatus),
		Reason: p.Reason, EvidenceRef: p.EvidenceRef, EvidenceSha256: hex.EncodeToString(p.EvidenceSHA256),
		ProposedBy: toAPIPredictionActor(p.ProposedBy), ProposedAt: p.ProposedAt.UTC(),
	}
	if p.ResolutionID != nil {
		out.ResolutionId = uuidPtr(*p.ResolutionID)
	}
	if a := p.Approval; a != nil {
		out.Approval = &api.PredictionVoidApproval{
			Id: toUUID(a.ID), ApprovedBy: toAPIPredictionActor(a.ApprovedBy), Reason: a.Reason, ApprovedAt: a.ApprovedAt.UTC(),
		}
	}
	return out
}

func toAPIPredictionHeldPayout(h predmarket.HeldPayout) api.PredictionHeldPayout {
	out := api.PredictionHeldPayout{
		Id: toUUID(h.ID), MarketId: toUUID(h.MarketID), SettlementId: toUUID(h.SettlementID), AccountId: toUUID(h.AccountID),
		Kind: api.PredictionSettlementKind(h.Kind), Amount: h.Amount.String(), ReasonCodes: append([]string{}, h.ReasonCodes...),
		RefusedIntentId: toUUID(h.RefusedIntentID), JournalTransactionId: toUUID(h.JournalTransactionID), HeldAt: h.HeldAt.UTC(),
		Claimed: h.Claimed(),
	}
	if c := h.Claim; c != nil {
		out.Claim = &api.PredictionHeldPayoutClaim{
			Id: toUUID(c.ID), Amount: c.Amount.String(), IntentId: toUUID(c.IntentID),
			JournalTransactionId: toUUID(c.JournalTransactionID), ClaimedBy: toAPIPredictionActor(c.ClaimedBy), ClaimedAt: c.ClaimedAt.UTC(),
		}
	}
	return out
}

func toAPIPredictionClaimResult(req predmarket.ClaimRequest, r predmarket.ClaimResult) api.PredictionClaimResult {
	out := api.PredictionClaimResult{
		ValueDomain: predmarketDomain, MarketId: toUUID(req.MarketID), AccountId: toUUID(req.AccountID),
		HeldPayout: toAPIPredictionHeldPayout(r.HeldPayout), Replay: r.Action.Replay,
		Intent: toAPIEconomicIntent(r.Action.Intent), RiskDecisionHash: strPtr(r.Action.DecisionHash),
	}
	if !r.Action.Effect.ID.IsZero() {
		e := toAPIEconomicEffect(r.Action.Effect)
		out.Effect = &e
	}
	if !r.Action.JournalTransactionID.IsZero() {
		out.JournalTransactionId = uuidPtr(r.Action.JournalTransactionID)
	}
	return out
}

func toAPIPredictionSetResult(req predmarket.PositionRequest, action string, r predmarket.ActionResult) api.PredictionSetResult {
	out := api.PredictionSetResult{
		ValueDomain: predmarketDomain, MarketId: toUUID(req.MarketID), AccountId: toUUID(req.AccountID),
		Action: api.PredictionSetAction(action), Quantity: req.Quantity.String(), Replay: r.Replay,
		Intent: toAPIEconomicIntent(r.Intent), RiskDecisionHash: strPtr(r.DecisionHash),
	}
	if !r.Effect.ID.IsZero() {
		e := toAPIEconomicEffect(r.Effect)
		out.Effect = &e
	}
	if !r.JournalTransactionID.IsZero() {
		out.JournalTransactionId = uuidPtr(r.JournalTransactionID)
	}
	return out
}

// toAPIOracleReport renders a report in the oracle's own wire form, through
// the package's encoder, so the bytes a client sees are the ones the package
// would decode.
func toAPIOracleReport(r predmarket.OracleReport) (api.PredictionOracleReport, error) {
	wire, err := predmarket.EncodeOracleReport(r)
	if err != nil {
		return api.PredictionOracleReport{}, err
	}
	var out api.PredictionOracleReport
	if err := json.Unmarshal(wire, &out); err != nil {
		return api.PredictionOracleReport{}, errs.Wrap(err, errs.CodeInternal, "the oracle report could not be rendered")
	}
	return out, nil
}

func toAPIPredictionReconciliation(r reconciliation.Record) (api.PredictionEscrowReconciliation, error) {
	out := api.PredictionEscrowReconciliation{
		ValueDomain: predmarketDomain, Id: toUUID(r.ID), Kind: string(r.Kind), Mode: string(r.Mode),
		ScopeType: r.ScopeType, ScopeId: r.ScopeID, Status: string(r.Status), Material: r.Material,
		BlocksNewRisk: r.BlocksNewRisk, OpenedAt: r.OpenedAt.UTC(), MatchedAt: timePtr(r.MatchedAt),
	}
	for _, f := range []struct {
		raw json.RawMessage
		dst *map[string]interface{}
	}{{r.Expected, &out.Expected}, {r.Observed, &out.Observed}, {r.Difference, &out.Difference}} {
		m, err := decodeObject(f.raw)
		if err != nil {
			return api.PredictionEscrowReconciliation{}, errs.Wrap(err, errs.CodeInternal, "the reconciliation record is not a JSON object")
		}
		if m == nil {
			m = map[string]interface{}{}
		}
		*f.dst = m
	}
	return out, nil
}
