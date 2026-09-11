package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/nodal/controlplane/internal/gen/api"
	"github.com/nodal/controlplane/internal/payout"
)

// PostPayoutsPayoutIdCancel withdraws the caller's own payout request.
//
// It exists because `payout.Cancel` had no caller outside tests. A user could
// request a payout, have their Credits reserved out of their spendable
// balance, and have no way to get them back: only an operator could, and only
// by running their own tool against the database. Reserving somebody's money
// with no path to release it is not a conservative control, it is a trap.
//
// A SUBMITTED request cannot be cancelled. It may already have been paid, and
// the only honest way out of that is reconciliation — `payout.Cancel` refuses
// it and says so.
//
// # Its kill-switch class, declared and deliberately not consulted
//
// Every guarded operation declares an ActionClass (POLICY_AUTHORITY §2). A
// cancellation is `killswitch.Cancel`, which that section and
// `killswitch.ActionClass.NeverBlocked` hold is exempt from every switch, so
// this path consults none and `Checker.Check` would return for it without
// touching the database. That is a decision, not an omission: cancelling
// returns the user's reserved Credits to the exact lots they came from, and a
// switch that stopped it would hold somebody's money in PAYOUT_RESERVED for the
// duration of an incident — the trap this endpoint was written to close. The
// sibling paths that OPEN or ADVANCE a conversion request declare `WITHDRAW`
// and are stopped by WITHDRAWALS_DISABLE, GLOBAL_NEW_RISK_KILL and
// ACCOUNT_FREEZE, inside their own transactions (D-092).
func (s *Server) PostPayoutsPayoutIdCancel(ctx context.Context, request api.PostPayoutsPayoutIdCancelRequestObject) (api.PostPayoutsPayoutIdCancelResponseObject, error) {
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
	payoutID, err := payout.ParseRequestID(request.PayoutId.String())
	if err != nil || payoutID.IsZero() {
		return nil, validationError("payoutId", "payoutId must be a canonical UUID")
	}
	reason := strings.TrimSpace(request.Body.Reason)
	if reason == "" {
		return nil, validationError("reason", "cancelling a payout requires a reason")
	}

	res, err := runCommand(ctx, s, request.Params.IdempotencyKey,
		func(ctx context.Context) (api.PayoutRequest, commandMeta, error) {
			req, cerr := s.opts.Ports.Payouts.Cancel(ctx, accountID, payoutID, reason)
			if cerr != nil {
				return api.PayoutRequest{}, commandMeta{}, cerr
			}
			// A cancellation carries no eligibility decision: nothing was
			// decided about whether the money MAY leave, only that the
			// request is withdrawn. An empty decision here is the honest
			// shape, not a missing one.
			return toAPIPayout(req, payout.Decision{}), commandMeta{
				Status:       http.StatusOK,
				ResourceType: "payout_request",
				ResourceID:   req.ID.String(),
			}, nil
		})
	if err != nil {
		return nil, err
	}
	return api.PostPayoutsPayoutIdCancel200JSONResponse(res.Value), nil
}
