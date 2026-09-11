package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/eligibility"
	"github.com/nodal/controlplane/internal/gen/api"
)

// GetMeEligibility explains withdrawal eligibility (goal §19, §23).
//
// It is the answer to the question §19 puts at the centre of the withdraw
// experience: "calculate conversion eligibility; show eligible / ineligible
// value". Two properties of the response matter more than its contents:
//
//  1. It is broken down by ORIGIN, not by balance. Two people holding the same
//     number of Credits can have entirely different withdrawable amounts, and
//     that is correct: provenance survives trading (§23), and a promotional
//     grant traded into a market gain is still a promotional grant's worth of
//     value under a policy that says so.
//  2. Every reason names something that could change. REQUIRES_VERIFICATION is
//     a next step; CAPABILITY_INACTIVE and PROVIDER_UNAVAILABLE are the
//     platform's state and not the person's fault; ORIGIN_NOT_WITHDRAWABLE is
//     a policy decision about what the value IS.
//
// It moves nothing, reserves nothing, and is safe to call as often as a screen
// needs it.
func (s *Server) GetMeEligibility(ctx context.Context, request api.GetMeEligibilityRequestObject) (api.GetMeEligibilityResponseObject, error) {
	if s.opts.Ports.Eligibility == nil {
		return nil, errNotWired("withdrawal eligibility")
	}
	accountID, err := accountScope(ctx, request.Params.AccountId)
	if err != nil {
		return nil, err
	}
	explanation, err := s.opts.Ports.Eligibility.Withdrawal(ctx, accountID)
	if err != nil {
		return nil, err
	}
	out := toAPIWithdrawalEligibility(explanation)
	out.AccountId = request.Params.AccountId
	return api.GetMeEligibility200JSONResponse(out), nil
}

func toAPIWithdrawalEligibility(e eligibility.WithdrawalExplanation) api.WithdrawalEligibility {
	out := api.WithdrawalEligibility{
		Eligible:              e.Eligible,
		WithdrawableNow:       qtyString(e.WithdrawableNow),
		Gross:                 qtyString(e.Gross),
		Spendable:             qtyString(e.Spendable),
		Frozen:                qtyString(e.Frozen),
		PayoutEligible:        qtyString(e.PayoutEligible),
		Ineligible:            qtyString(e.Ineligible),
		Reasons:               toAPIWithdrawalReasons(e.Reasons),
		CurrentVerification:   api.VerificationLevel(e.CurrentVerification),
		RequiredVerification:  api.VerificationLevel(e.RequiredVerification),
		PolicyVersion:         e.PolicyVersion,
		Sandbox:               e.Sandbox,
		DestinationConfigured: ptr(e.DestinationConfigured),
		JurisdictionSupported: ptr(e.JurisdictionSupported),
		ProviderAvailable:     ptr(e.ProviderAvailable),
		MinimumQuantity:       ptr(qtyString(e.MinimumQuantity)),
	}
	if e.VerificationWouldSuffice {
		out.VerificationWouldSuffice = ptr(true)
	}
	if e.PolicyHash != "" {
		out.PolicyHash = ptr(e.PolicyHash)
	}
	if e.ProviderName != "" {
		out.Provider = ptr(e.ProviderName)
	}
	buckets := make([]api.WithdrawalOriginBucket, 0, len(e.Buckets))
	for _, b := range e.Buckets {
		item := api.WithdrawalOriginBucket{
			Origin:          api.CreditOrigin(b.Origin),
			Quantity:        qtyString(b.Quantity),
			Withdrawable:    qtyString(b.Withdrawable),
			Reasons:         toAPIWithdrawalReasons(b.Reasons),
			PayoutAllowed:   b.PayoutAllowed,
			ConsumptionRank: credit.ConsumptionRank(b.Origin),
			MinHoldDays:     ptr(b.MinHoldDays),
		}
		if b.RequiredVerification != "" {
			item.RequiredVerification = ptr(api.VerificationLevel(b.RequiredVerification))
		}
		if b.RequiredCapability != "" {
			item.RequiredCapability = ptr(string(b.RequiredCapability))
		}
		if b.VerificationWouldSuffice {
			item.VerificationWouldSuffice = ptr(true)
		}
		buckets = append(buckets, item)
	}
	out.Buckets = buckets
	return out
}

func toAPIWithdrawalReasons(in []eligibility.WithdrawalReason) []api.WithdrawalReason {
	out := make([]api.WithdrawalReason, 0, len(in))
	for _, r := range in {
		out = append(out, api.WithdrawalReason(r))
	}
	return out
}
