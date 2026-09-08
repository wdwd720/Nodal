package httpapi

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/admin"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/commerce"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
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
	// Credits resolves the deployment's Credit asset, which a market is priced
	// in. Without it NATIVE_MARKET_LAUNCH stays unregistered: a market priced
	// in nothing is not a market.
	Credits CreditAssetResolver
}

// DomainAExecutors returns the executors for the internal economy.
func DomainAExecutors(d DomainAExecutorDeps) map[admin.Kind]admin.ExecFunc {
	out := make(map[admin.Kind]admin.ExecFunc, 10)
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
	if d.NativeMarkets != nil && d.NativeAssets != nil && d.Credits != nil {
		out[admin.KindNativeMarketLaunch] = marketLaunchExecutor(d.NativeAssets, d.NativeMarkets, d.Credits)
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

// --- launching a market -----------------------------------------------------

// marketLaunchParams are the economics the proposer wrote down. Execute
// re-verifies the params hash, so the market that opens has exactly the curve
// and fees that were approved — not the ones somebody typed at execution time.
type marketLaunchParams struct {
	// VirtualCreditReserve sets the opening price: the first unit costs
	// roughly VirtualCreditReserve / PoolSupply Credits.
	VirtualCreditReserve string `json:"virtual_credit_reserve"`
	PlatformFeeBPS       int    `json:"platform_fee_bps"`
	CreatorFeeBPS        int    `json:"creator_fee_bps"`
}

type marketLaunchResult struct {
	AssetID              string `json:"asset_id"`
	MarketID             string `json:"market_id"`
	AssetFrom            string `json:"asset_from_status"`
	AssetTo              string `json:"asset_to_status"`
	MarketStatus         string `json:"market_status"`
	VirtualCreditReserve string `json:"virtual_credit_reserve"`
	PoolSupply           string `json:"pool_supply"`
	PlatformFeeBPS       int    `json:"platform_fee_bps"`
	CreatorFeeBPS        int    `json:"creator_fee_bps"`
	Reason               string `json:"reason"`
	ApprovalID           string `json:"approval_id"`
}

// marketLaunchExecutor activates an approved asset and opens its market.
//
// It exists because the chain from "a creator made an asset" to "a market
// trades it" had no middle in any deployment. `nativeasset.Activate` and
// `nativemarket.Create` were reachable only from tests, and the executor that
// records a moderation verdict says in its own comment that "activating a
// market is a separate act" — an act nothing implemented. The trading half of
// Domain A could not be started (F-28).
//
// # What it will not do
//
//   - It will not launch an asset moderation has not approved. `Activate`
//     refuses that itself; this refuses it earlier so the operator gets the
//     moderation state in the error rather than a transition failure.
//   - It will not launch a DRAFT. PENDING_REVIEW means the CREATOR submitted
//     it, which is what freezes the economics for review; launching a draft
//     would mean launching something its creator was still editing.
//   - It will not mint twice. `nativemarket.Create` is keyed by the approval
//     id, so one approval opens one market however many times it is executed,
//     and a second approval for an asset that already has one gets that market
//     back rather than a second mint.
//
// The order matters: the asset goes ACTIVE first, because `Create` mints the
// entire supply and an asset that is not live must not have units in
// existence. If the mint fails, the savepoint takes the activation with it.
func marketLaunchExecutor(assetsSvc *nativeasset.Service, markets *nativemarket.Service, creditAsset CreditAssetResolver) admin.ExecFunc {
	return func(ctx context.Context, tx pgx.Tx, params json.RawMessage) (json.RawMessage, error) {
		action, err := executingAction(ctx, admin.KindNativeMarketLaunch)
		if err != nil {
			return nil, err
		}
		var p marketLaunchParams
		if uerr := json.Unmarshal(params, &p); uerr != nil {
			return nil, errs.Wrap(uerr, errs.CodeValidationFailed,
				"a market launch's params must be {virtual_credit_reserve, platform_fee_bps, creator_fee_bps}")
		}
		reserve, perr := money.ParseQuantity(strings.TrimSpace(p.VirtualCreditReserve))
		if perr != nil || !reserve.IsPositive() {
			return nil, errs.New(errs.CodeValidationFailed,
				"virtual_credit_reserve must be a positive integer string of Credit base units; "+
					"it sets the opening price and there is no default worth guessing")
		}
		assetID, err := assets.ParseAssetID(action.TargetID)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeValidationFailed,
				"this action's target is not an asset id")
		}
		asset, err := assetsSvc.Get(ctx, tx, assetID)
		if err != nil {
			return nil, err
		}
		if !asset.Moderation.PermitsActivation() {
			return nil, errs.Newf(errs.CodeForbidden,
				"an asset whose moderation state is %s cannot be launched", asset.Moderation).
				WithField("asset_id", assetID.String()).
				WithField("moderation_state", string(asset.Moderation))
		}
		if asset.Status != nativeasset.StatusPendingReview && asset.Status != nativeasset.StatusActive {
			return nil, errs.Newf(errs.CodeInvalidStateTransition,
				"only an asset its creator has submitted for review can be launched; this one is %s", asset.Status).
				WithField("asset_id", assetID.String()).
				WithField("status", string(asset.Status))
		}
		creditAssetID, err := creditAsset.AssetID(ctx, tx)
		if err != nil {
			return nil, err
		}

		before := asset.Status
		live, err := assetsSvc.Activate(ctx, tx, assetID, action.Reason)
		if err != nil {
			return nil, err
		}
		market, err := markets.Create(ctx, tx, nativemarket.CreateRequest{
			AssetID:              assetID,
			CreditAssetID:        creditAssetID,
			CreatorID:            asset.CreatorAccountID,
			PoolSupply:           asset.Supply.PoolSupply(),
			CreatorAllocation:    asset.Supply.CreatorAllocation,
			TreasuryAllocation:   asset.Supply.TreasuryAllocation,
			VirtualCreditReserve: reserve,
			Fees: nativemarket.Fees{
				PlatformBPS: money.BPS(p.PlatformFeeBPS),
				CreatorBPS:  money.BPS(p.CreatorFeeBPS),
			},
			// Keyed by the APPROVAL, so one approval mints one supply.
			IdempotencyKey: "native_market_launch:" + action.ID.String(),
			EffectiveAt:    activatedAt(live),
		})
		if err != nil {
			return nil, err
		}
		open, err := markets.SetStatus(ctx, tx, market.ID, nativemarket.StatusActive, action.Reason)
		if err != nil {
			return nil, err
		}
		return encode(marketLaunchResult{
			AssetID: assetID.String(), MarketID: open.ID.String(),
			AssetFrom: string(before), AssetTo: string(live.Status),
			MarketStatus:         string(open.Status),
			VirtualCreditReserve: reserve.String(),
			PoolSupply:           asset.Supply.PoolSupply().String(),
			PlatformFeeBPS:       p.PlatformFeeBPS, CreatorFeeBPS: p.CreatorFeeBPS,
			Reason: action.Reason, ApprovalID: action.ID.String(),
		})
	}
}

// activatedAt is the instant the asset went live, which is what the mint is
// dated by. It cannot be nil after a successful Activate; the fallback exists
// so a future change to that cannot silently date the mint at the zero time.
func activatedAt(a nativeasset.Asset) time.Time {
	if a.ActivatedAt != nil {
		return a.ActivatedAt.UTC()
	}
	return time.Now().UTC()
}

// CreditAssetResolver reports the deployment's Credit asset. internal/credit
// supplies it; a deployment without one cannot launch a market, which is the
// correct answer rather than a market priced in nothing.
type CreditAssetResolver interface {
	AssetID(ctx context.Context, q db.Querier) (assets.AssetID, error)
}
