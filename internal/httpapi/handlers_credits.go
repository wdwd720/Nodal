package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
)

// GetCreditsPricing returns the policy that converts money into Credits.
//
// It exists so that a funding page can show "$100 buys 10,000 Credits" without
// implementing that arithmetic itself. Two implementations would eventually
// disagree, and the one that issues is this one.
func (s *Server) GetCreditsPricing(ctx context.Context, _ api.GetCreditsPricingRequestObject) (api.GetCreditsPricingResponseObject, error) {
	if s.opts.Ports.Credits == nil {
		return nil, errNotWired("credits")
	}
	p, err := s.opts.Ports.Credits.Pricing(ctx)
	if err != nil {
		return nil, err
	}
	// Every term of the conversion, not just the rate.
	//
	// A client given credits_per_major_unit alone cannot compute what it is
	// about to be charged for, and the Buy Credits page proved it: it rendered
	// "100 Credits per 1 USD" from this field over a server whose arithmetic
	// omitted the asset scale entirely, and then rendered the result -- 0.001
	// Credits -- from a scale it had hardcoded (F-151). The scale, the minor
	// unit and the rounding are the rest of the function, and they are
	// published so a page can show the same number the server will issue.
	return api.GetCreditsPricing200JSONResponse(api.CreditPricing{
		Version:                p.Version,
		Currency:               p.Currency,
		CreditsPerMajorUnit:    p.CreditsPerMajorUnit,
		MinorUnitsPerMajorUnit: p.MinorUnitsPerMajorUnit,
		Decimals:               int(p.Decimals),
		Rounding:               api.CreditPricingRounding(p.Rounding.String()),
		MinAmountMinor:         p.MinAmountMinor,
		MaxAmountMinor:         p.MaxAmountMinor,
	}), nil
}

// PostPayments starts a Credit purchase.
//
// The request body carries an amount of money and no Credit quantity, because
// the schema has no such field. That is the enforcement: not a validation that
// could be forgotten, but the absence of anywhere for the claim to be written.
func (s *Server) PostPayments(ctx context.Context, request api.PostPaymentsRequestObject) (api.PostPaymentsResponseObject, error) {
	if s.opts.Ports.Credits == nil {
		return nil, errNotWired("credits")
	}
	if request.Body == nil {
		return nil, validationError("body", "a request body is required")
	}
	accountID, err := accountScopeWrite(ctx, request.Body.AccountId)
	if err != nil {
		return nil, err
	}
	if request.Body.AmountMinor <= 0 {
		return nil, validationError("amount_minor", "the amount must be positive")
	}
	// Bounded here, before any guard measures anything against it (F-159).
	//
	// internal/capacity compares headroom rather than summing, so it can no
	// longer be overflowed -- but a ceiling that is only safe because the
	// number reaching it happens to be small is not safe, and the bound the
	// deployment publishes is the honest one to apply. It is the SAME number
	// GET /v1/credits/pricing states, read from the policy rather than
	// repeated here, so a client is refused by the figure it was shown instead
	// of by a constant nobody published.
	pricing, err := s.opts.Ports.Credits.Pricing(ctx)
	if err != nil {
		return nil, err
	}
	if request.Body.AmountMinor > pricing.MaxAmountMinor {
		return nil, validationError("amount_minor",
			"the largest single purchase this deployment sells is "+money.USDFromMinor(pricing.MaxAmountMinor).String())
	}
	currency := "USD"
	if request.Body.Currency != nil {
		currency = *request.Body.Currency
	}

	cmd := StartCreditPurchase{
		AccountID: accountID, AmountMinor: request.Body.AmountMinor, Currency: currency,
		IdempotencyKey: request.Params.IdempotencyKey,
		CorrelationID:  observability.CorrelationID(ctx),
	}

	// The client secret is captured here rather than returned through
	// runCommand, and that placement is the point. runCommand persists what it
	// returns as the idempotency record; a secret that authorises confirming a
	// payment must not be stored, and must not be handed to whoever replays
	// the request later. It is attached to the 201 only.
	var clientSecret string

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.CreditPurchase, commandMeta, error) {
			started, cerr := s.opts.Ports.Credits.StartPurchase(ctx, cmd)
			if cerr != nil {
				return api.CreditPurchase{}, commandMeta{}, cerr
			}
			clientSecret = started.ClientSecret
			return toAPICreditPurchase(started.Funding), commandMeta{
				Status:       http.StatusCreated,
				ResourceType: "credit_purchase",
				ResourceID:   started.Funding.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostPayments200JSONResponse(res.Value), nil
	}
	out := res.Value
	if clientSecret != "" {
		out.ClientSecret = &clientSecret
	}
	return api.PostPayments201JSONResponse(out), nil
}

// GetPaymentsPaymentId reads one purchase.
func (s *Server) GetPaymentsPaymentId(ctx context.Context, request api.GetPaymentsPaymentIdRequestObject) (api.GetPaymentsPaymentIdResponseObject, error) {
	if s.opts.Ports.Credits == nil {
		return nil, errNotWired("credits")
	}
	id, err := credit.ParseFundingID(request.PaymentId.String())
	if err != nil {
		return nil, validationError("paymentId", "paymentId must be a canonical UUID")
	}
	f, err := s.opts.Ports.Credits.Purchase(ctx, id)
	if err != nil {
		return nil, err
	}
	if _, err := accountScope(ctx, uuid.MustParse(f.AccountID.String())); err != nil {
		return nil, err
	}
	return api.GetPaymentsPaymentId200JSONResponse(toAPICreditPurchase(f)), nil
}

// toAPICreditPurchase renders a funding for the API.
//
// It never renders a client secret. The secret is not stored on the funding at
// all, so there is nothing here that could leak it -- which is a stronger
// guarantee than remembering to omit a field.
//
// The sandbox flag comes from the FUNDING, not from the deployment. It used to
// be a boolean the composition root computed once at startup from the current
// provider configuration, so every funding this endpoint returned carried
// today's answer to a question about the past: a deployment promoted from
// sandbox to live re-labelled every sandbox purchase it had ever made as real
// value, here and in the browser's temperature badge (F-158). A funding with no
// recorded mode predates migration 00793 and is rendered as sandbox, because an
// unrecorded mode cannot be asserted to be real money.
func toAPICreditPurchase(f credit.Funding) api.CreditPurchase {
	sandbox := credit.SandboxMode(f.ProviderMode)
	out := api.CreditPurchase{
		Sandbox:        &sandbox,
		PurchaseId:     uuid.MustParse(f.ID.String()),
		AccountId:      uuid.MustParse(f.AccountID.String()),
		State:          api.CreditPurchaseState(f.State),
		CreditQuantity: f.CreditQuantity.String(),
		AmountMinor:    f.PaidAmount.Minor(),
		Currency:       f.PaidCurrency,
		Provider:       f.Provider,
		CreatedAt:      &f.CreatedAt,
	}
	if f.ReversibleAt != nil {
		out.ReversibleAt = f.ReversibleAt
	}
	if f.SettledAt != nil {
		out.SettledAt = f.SettledAt
	}
	if f.FailureReason != "" {
		out.FailureReason = &f.FailureReason
	}
	if f.ProviderMode != "" {
		mode := api.CreditPurchaseProviderMode(f.ProviderMode)
		out.ProviderMode = &mode
	}
	return out
}
