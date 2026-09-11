package httpapi

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/gen/api"
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
	return api.GetCreditsPricing200JSONResponse(api.CreditPricing{
		Version:             p.Version,
		Currency:            p.Currency,
		CreditsPerMajorUnit: p.CreditsPerMajorUnit,
		MinAmountMinor:      p.MinAmountMinor,
		MaxAmountMinor:      p.MaxAmountMinor,
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
			return toAPICreditPurchase(started.Funding, s.opts.CreditPurchaseSandbox), commandMeta{
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
	return api.GetPaymentsPaymentId200JSONResponse(toAPICreditPurchase(f, s.opts.CreditPurchaseSandbox)), nil
}

// toAPICreditPurchase renders a funding for the API.
//
// It never renders a client secret. The secret is not stored on the funding at
// all, so there is nothing here that could leak it -- which is a stronger
// guarantee than remembering to omit a field.
func toAPICreditPurchase(f credit.Funding, sandbox bool) api.CreditPurchase {
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
	return out
}
