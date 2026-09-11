package httpapi

import (
	"context"
	"net/http"
	"sort"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/payout"
	"github.com/nodal/controlplane/internal/security"
)

// Handlers for the Nodal-native economy (gola.md PARTS XII-XXI).
//
// Two rules govern everything below, and they are the reason this file exists
// separately from handlers_trading.go:
//
//   - A balance is never one number. PART LII forbids showing Credits,
//     simulated capital and real capital as though they were the same thing,
//     and the Credit balance itself is not one number either: gross, spendable,
//     frozen and payout-eligible are four different facts and the response
//     carries all of them plus the reasons for the gap.
//   - An endpoint existing says only that the system knows how to answer.
//     Whether this deployment permits the action is a capability question
//     answered further down, and a refusal names the capability rather than
//     saying "no".

func qtyString(q money.Quantity) string { return q.String() }

// toAPICreditBalance renders the breakdown of PART XX.
func toAPICreditBalance(accountID string, b credit.Balances) api.CreditBalance {
	byOrigin := map[string]string{}
	for o, q := range b.ByOrigin {
		byOrigin[string(o)] = qtyString(q)
	}
	byFinality := map[string]string{}
	for f, q := range b.ByFinality {
		byFinality[string(f)] = qtyString(q)
	}
	reasons := make([]string, 0, len(b.IneligibleReasons))
	for r := range b.IneligibleReasons {
		reasons = append(reasons, string(r))
	}
	sortStrings(reasons)

	id := uuid.MustParse(accountID)
	return api.CreditBalance{
		AccountId: id,
		// The scale travels with the figures. Every consumer that assumed six
		// was a consumer that could be a million times wrong (F-151).
		CreditDecimals:    int(b.CreditDecimals),
		Gross:             qtyString(b.Gross),
		Spendable:         qtyString(b.Spendable),
		Frozen:            qtyString(b.Frozen),
		Reversed:          qtyString(b.Reversed),
		PayoutEligible:    qtyString(b.PayoutEligible),
		Ineligible:        qtyString(b.Ineligible),
		ByOrigin:          &byOrigin,
		ByFinality:        &byFinality,
		IneligibleReasons: &reasons,
		PolicyVersion:     b.PolicyVersion,
		PolicyHash:        ptr(b.PolicyHash),
	}
}

