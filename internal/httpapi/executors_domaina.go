package httpapi

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/payout"
)

// The operator's hands on the internal economy (gola.md PARTS XIII-XXI,
// LXVIII; Stage 17).
//
// Before this, every control in Domain A existed and none of them had an
// operator interface. A market could be halted, an asset delisted, a seller
// suspended and a stuck payout resolved — by running SQL. A control that can
// only be exercised by hand-written SQL is a control with no audit trail, no
// dual-control story and no reason attached, which in an incident is barely a
// control at all.
//
// Each executor below runs inside the savepoint `admin.Execute` opens, after
// the action's approval, step-up freshness, expiry and params hash have
// already been re-verified. What each one adds is the domain call and the
// refusal that belongs to the domain rather than to the approval.
//
// # The asymmetry
//
// Stopping is one operator; restarting is two. That is stated in
// `admin/kinds.go` and it is what the executors here rely on: nothing in this
// file weakens it, and `TestKindTable_Golden` fails if the table moves.

// DomainAExecutorDeps are the services the internal-economy executors drive.
// Each is optional: a nil service leaves its kinds unregistered rather than
// half-wired, so a deployment without the internal economy answers 422
// UNSUPPORTED instead of reporting a success that never happened.
type DomainAExecutorDeps struct {
	NativeAssets  *nativeasset.Service
	NativeMarkets *nativemarket.Service
	Commerce      *commerce.Service
	Payouts       *payout.Service
}

// DomainAExecutors returns the executors for the internal economy.
func DomainAExecutors(d DomainAExecutorDeps) map[admin.Kind]admin.ExecFunc {
	out := make(map[admin.Kind]admin.ExecFunc, 9)
	if d.NativeMarkets != nil {
		out[admin.KindNativeMarketHalt] = marketStatusExecutor(d.NativeMarkets,
			admin.KindNativeMarketHalt, nativemarket.StatusHalted)
		out[admin.KindNativeMarketCloseOnly] = marketStatusExecutor(d.NativeMarkets,
			admin.KindNativeMarketCloseOnly, nativemarket.StatusCloseOnly)
		out[admin.KindNativeMarketFreeze] = marketStatusExecutor(d.NativeMarkets,
			admin.KindNativeMarketFreeze, nativemarket.StatusFrozen)
		out[admin.KindNativeMarketResume] = marketStatusExecutor(d.NativeMarkets,
			admin.KindNativeMarketResume, nativemarket.StatusActive)
	}
	if d.NativeAssets != nil {
		out[admin.KindNativeAssetModerationVerdict] = moderationVerdictExecutor(d.NativeAssets)
		out[admin.KindNativeAssetDelist] = assetDelistExecutor(d.NativeAssets)
	}
	if d.Commerce != nil {
		out[admin.KindCommerceSellerSuspend] = sellerSuspendExecutor(d.Commerce)
		out[admin.KindCommerceProductWithdraw] = productWithdrawExecutor(d.Commerce)
	}
	if d.Payouts != nil {
		out[admin.KindPayoutManualReviewResolve] = payoutManualReviewExecutor(d.Payouts)
	}
	return out
}

// executingAction returns the action being executed, refusing if the executor
// was somehow called for a different kind. The target is read from the STORED
// action, never from the request: an executor that took its target from the
// caller would let an approval for one market be spent on another.
func executingAction(ctx context.Context, want admin.Kind) (admin.Action, error) {
	action, ok := admin.ExecutingAction(ctx)
	if !ok {
		return admin.Action{}, errs.New(errs.CodeInternal,
			"this executor runs only inside Execute")
	}
	if action.Kind != want {
		return admin.Action{}, errs.Newf(errs.CodeInvalidStateTransition,
			"action %s is not a %s", action.Kind, want)
	}
	if strings.TrimSpace(action.Reason) == "" {
		return admin.Action{}, errs.New(errs.CodeValidationFailed,
			"an administrative action on the internal economy must carry a reason")
	}
	return action, nil
}

