package httpapi

import (
	"context"
	"net/http"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/funding"
	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/observability"
	"github.com/nodal/controlplane/internal/security"
)

// PostFundingDeposits starts a fiat-to-USDC funding session. Creating the
// session credits nothing: a redirect "success" and a provider 200 are not
// settlement (PART 162). The customer's money appears only after a chain
// receipt is observed, reconciled and posted.
func (s *Server) PostFundingDeposits(ctx context.Context, request api.PostFundingDepositsRequestObject) (api.PostFundingDepositsResponseObject, error) {
	if s.opts.Ports.Funding == nil {
		return nil, errNotWired("funding")
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
	amount, err := money.ParseUSD(request.Body.FiatAmount)
	if err != nil {
		return nil, validationError("fiat_amount", "fiat_amount must be a decimal string with two fraction digits")
	}
	if !amount.IsPositive() {
		return nil, validationError("fiat_amount", "fiat_amount must be positive")
	}
	minor := amount.Minor()
	fundingSource := ""
	if request.Body.FundingSourceId != nil {
		fundingSource = request.Body.FundingSourceId.String()
	}
	r, _ := requestFrom(ctx)
	ip := ""
	if r != nil {
		ip = clientIP(r, s.trusted)
	}

	cmd := StartDeposit{
		AccountID:       accountID,
		FiatAmountMinor: &minor,
		FiatCurrency:    string(request.Body.FiatCurrency),
		FundingSourceID: fundingSource,
		CustomerIP:      ip,
		IdempotencyKey:  request.Params.IdempotencyKey,
		CorrelationID:   observability.CorrelationID(ctx),
		RequestID:       observability.RequestID(ctx),
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Deposit, commandMeta, error) {
			out, serr := s.opts.Ports.Funding.Start(ctx, p, cmd)
			if serr != nil {
				return api.Deposit{}, commandMeta{}, serr
			}
			// The provider client secret is returned once, at creation,
			// and is never persisted by this layer.
			return toAPIDeposit(out.Deposit, out.Session.ClientSecret), commandMeta{
				Status:       http.StatusAccepted,
				ResourceType: "deposit",
				ResourceID:   out.Deposit.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	if res.Replayed {
		return api.PostFundingDeposits200JSONResponse(res.Value), nil
	}
	return api.PostFundingDeposits202JSONResponse(res.Value), nil
}

// GetFundingDeposits pages an account's deposits.
func (s *Server) GetFundingDeposits(ctx context.Context, request api.GetFundingDepositsRequestObject) (api.GetFundingDepositsResponseObject, error) {
	if s.opts.Ports.Funding == nil {
		return nil, errNotWired("funding")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	cursor := ""
	if request.Params.Cursor != nil {
		cursor = *request.Params.Cursor
	}
	list, next, err := s.opts.Ports.Funding.List(ctx, accountID, cursor,
		pageLimit(request.Params.Limit, defaultPageLimit, maxPageLimit))
	if err != nil {
		return nil, err
	}
	items := make([]api.Deposit, 0, len(list))
	for _, d := range list {
		items = append(items, toAPIDeposit(d, ""))
	}
	return api.GetFundingDeposits200JSONResponse(api.DepositPage{
		Items:      items,
		NextCursor: nextCursor(next),
	}), nil
}

// GetFundingDepositsDepositId returns a deposit with its transition history.
func (s *Server) GetFundingDepositsDepositId(ctx context.Context, request api.GetFundingDepositsDepositIdRequestObject) (api.GetFundingDepositsDepositIdResponseObject, error) {
	if s.opts.Ports.Funding == nil {
		return nil, errNotWired("funding")
	}
	depositID, err := funding.ParseDepositID(request.DepositId.String())
	if err != nil || depositID.IsZero() {
		return nil, validationError("depositId", "depositId must be a canonical UUID")
	}
	d, err := s.opts.Ports.Funding.Detail(ctx, depositID)
	if err != nil {
		return nil, err
	}
	if err := security.RequireAccount(ctx, d.Deposit.AccountID.String()); err != nil {
		return nil, err
	}
	return api.GetFundingDepositsDepositId200JSONResponse(toAPIDepositDetail(d)), nil
}

// PostWithdrawals records a withdrawal request. Every guard lives in
// internal/withdrawal: a human actor, the account permission, a recent
// step-up, the WITHDRAWALS capability gate (DISABLED in every environment
// today), kill switches, destination validation and the velocity policy. This
// handler decides none of them, and no code path here moves value.
func (s *Server) PostWithdrawals(ctx context.Context, request api.PostWithdrawalsRequestObject) (api.PostWithdrawalsResponseObject, error) {
	if s.opts.Ports.Withdrawals == nil {
		return nil, errNotWired("withdrawals")
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
	assetID, err := assets.ParseAssetID(request.Body.AssetId.String())
	if err != nil || assetID.IsZero() {
		return nil, validationError("asset_id", "asset_id must be a canonical UUID")
	}
	qty, err := money.ParseQuantity(request.Body.Quantity)
	if err != nil {
		return nil, validationError("quantity", "quantity must be an exact integer base-unit string")
	}

	req := WithdrawalRequest{
		AccountID:          accountID,
		AssetID:            assetID,
		Quantity:           qty,
		DestinationAddress: request.Body.DestinationAddress,
		IdempotencyKey:     request.Params.IdempotencyKey,
		CorrelationID:      observability.CorrelationID(ctx),
		RequestID:          observability.RequestID(ctx),
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.Withdrawal, commandMeta, error) {
			w, werr := s.opts.Ports.Withdrawals.Request(ctx, p, req)
			if werr != nil {
				return api.Withdrawal{}, commandMeta{}, werr
			}
			out := api.Withdrawal{
				Id:                 toUUID(w.ID),
				AccountId:          toUUID(w.AccountID),
				AssetId:            toUUID(w.AssetID),
				Quantity:           w.Quantity.String(),
				DestinationAddress: w.DestinationAddress,
				Status:             string(w.Status),
				CreatedAt:          w.CreatedAt.UTC(),
			}
			return out, commandMeta{
				Status:       http.StatusAccepted,
				ResourceType: "withdrawal",
				ResourceID:   w.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostWithdrawals202JSONResponse(res.Value), nil
}