// GetCreditsBalance returns what a user holds and what of it may leave.
func (s *Server) GetCreditsBalance(ctx context.Context, request api.GetCreditsBalanceRequestObject) (api.GetCreditsBalanceResponseObject, error) {
	if s.opts.Ports.Credits == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	b, err := s.opts.Ports.Credits.Balance(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return api.GetCreditsBalance200JSONResponse(toAPICreditBalance(accountID.String(), b)), nil
}

// --- native assets ---------------------------------------------------------

func toAPINativeAsset(a nativeasset.Asset) api.NativeAsset {
	out := api.NativeAsset{
		AssetId:          uuid.MustParse(a.AssetID.String()),
		CreatorAccountId: uuid.MustParse(a.CreatorAccountID.String()),
		Name:             a.Name,
		Symbol:           a.Symbol,
		Description:      ptr(a.Description),
		Status:           api.NativeAssetStatus(a.Status),
		ModerationState:  api.NativeAssetModerationState(a.Moderation),
		ModerationNotes:  ptr(a.ModerationNotes),
		CreatedAt:        ptr(a.CreatedAt.UTC()),
		Supply: api.NativeSupply{
			MaxSupply:          qtyString(a.Supply.MaxSupply),
			CreatorAllocation:  qtyString(a.Supply.CreatorAllocation),
			TreasuryAllocation: qtyString(a.Supply.TreasuryAllocation),
			PoolSupply:         qtyString(a.Supply.PoolSupply()),
		},
		Policy: api.NativeAssetPolicy{
			InternalOnly:           a.Policy.InternalOnly,
			Transferable:           a.Policy.Transferable,
			CashoutEligible:        a.Policy.CashoutEligible,
			CreatorEarningEligible: ptr(a.Policy.CreatorEarningEligible),
			MarketProceedsEligible: ptr(a.Policy.MarketProceedsEligible),
			MinimumAge:             a.Policy.MinimumAge,
			JurisdictionPolicy:     ptr(a.Policy.JurisdictionPolicy),
			MarketingRestrictions:  ptr(a.Policy.MarketingRestrictions),
		},
	}
	if a.ImageURL != "" {
		out.ImageUrl = ptr(a.ImageURL)
	}
	if a.EconomicsLockedAt != nil {
		out.EconomicsLockedAt = timePtr(a.EconomicsLockedAt)
	}
	if a.ActivatedAt != nil {
		out.ActivatedAt = timePtr(a.ActivatedAt)
	}
	return out
}

// PostNativeAssets creates an asset in DRAFT after content screening.
//
// A screening refusal is a 422 whose problem carries every finding, because a
// creator told only "rejected" cannot fix anything, and a moderation system
// nobody can argue with is a moderation system nobody trusts.
func (s *Server) PostNativeAssets(ctx context.Context, request api.PostNativeAssetsRequestObject) (api.PostNativeAssetsResponseObject, error) {
	if s.opts.Ports.NativeAssets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	maxSupply, err := money.ParseQuantity(request.Body.MaxSupply)
	if err != nil {
		return nil, validationError("max_supply", "max_supply must be an integer string of base units")
	}
	creatorAllocation := money.Quantity{}
	if request.Body.CreatorAllocation != nil {
		creatorAllocation, err = money.ParseQuantity(*request.Body.CreatorAllocation)
		if err != nil {
			return nil, validationError("creator_allocation", "creator_allocation must be an integer string of base units")
		}
	}
	decimals := uint8(0)
	if request.Body.Decimals != nil {
		if *request.Body.Decimals < 0 || *request.Body.Decimals > int(nativeasset.MaxDecimals) {
			return nil, validationError("decimals", "decimals must be between 0 and 18")
		}
		decimals = uint8(*request.Body.Decimals) // #nosec G115 -- bounds checked above
	}

	cmd := CreateNativeAsset{
		AccountID: accountID, Name: request.Body.Name, Symbol: request.Body.Symbol,
		MaxSupply: maxSupply, CreatorAllocation: creatorAllocation, Decimals: decimals,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}
	if request.Body.Description != nil {
		cmd.Description = *request.Body.Description
	}
	if request.Body.ImageUrl != nil {
		cmd.ImageURL = *request.Body.ImageUrl
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.NativeAsset, commandMeta, error) {
			asset, _, cerr := s.opts.Ports.NativeAssets.Create(ctx, cmd)
			if cerr != nil {
				return api.NativeAsset{}, commandMeta{}, cerr
			}
			return toAPINativeAsset(asset), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "native_asset",
				ResourceID:   asset.AssetID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostNativeAssets200JSONResponse(res.Value), nil
	}
	return api.PostNativeAssets201JSONResponse(res.Value), nil
}

// GetNativeAssets lists assets whose markets accept at least sells.
func (s *Server) GetNativeAssets(ctx context.Context, request api.GetNativeAssetsRequestObject) (api.GetNativeAssetsResponseObject, error) {
	if s.opts.Ports.NativeAssets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	limit := 50
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	list, err := s.opts.Ports.NativeAssets.ListTradable(ctx, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.NativeAsset, 0, len(list))
	for _, a := range list {
		items = append(items, toAPINativeAsset(a))
	}
	return api.GetNativeAssets200JSONResponse(api.NativeAssetPage{Items: items}), nil
}

// GetNativeAssetsAssetId returns one asset.
func (s *Server) GetNativeAssetsAssetId(ctx context.Context, request api.GetNativeAssetsAssetIdRequestObject) (api.GetNativeAssetsAssetIdResponseObject, error) {
	if s.opts.Ports.NativeAssets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	assetID, err := assets.ParseAssetID(request.AssetId.String())
	if err != nil {
		return nil, validationError("assetId", "assetId must be a canonical UUID")
	}
	a, err := s.opts.Ports.NativeAssets.Get(ctx, assetID)
	if err != nil {
		return nil, err
	}
	// A DRAFT or PENDING_REVIEW asset is its creator's private work, and this
	// response carries its moderation state and the moderator's notes.
	// `ListTradable` shows only ACTIVE and CLOSE_ONLY; this read had no filter
	// at all, so anybody holding `native_asset:read` -- every customer -- could
	// read an unpublished asset and the internal commentary on it by asking for
	// its id. NOT_FOUND rather than FORBIDDEN, for the same reason as
	// everywhere else: a distinguishable refusal is a membership oracle (F-41).
	if !a.Status.AllowsSell() && securityRequireAccountOwner(ctx, a.CreatorAccountID.String()) != nil {
		return nil, errs.New(errs.CodeNotFound, "no such asset").
			WithField("asset_id", assetID.String())
	}
	return api.GetNativeAssetsAssetId200JSONResponse(toAPINativeAsset(a)), nil
}

// --- native markets --------------------------------------------------------

// GetNativeMarketsMarketId returns the market with everything PART LIV says a
// buyer should see before trading: reserves, fees, and who holds it.
func (s *Server) GetNativeMarketsMarketId(ctx context.Context, request api.GetNativeMarketsMarketIdRequestObject) (api.GetNativeMarketsMarketIdResponseObject, error) {
	if s.opts.Ports.NativeMarkets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	v, err := s.opts.Ports.NativeMarkets.Market(ctx, marketID)
	if err != nil {
		return nil, err
	}

	// Naming nobody, like the summary read: this route takes no account, so no
	// row is marked as the caller's own either (D-111).
	holders := toAPIHolders(v.Holders)

	spot := nativemarket.SpotPrice(v.Market.Curve, v.State)
	scale := nativemarket.PriceScale
	return api.GetNativeMarketsMarketId200JSONResponse(api.NativeMarket{
		MarketId:             uuid.MustParse(v.Market.ID.String()),
		AssetId:              uuid.MustParse(v.Market.AssetID.String()),
		Status:               api.NativeMarketStatus(v.Market.Status),
		VirtualCreditReserve: ptr(qtyString(v.Market.Curve.VirtualCreditReserve)),
		InitialAssetReserve:  ptr(qtyString(v.Market.Curve.InitialAssetReserve)),
		RealCreditReserve:    qtyString(v.State.RealCreditReserve),
		AssetReserve:         qtyString(v.State.AssetReserve),
		CirculatingSupply:    ptr(qtyString(v.CirculatingSupply())),
		SpotPrice:            ptr(qtyString(spot)),
		PriceScale:           ptr(scale),
		AssetDecimals:        ptr(v.AssetDecimals),
		PlatformFeeBps:       int(v.Market.Fees.PlatformBPS),
		CreatorFeeBps:        int(v.Market.Fees.CreatorBPS),
		StateVersion:         v.State.Version,
		TopHolders:           &holders,
	}), nil
}

// PostNativeMarketsMarketIdQuotes prices a hypothetical trade.
func (s *Server) PostNativeMarketsMarketIdQuotes(ctx context.Context, request api.PostNativeMarketsMarketIdQuotesRequestObject) (api.PostNativeMarketsMarketIdQuotesResponseObject, error) {
	if s.opts.Ports.NativeMarkets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseQuantity(request.Body.Amount)
	if err != nil {
		return nil, validationError("amount", "amount must be an integer string of base units")
	}
	// A quote is persisted -- it is the record of what the user was shown --
	// so it goes through the same idempotent command path as every other write
	// rather than being treated as a read that happens to insert a row.
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (NativeQuoteView, commandMeta, error) {
			out, qerr := s.opts.Ports.NativeMarkets.Quote(ctx, nativemarket.QuoteRequest{
				MarketID: marketID, AccountID: accountID,
				Side: nativemarket.Side(request.Body.Side), Amount: amount,
			})
			if qerr != nil {
				return NativeQuoteView{}, commandMeta{}, qerr
			}
			return out, commandMeta{
				Status:       http.StatusOK,
				ResourceType: "native_quote",
				ResourceID:   out.Quote.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	// A replayed record written before the response type gained the asset scale
	// unmarshals into a zero-valued view, because runCommand stores the command
	// value as JSON and reads it back into whatever type the handler now asks
	// for. Records live 24 hours, so the window is bounded and the answer would
	// otherwise be a quote of zero for zero -- which a client would act on.
	// Refusing is the only honest reply: the original response no longer exists
	// in a form this build can return.
	if res.Value.Quote.ID.IsZero() {
		return nil, errs.New(errs.CodeConflict,
			"this idempotency key was used before the quote response changed shape; retry with a new key")
	}
	q := res.Value.Quote
	scale := nativemarket.PriceScale
	return api.PostNativeMarketsMarketIdQuotes200JSONResponse(api.NativeQuote{
		QuoteId:         uuid.MustParse(q.ID.String()),
		MarketId:        uuid.MustParse(q.MarketID.String()),
		Side:            api.NativeQuoteSide(q.Side),
		InputAmount:     qtyString(q.InputAmount),
		ExpectedOutput:  qtyString(q.ExpectedOutput),
		PlatformFee:     ptr(qtyString(q.PlatformFee)),
		CreatorFee:      ptr(qtyString(q.CreatorFee)),
		SpotPriceBefore: ptr(qtyString(q.SpotPriceBefore)),
		EffectivePrice:  ptr(qtyString(q.EffectivePrice)),
		PriceScale:      ptr(scale),
		AssetDecimals:   ptr(res.Value.AssetDecimals),
		SlippageBps:     ptr(int(q.SlippageBPS)),
		StateVersion:    q.StateVersion,
		ExpiresAt:       q.ExpiresAt.UTC(),
	}), nil
}

// PostNativeMarketsMarketIdOrders executes a trade.
func (s *Server) PostNativeMarketsMarketIdOrders(ctx context.Context, request api.PostNativeMarketsMarketIdOrdersRequestObject) (api.PostNativeMarketsMarketIdOrdersResponseObject, error) {
	if s.opts.Ports.NativeMarkets == nil {
		return nil, errNotWired("the Nodal-native economy")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	marketID, err := nativemarket.ParseMarketID(request.MarketId.String())
	if err != nil {
		return nil, validationError("marketId", "marketId must be a canonical UUID")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseQuantity(request.Body.Amount)
	if err != nil {
		return nil, validationError("amount", "amount must be an integer string of base units")
	}
	minOutput, err := money.ParseQuantity(request.Body.MinOutput)
	if err != nil {
		return nil, validationError("min_output", "min_output must be an integer string of base units")
	}
	var quoteID *nativemarket.QuoteID
	if request.Body.QuoteId != nil {
		parsed, perr := nativemarket.ParseQuoteID(request.Body.QuoteId.String())
		if perr != nil {
			return nil, validationError("quote_id", "quote_id must be a canonical UUID")
		}
		quoteID = &parsed
	}

	cmd := nativemarket.ExecuteRequest{
		MarketID: marketID, AccountID: accountID,
		Side: nativemarket.Side(request.Body.Side), Amount: amount, MinOutput: minOutput,
		QuoteID: quoteID, IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID: observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.NativeFill, commandMeta, error) {
			out, eerr := s.opts.Ports.NativeMarkets.Execute(ctx, cmd)
			if eerr != nil {
				return api.NativeFill{}, commandMeta{}, eerr
			}
			return toAPINativeFill(out.Result, out.AssetDecimals), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "native_fill",
				ResourceID:   out.Result.FillID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostNativeMarketsMarketIdOrders200JSONResponse(res.Value), nil
	}
	return api.PostNativeMarketsMarketIdOrders201JSONResponse(res.Value), nil
}

func toAPINativeFill(r nativemarket.ExecuteResult, assetDecimals int) api.NativeFill {
	alerts := make([]struct {
		Kind     *string                       `json:"kind,omitempty"`
		Reason   *string                       `json:"reason,omitempty"`
		Severity *api.NativeFillAlertsSeverity `json:"severity,omitempty"`
	}, 0, len(r.Alerts))
	for _, a := range r.Alerts {
		kind := string(a.Kind)
		sev := api.NativeFillAlertsSeverity(a.Severity)
		reason, _ := a.Detail["reason"].(string)
		alerts = append(alerts, struct {
			Kind     *string                       `json:"kind,omitempty"`
			Reason   *string                       `json:"reason,omitempty"`
			Severity *api.NativeFillAlertsSeverity `json:"severity,omitempty"`
		}{Kind: &kind, Reason: &reason, Severity: &sev})
	}
	scale := nativemarket.PriceScale
	return api.NativeFill{
		FillId:                 uuid.MustParse(r.FillID.String()),
		MarketId:               uuid.MustParse(r.MarketID.String()),
		Side:                   api.NativeFillSide(r.Fill.Side),
		CreditsIn:              ptr(qtyString(r.Fill.CreditsIn)),
		CreditsOut:             ptr(qtyString(r.Fill.CreditsOut)),
		AssetsIn:               ptr(qtyString(r.Fill.AssetsIn)),
		AssetsOut:              ptr(qtyString(r.Fill.AssetsOut)),
		PlatformFee:            ptr(qtyString(r.Fill.PlatformFee)),
		CreatorFee:             ptr(qtyString(r.Fill.CreatorFee)),
		EffectivePrice:         ptr(qtyString(r.Fill.EffectivePrice)),
		PriceScale:             ptr(scale),
		AssetDecimals:          ptr(assetDecimals),
		SlippageBps:            ptr(int(r.Fill.SlippageBPS())),
		RealCreditReserveAfter: ptr(qtyString(r.Fill.StateAfter.RealCreditReserve)),
		AssetReserveAfter:      ptr(qtyString(r.Fill.StateAfter.AssetReserve)),
		StateVersionAfter:      r.Fill.StateAfter.Version,
		Alerts:                 &alerts,
	}
}

// --- payouts ---------------------------------------------------------------

func toAPIPayout(r payout.Request, d payout.Decision) api.PayoutRequest {
	out := api.PayoutRequest{
		PayoutId:          uuid.MustParse(r.ID.String()),
		AccountId:         uuid.MustParse(r.AccountID.String()),
		State:             api.PayoutRequestState(r.State),
		RequestedQuantity: qtyString(r.RequestedQuantity),
		ReservedQuantity:  qtyString(r.ReservedQuantity),
		SettledQuantity:   ptr(qtyString(r.SettledQuantity)),
		PolicyVersion:     r.PolicyVersion,
		PolicyHash:        ptr(r.PolicyHash),
		CreatedAt:         ptr(r.CreatedAt.UTC()),
	}
	if r.DestinationID != nil {
		id := uuid.MustParse(r.DestinationID.String())
		out.DestinationId = &id
	}
	if r.FailureReason != "" {
		out.FailureReason = ptr(r.FailureReason)
	}
	// Why a RESERVED payout cannot be sent, which is not a failure and not a
	// state: the value is still reserved and cancelling is what releases it
	// (F-277). The Withdraw page renders it beside the Cancel control.
	if r.BlockedReason != "" {
		out.BlockedReason = ptr(r.BlockedReason)
		if r.BlockedAt != nil {
			out.BlockedAt = ptr(r.BlockedAt.UTC())
		}
	}
	reasons := append([]string(nil), r.EligibilityReasons...)
	if len(d.Reasons) > 0 {
		reasons = d.ReasonStrings()
	}
	if len(reasons) > 0 {
		out.EligibilityReasons = &reasons
	}
	// The decision fields are present only on creation, where the caller has
	// just been told what was decided and needs to know what to do next.
	if d.PolicyVersion != "" {
		out.EligibleQuantity = ptr(qtyString(d.Eligible))
		out.VerificationWouldSuffice = ptr(d.VerificationWouldSuffice)
		out.RequiredVerification = ptr(string(d.RequiredVerification))
		// What value WOULD leave, from the decision's own lot selection. On a
		// creation the allocations may not exist yet -- a request that only
		// needs verification reserves nothing -- so the decision is the only
		// place this can come from (PART 23).
		out.Provenance = ptr(toAPIProvenance(payout.DecisionProvenance(d)))
	}
	if r.QuoteID != nil {
		id := uuid.MustParse(r.QuoteID.String())
		out.QuoteId = &id
	}
	// The price the customer was shown, read off the request rather than
	// recomputed: a fee schedule repriced later must not change what a payout
	// says it sent (D-119). Zero on a row written before the quote was
	// required, and omitted rather than rendered as a free payout.
	if r.QuoteCurrency != "" {
		out.QuotedGrossAmountMinor = ptr(r.QuoteGrossAmountMinor)
		out.QuotedFeeAmountMinor = ptr(r.QuoteFeeAmountMinor)
		out.QuotedNetAmountMinor = ptr(r.QuoteNetAmountMinor)
		out.QuotedCurrency = ptr(r.QuoteCurrency)
	}
	// The tier this request was MADE on, read off the row. It used to be
	// answered only by the by-id read, from today's provider mode, so the same
	// payout was a rehearsal on one screen and unlabelled on two others
	// (F-232).
	//
	// There is no "no recorded fact" case any more. The column was nullable and
	// this reader treated NULL as a rehearsal while the PROD CHECK treated it as
	// a real payout, so one of the two was wrong on every pre-00810 row and
	// nothing could say which; 00817 made it NOT NULL and backfilled the rows
	// that had no fact as rehearsals, on the ground that no PROD deployment of
	// this system has ever existed (D-134).
	out.Sandbox = ptr(r.Sandbox)
	if r.Provider != "" {
		out.Provider = ptr(r.Provider)
	}
	return out
}

// withProvenance attaches what value a payout draws on, in the order it leaves
// (PART 23). It is a read over payout_allocations and changes no ledger
// semantics: a payout does not take "500 Credits", it takes specific units from
// specific provenance lots, and a person is entitled to see which.
func (s *Server) withProvenance(ctx context.Context, out api.PayoutRequest, id payout.RequestID) api.PayoutRequest {
	slices, err := s.opts.Ports.Payouts.Provenance(ctx, id)
	if err == nil && len(slices) > 0 {
		out.Provenance = ptr(toAPIProvenance(slices))
	}
	// The sandbox label is NOT set here any more. It used to be
	// `s.opts.Ports.Payouts.SandboxProvider()` -- what the provider is now --
	// which made this route the only one that answered it and made the answer a
	// property of today's configuration rather than of the payout (F-232).
	// toAPIPayout reads the column.
	return out
}

// PostPayouts requests a payout of eligible value.
func (s *Server) PostPayouts(ctx context.Context, request api.PostPayoutsRequestObject) (api.PostPayoutsResponseObject, error) {
	if s.opts.Ports.Payouts == nil {
		return nil, errNotWired("payouts")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseQuantity(request.Body.Amount)
	if err != nil {
		return nil, validationError("amount", "amount must be an integer string of base units")
	}
	var destID *payout.DestinationID
	if request.Body.DestinationId != nil {
		parsed, perr := payout.ParseDestinationID(request.Body.DestinationId.String())
		if perr != nil {
			return nil, validationError("destination_id", "destination_id must be a canonical UUID")
		}
		destID = &parsed
	}
	// The quote the customer was shown (PART 19, PART 22), and it is REQUIRED.
	//
	// It used to be optional, on the stated reason that "an operator resolving a
	// stuck payout has no quote to name". That reason was false: this route is
	// accountScopeWrite, and an operator resolves a stuck payout through
	// ResolveManualReview, which creates nothing. What being optional bought
	// was a payout of 50 Credits against a provider publishing a $1.00 minimum
	// -- refused by POST /payouts/quote in as many words -- reserved and settled
	// with the fee never taken, because the whole minimum-and-fee branch of
	// internal/payout.Create sat inside `if r.QuoteID != nil` (F-224, D-119).
	//
	// It must name the same destination and the same gross amount, and
	// internal/payout consumes it inside the reserving transaction so one quote
	// funds exactly one payout.
	if request.Body.QuoteId == uuid.Nil {
		return nil, validationError("quote_id",
			"a payout names the quote the customer was shown; ask POST /v1/payouts/quote first")
	}
	quoteID, perr := payout.ParseQuoteID(request.Body.QuoteId.String())
	if perr != nil {
		return nil, validationError("quote_id", "quote_id must be a canonical UUID")
	}

	cmd := CreatePayout{
		AccountID: accountID, Amount: amount, DestinationID: destID, QuoteID: &quoteID,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PayoutRequest, commandMeta, error) {
			req, decision, cerr := s.opts.Ports.Payouts.Create(ctx, cmd)
			if cerr != nil {
				return api.PayoutRequest{}, commandMeta{}, cerr
			}
			return toAPIPayout(req, decision), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "payout_request",
				ResourceID:   req.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostPayouts200JSONResponse(res.Value), nil
	}
	return api.PostPayouts201JSONResponse(res.Value), nil
}

// GetPayouts lists an account's payout requests.
func (s *Server) GetPayouts(ctx context.Context, request api.GetPayoutsRequestObject) (api.GetPayoutsResponseObject, error) {
	if s.opts.Ports.Payouts == nil {
		return nil, errNotWired("payouts")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	limit := 50
	if request.Params.Limit != nil {
		limit = *request.Params.Limit
	}
	list, err := s.opts.Ports.Payouts.ListByAccount(ctx, accountID, limit)
	if err != nil {
		return nil, err
	}
	items := make([]api.PayoutRequest, 0, len(list))
	for _, r := range list {
		items = append(items, toAPIPayout(r, payout.Decision{}))
	}
	return api.GetPayouts200JSONResponse(api.PayoutRequestPage{Items: items}), nil
}

// GetPayoutsPayoutId returns one payout request.
func (s *Server) GetPayoutsPayoutId(ctx context.Context, request api.GetPayoutsPayoutIdRequestObject) (api.GetPayoutsPayoutIdResponseObject, error) {
	if s.opts.Ports.Payouts == nil {
		return nil, errNotWired("payouts")
	}
	id, err := payout.ParseRequestID(request.PayoutId.String())
	if err != nil {
		return nil, validationError("payoutId", "payoutId must be a canonical UUID")
	}
	r, err := s.opts.Ports.Payouts.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// Tenant scoping: a payout belongs to an account, and reading somebody
	// else's is a cross-tenant read however it is addressed.
	//
	// NOT_FOUND, not FORBIDDEN. This route fetched first and checked after, so
	// a foreign payout answered 403 and an absent one 404 -- a membership
	// oracle: anybody could learn which payout ids exist by asking. Its own
	// sibling, POST /payouts/{id}/cancel, has answered NOT_FOUND to both since
	// F-29 and says why in a comment. The two are the same resource and must
	// not disagree about what a stranger is told (F-41).
	if serr := securityRequireAccount(ctx, r.AccountID.String()); serr != nil {
		return nil, errs.New(errs.CodeNotFound, "no such payout").
			WithField("payout_id", id.String())
	}
	return api.GetPayoutsPayoutId200JSONResponse(
		s.withProvenance(ctx, toAPIPayout(r, payout.Decision{}), id),
	), nil
}

// securityRequireAccountOwner is security.RequireAccountOwner: ownership only,
// no operator override. Used by the by-id reads to decide whether a caller may
// see something unpublished, which is a question about the owner and not about
// operator standing -- an operator reading a DRAFT still goes through the
// account-scoped routes, which do honour the override.
var securityRequireAccountOwner = security.RequireAccountOwner

// securityRequireAccount is security.RequireAccount, named locally so the
// tenant check reads the same way in this file as accountScope does elsewhere.
func securityRequireAccount(ctx context.Context, accountID string) error {
	return security.RequireAccount(ctx, accountID)
}

func ptr[T any](v T) *T { return &v }

func sortStrings(s []string) { sort.Strings(s) }
