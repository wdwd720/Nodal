package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/execution"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/instruments"
	"github.com/nodal/controlplane/internal/intent"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
	"github.com/nodal/controlplane/internal/settlement"
)

// PostQuotesPreview returns a non-binding disclosure of what a trade would
// cost. Nothing is reserved and no money moves. When no execution adapter is
// configured the answer is PROVIDER_UNAVAILABLE: a price is never invented.
func (s *Server) PostQuotesPreview(ctx context.Context, request api.PostQuotesPreviewRequestObject) (api.PostQuotesPreviewResponseObject, error) {
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	instrumentID, err := instruments.ParseInstrumentID(request.Body.InstrumentId.String())
	if err != nil || instrumentID.IsZero() {
		return nil, validationError("instrument_id", "instrument_id must be a canonical UUID")
	}
	action := intent.Action(request.Body.Action)
	if !action.Declared() {
		return nil, validationError("action", "unknown action")
	}
	notional, quantity, err := parseAmounts(request.Body.NotionalUsd, request.Body.Quantity)
	if err != nil {
		return nil, err
	}
	constraints, err := fromAPIConstraints(request.Body.Constraints)
	if err != nil {
		return nil, err
	}
	if s.opts.Ports.Quotes == nil {
		return nil, errs.New(errs.CodeProviderUnavailable,
			"no execution venue adapter is configured, so no quote can be produced")
	}
	v, err := s.opts.Ports.Quotes.Preview(ctx, QuotePreview{
		AccountID:    accountID,
		InstrumentID: instrumentID,
		Action:       action,
		NotionalUSD:  notional,
		Quantity:     quantity,
		Constraints:  constraints,
	})
	if err != nil {
		return nil, err
	}
	return api.PostQuotesPreview200JSONResponse(toAPIQuoteDisclosure(v)), nil
}

func parseAmounts(notional *api.USD, qty *api.Quantity) (*money.USD, *money.Quantity, error) {
	var (
		outUSD *money.USD
		outQty *money.Quantity
	)
	if notional != nil {
		v, err := money.ParseUSD(*notional)
		if err != nil {
			return nil, nil, validationError("notional_usd", "notional_usd must be a decimal string with two fraction digits")
		}
		outUSD = &v
	}
	if qty != nil {
		v, err := money.ParseQuantity(*qty)
		if err != nil {
			return nil, nil, validationError("quantity", "quantity must be an exact integer base-unit string")
		}
		outQty = &v
	}
	return outUSD, outQty, nil
}