func encode(v any) (json.RawMessage, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "encode administrative execution result")
	}
	return out, nil
}

// --- native markets ---------------------------------------------------------

type marketStatusResult struct {
	MarketID   string `json:"market_id"`
	AssetID    string `json:"asset_id"`
	From       string `json:"from_status"`
	To         string `json:"to_status"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

// marketStatusExecutor moves one market to a fixed status.
//
// The target status is baked into the executor rather than read from params,
// so an approval for NATIVE_MARKET_CLOSE_ONLY cannot be executed as a freeze.
// The kind IS the decision that was approved.
func marketStatusExecutor(svc *nativemarket.Service, kind admin.Kind, to nativemarket.Status) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, kind)
		if err != nil {
			return nil, err
		}
		marketID, err := nativemarket.ParseMarketID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not a market id")
		}
		before, err := svc.Market(ctx, tx, marketID)
		if err != nil {
			return nil, err
		}
		// The market's own transition table still decides. FROZEN cannot go
		// straight back to ACTIVE, and an approved RESUME does not change
		// that: an economic incident is stepped down through HALTED or
		// CLOSE_ONLY, and no signature shortens that path.
		after, err := svc.SetStatus(ctx, tx, marketID, to, action.Reason)
		if err != nil {
			return nil, err
		}
		return encode(marketStatusResult{
			MarketID: after.ID.String(), AssetID: after.AssetID.String(),
			From: string(before.Status), To: string(after.Status),
			Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}

// --- native assets ----------------------------------------------------------

// moderationVerdictParams is what the proposer wrote down. The params hash is
// re-verified by Execute, so the verdict that runs is exactly the one that was
// approved.
type moderationVerdictParams struct {
	State string `json:"state"`
	Notes string `json:"notes"`
}

type moderationVerdictResult struct {
	AssetID    string `json:"asset_id"`
	From       string `json:"from_state"`
	To         string `json:"to_state"`
	Notes      string `json:"notes"`
	ApprovalID string `json:"approval_id"`
}

// moderationVerdictExecutor records a moderation decision.
//
// Recording APPROVED does not start trading. Activating a market is a separate
// act behind its own capability, and keeping the two apart is what stops a
// content decision from being an economic one.
func moderationVerdictExecutor(svc *nativeasset.Service) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindNativeAssetModerationVerdict)
		if err != nil {
			return nil, err
		}
		var p moderationVerdictParams
		if uerr := json.Unmarshal(params, &p); uerr != nil {
			return nil, errs.Wrap(uerr, errs.CodeValidationFailed,
				"a moderation verdict's params must be {state, notes}")
		}
		state := nativeasset.ModerationState(strings.ToUpper(strings.TrimSpace(p.State)))
		if !state.Valid() {
			return nil, errs.Newf(errs.CodeValidationFailed,
				"unknown moderation state %q", p.State)
		}
		assetID, err := assets.ParseAssetID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not an asset id")
		}
		before, err := svc.Get(ctx, tx, assetID)
		if err != nil {
			return nil, err
		}
		notes := strings.TrimSpace(p.Notes)
		if notes == "" {
			notes = action.Reason
		}
		after, err := svc.SetModeration(ctx, tx, assetID, state, notes)
		if err != nil {
			return nil, err
		}
		return encode(moderationVerdictResult{
			AssetID: after.AssetID.String(), From: string(before.Moderation),
			To: string(after.Moderation), Notes: notes, ApprovalID: action.ID.String(),
		})
	}
}

type assetStatusResult struct {
	AssetID    string `json:"asset_id"`
	From       string `json:"from_status"`
	To         string `json:"to_status"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

// assetDelistExecutor removes an asset from the tradable set permanently.
func assetDelistExecutor(svc *nativeasset.Service) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindNativeAssetDelist)
		if err != nil {
			return nil, err
		}
		assetID, err := assets.ParseAssetID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not an asset id")
		}
		before, err := svc.Get(ctx, tx, assetID)
		if err != nil {
			return nil, err
		}
		after, err := svc.SetStatus(ctx, tx, assetID, nativeasset.StatusDelisted, action.Reason)
		if err != nil {
			return nil, err
		}
		return encode(assetStatusResult{
			AssetID: after.AssetID.String(), From: string(before.Status),
			To: string(after.Status), Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}

// --- internal commerce ------------------------------------------------------

type sellerSuspendResult struct {
	AccountID  string `json:"account_id"`
	Status     string `json:"status"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

// sellerSuspendExecutor stops an account taking new orders.
//
// It does not touch what they have already earned. Suspension stops new
// activity; clawing back a completed sale would be a ledger correction, which
// is a different action with a different approval.
func sellerSuspendExecutor(svc *commerce.Service) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindCommerceSellerSuspend)
		if err != nil {
			return nil, err
		}
		accountID, err := accounts.ParseAccountID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not an account id")
		}
		sel, err := svc.SetSellerStatus(ctx, tx, accountID, commerce.SellerSuspended, action.Reason)
		if err != nil {
			return nil, err
		}
		return encode(sellerSuspendResult{
			AccountID: sel.AccountID.String(), Status: string(sel.Status),
			Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}

type productWithdrawResult struct {
	ProductID  string `json:"product_id"`
	From       string `json:"from_status"`
	To         string `json:"to_status"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

// productWithdrawExecutor takes one product down permanently.
func productWithdrawExecutor(svc *commerce.Service) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, _ json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindCommerceProductWithdraw)
		if err != nil {
			return nil, err
		}
		productID, err := commerce.ParseProductID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not a product id")
		}
		before, err := svc.Product(ctx, tx, productID)
		if err != nil {
			return nil, err
		}
		after, err := svc.SetStatus(ctx, tx, productID, commerce.StatusWithdrawn)
		if err != nil {
			return nil, err
		}
		return encode(productWithdrawResult{
			ProductID: after.ID.String(), From: string(before.Status),
			To: string(after.Status), Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}

// --- payouts ----------------------------------------------------------------

type payoutResolveParams struct {
	Resolution string `json:"resolution"`
}

type payoutResolveResult struct {
	PayoutID   string `json:"payout_id"`
	From       string `json:"from_state"`
	To         string `json:"to_state"`
	Resolution string `json:"resolution"`
	Reason     string `json:"reason"`
	ApprovalID string `json:"approval_id"`
}

// payoutManualReviewExecutor applies a dual-approved decision to a payout
// stuck in MANUAL_REVIEW.
//
// There is no resolution that declares a payout settled. The provider is
// authoritative for that, and an operator who could assert it by hand could
// close a ticket by claiming money moved.
func payoutManualReviewExecutor(svc *payout.Service) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindPayoutManualReviewResolve)
		if err != nil {
			return nil, err
		}
		var p payoutResolveParams
		if uerr := json.Unmarshal(params, &p); uerr != nil {
			return nil, errs.Wrap(uerr, errs.CodeValidationFailed,
				"a payout resolution's params must be {resolution}")
		}
		resolution := payout.ManualResolution(strings.ToUpper(strings.TrimSpace(p.Resolution)))
		if !resolution.Valid() {
			return nil, errs.Newf(errs.CodeValidationFailed,
				"unknown payout resolution %q; a payout is never declared settled by hand", p.Resolution)
		}
		requestID, err := payout.ParseRequestID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not a payout id")
		}
		before, err := svc.Get(ctx, tx, requestID)
		if err != nil {
			return nil, err
		}
		after, err := svc.ResolveManualReview(ctx, tx, requestID, resolution, action.Reason)
		if err != nil {
			return nil, err
		}
		return encode(payoutResolveResult{
			PayoutID: after.ID.String(), From: string(before.State), To: string(after.State),
			Resolution: string(resolution), Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}