// PostIntents submits a typed trade intent (PART 35). The intent is a request:
// RECEIVED means recorded, never authorized. Eligibility, risk, reservation and
// planning happen downstream in their own packages.
func (s *Server) PostIntents(ctx context.Context, request api.PostIntentsRequestObject) (api.PostIntentsResponseObject, error) {
	if s.opts.Ports.Intents == nil {
		return nil, errNotWired("trading")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	instrumentID, err := instruments.ParseInstrumentID(request.Body.InstrumentId.String())
	if err != nil || instrumentID.IsZero() {
		return nil, validationError("instrument_id", "instrument_id must be a canonical UUID")
	}
	action := intent.Action(request.Body.Action)
	if !action.Declared() {
		return nil, validationError("action", "unknown action")
	}
	mode := intent.Mode(request.Body.Mode)
	if !mode.Valid() {
		return nil, validationError("mode", "unknown mode")
	}
	notional, quantity, err := parseAmounts(request.Body.NotionalUsd, request.Body.Quantity)
	if err != nil {
		return nil, err
	}
	var target *money.USD
	if request.Body.TargetExposureUsd != nil {
		v, perr := money.ParseUSD(*request.Body.TargetExposureUsd)
		if perr != nil {
			return nil, validationError("target_exposure_usd", "target_exposure_usd must be a decimal string with two fraction digits")
		}
		target = &v
	}
	constraints, err := fromAPIConstraints(request.Body.Constraints)
	if err != nil {
		return nil, err
	}
	deadline := time.Time{}
	if request.Body.Deadline != nil {
		deadline = request.Body.Deadline.UTC()
	}

	routeAmount := ""
	if quantity != nil {
		routeAmount = quantity.String()
	}
	routeNotional := notional
	if routeNotional == nil && quantity == nil && target != nil {
		// TARGET_EXPOSURE states where it wants to end up rather than how much
		// it moves. The target is the conservative stand-in: it is at least as
		// large as the move, so it faces the same rules or stricter ones.
		routeNotional = target
	}

	req := intent.SubmitRequest{
		AccountID:         accountID.String(),
		ActorType:         p.ActorType,
		Action:            action,
		InstrumentID:      instrumentID,
		NotionalUSD:       notional,
		TargetExposureUSD: target,
		Quantity:          quantity,
		Constraints:       constraints,
		Deadline:          deadline,
		RequestedAt:       s.clk.Now(),
		IdempotencyKey:    request.Params.IdempotencyKey,
		CorrelationID:     observability.CorrelationID(ctx),
		Mode:              mode,
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TradeIntent, commandMeta, error) {
			// STAGE 12 / PART XXVI: one place decides whether this may happen
			// at all. See wiring_intents.go. Everything below still runs; this
			// is in front of it, not instead of it.
			//
			// It is INSIDE runCommand because a refusal is an outcome. PART 36
			// makes a business rejection a recorded conclusion, so replaying
			// the key reproduces the refusal instead of asking the policy
			// again -- which is how the same idempotency key ends up with two
			// different answers across a gate activation.
			if rerr := s.routeIntent(ctx, request, accountID, instrumentID, mode, action, routeAmount, routeNotional); rerr != nil {
				return api.TradeIntent{}, commandMeta{}, rerr
			}
			t, serr := s.opts.Ports.Intents.Submit(ctx, p, req)
			if serr != nil {
				return api.TradeIntent{}, commandMeta{}, serr
			}
			return toAPIIntent(t), commandMeta{
				Status:       http.StatusAccepted,
				ResourceType: "trade_intent",
				ResourceID:   t.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostIntents200JSONResponse(res.Value), nil
	}
	return api.PostIntents202JSONResponse(res.Value), nil
}

// routeIntent compiles the trade intent's settlement route and returns the
// refusal, if any. See wiring_intents.go for what decides the action type.
func (s *Server) routeIntent(
	ctx context.Context,
	request api.PostIntentsRequestObject,
	accountID accounts.AccountID,
	instrumentID instruments.InstrumentID,
	mode intent.Mode,
	action intent.Action,
	amount string,
	notional *money.USD,
) error {
	baseAssetID, baseDomain, err := s.intentBase(ctx, instrumentID)
	if err != nil {
		return err
	}
	actionType, err := intentActionType(mode, action, baseDomain)
	if err != nil {
		return err
	}
	_, err = s.opts.Ports.SettlementPolicy.compileRoute(ctx, compileContext{
		Action: actionType,
		Subject: settlement.Subject{
			Type: settlement.SubjectInstrument, ID: instrumentID.String(), AssetID: baseAssetID,
		},
		AccountID:      accountID,
		Amount:         amount,
		NotionalUSD:    notional,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	})
	return err
}

// GetIntents pages an account's intents, newest first.
func (s *Server) GetIntents(ctx context.Context, request api.GetIntentsRequestObject) (api.GetIntentsResponseObject, error) {
	if s.opts.Ports.Intents == nil {
		return nil, errNotWired("trading")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.Intents.ListForAccount(ctx, accountID, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.TradeIntent, 0, len(page.Items))
	for _, t := range page.Items {
		items = append(items, toAPIIntent(t))
	}
	return api.GetIntents200JSONResponse(api.TradeIntentPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// GetIntentsIntentId returns an intent with its linked records.
func (s *Server) GetIntentsIntentId(ctx context.Context, request api.GetIntentsIntentIdRequestObject) (api.GetIntentsIntentIdResponseObject, error) {
	if s.opts.Ports.Intents == nil {
		return nil, errNotWired("trading")
	}
	intentID, err := intent.ParseIntentID(request.IntentId.String())
	if err != nil || intentID.IsZero() {
		return nil, validationError("intentId", "intentId must be a canonical UUID")
	}
	base, err := s.opts.Ports.Intents.Get(ctx, intentID)
	if err != nil {
		return nil, err
	}
	if err := requireIntentScope(ctx, base.AccountID); err != nil {
		return nil, err
	}
	d, err := s.opts.Ports.Intents.Detail(ctx, intentID)
	if err != nil {
		return nil, err
	}
	return api.GetIntentsIntentId200JSONResponse(toAPIIntentDetail(d)), nil
}

// requireIntentScope enforces tenant scoping on a record fetched by its own
// id, for a READ: the caller must own the account the record belongs to, or
// hold the operator read override.
func requireIntentScope(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errs.New(errs.CodeNotFound, "not found")
	}
	return security.RequireAccount(ctx, accountID)
}

// requireIntentScopeWrite is requireIntentScope for a WRITE: ownership only.
//
// The split is F-36's rule applied to a record fetched by its own id.
// account:read_any is a READ permission and RoleAdmin holds it alongside the
// customer surface, so scoping a write through the read helper let one ADMIN
// session cancel any customer's intent with no second signature, no reason and
// no admin action. accountScope/accountScopeWrite already say this for routes
// that take an account id; this pair says it for routes that do not, which is
// how the two survived the fix (F-102).
func requireIntentScopeWrite(ctx context.Context, accountID string) error {
	if accountID == "" {
		return errs.New(errs.CodeNotFound, "not found")
	}
	return security.RequireAccountOwner(ctx, accountID)
}

// PostIntentsIntentIdCancel records a cancellation request. Only external
// confirmation ever yields CANCELLED (PART 227); this endpoint never asserts
// that an in-flight submission did not happen.
func (s *Server) PostIntentsIntentIdCancel(ctx context.Context, request api.PostIntentsIntentIdCancelRequestObject) (api.PostIntentsIntentIdCancelResponseObject, error) {
	if s.opts.Ports.Intents == nil {
		return nil, errNotWired("trading")
	}
	p, ok := security.PrincipalFrom(ctx)
	if !ok {
		return nil, errs.New(errs.CodeUnauthenticated, "authentication is required")
	}
	intentID, err := intent.ParseIntentID(request.IntentId.String())
	if err != nil || intentID.IsZero() {
		return nil, validationError("intentId", "intentId must be a canonical UUID")
	}
	base, err := s.opts.Ports.Intents.Get(ctx, intentID)
	if err != nil {
		return nil, err
	}
	if err := requireIntentScopeWrite(ctx, base.AccountID); err != nil {
		return nil, err
	}
	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.TradeIntent, commandMeta, error) {
			t, cerr := s.opts.Ports.Intents.RequestCancel(ctx, p, intentID, request.Params.IdempotencyKey)
			if cerr != nil {
				return api.TradeIntent{}, commandMeta{}, cerr
			}
			return toAPIIntent(t), commandMeta{
				Status:       http.StatusAccepted,
				ResourceType: "trade_intent",
				ResourceID:   t.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostIntentsIntentIdCancel202JSONResponse(res.Value), nil
}

// GetOrders pages an account's orders.
func (s *Server) GetOrders(ctx context.Context, request api.GetOrdersRequestObject) (api.GetOrdersResponseObject, error) {
	if s.opts.Ports.Orders == nil {
		return nil, errNotWired("execution records")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	page, err := s.opts.Ports.Orders.ListForAccount(ctx, accountID, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.Order, 0, len(page.Items))
	for _, o := range page.Items {
		items = append(items, toAPIOrder(o))
	}
	return api.GetOrders200JSONResponse(api.OrderPage{
		Items:      items,
		NextCursor: nextCursor(page.NextCursor),
	}), nil
}

// GetOrdersOrderId returns an order with its attempts and fills.
func (s *Server) GetOrdersOrderId(ctx context.Context, request api.GetOrdersOrderIdRequestObject) (api.GetOrdersOrderIdResponseObject, error) {
	if s.opts.Ports.Orders == nil {
		return nil, errNotWired("execution records")
	}
	orderID, err := execution.ParseOrderID(request.OrderId.String())
	if err != nil || orderID.IsZero() {
		return nil, validationError("orderId", "orderId must be a canonical UUID")
	}
	d, err := s.opts.Ports.Orders.Detail(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if err := requireOrderScope(ctx, d.Order.AccountID); err != nil {
		return nil, err
	}
	return api.GetOrdersOrderId200JSONResponse(toAPIOrderDetail(d)), nil
}

func requireOrderScope(ctx context.Context, accountID accounts.AccountID) error {
	if accountID.IsZero() {
		return errs.New(errs.CodeNotFound, "not found")
	}
	return security.RequireAccount(ctx, accountID.String())
}
