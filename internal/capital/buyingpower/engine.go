package buyingpower

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/clock"
	"github.com/nodal/controlplane/internal/db"
	"github.com/nodal/controlplane/internal/errs"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/valuation"
)

// EngineVersion is folded into every policy_version so a change in the
// arithmetic is visible on every recorded decision.
const EngineVersion = "buyingpower/1"

// Deps are the engine's collaborators. Every field is required except
// Assets and Valuer, which default to the standard implementations.
type Deps struct {
	Clock          clock.Clock
	Policies       valuation.PolicyReader
	Prices         valuation.PriceSource
	KillSwitches   KillSwitchReader
	Reconciliation ReconciliationBlockReader
	// QuoteAssetID is the USD-pegged registry asset that prices are quoted
	// in and that face values are expressed in (V1: USDC).
	QuoteAssetID assets.AssetID
	Assets       *assets.Repository
	Valuer       *valuation.Valuer
}

// Engine computes buying power. It is safe for concurrent use and holds no
// per-account state.
type Engine struct {
	clk            clock.Clock
	policies       valuation.PolicyReader
	prices         valuation.PriceSource
	killSwitches   KillSwitchReader
	reconciliation ReconciliationBlockReader
	quoteAssetID   assets.AssetID
	assets         *assets.Repository
	valuer         *valuation.Valuer
}

// NewEngine validates deps and returns an Engine.
func NewEngine(d Deps) (*Engine, error) {
	var missing []string
	if d.Clock == nil {
		missing = append(missing, "Clock")
	}
	if d.Policies == nil {
		missing = append(missing, "Policies")
	}
	if d.Prices == nil {
		missing = append(missing, "Prices")
	}
	if d.KillSwitches == nil {
		missing = append(missing, "KillSwitches")
	}
	if d.Reconciliation == nil {
		missing = append(missing, "Reconciliation")
	}
	if d.QuoteAssetID.IsZero() {
		missing = append(missing, "QuoteAssetID")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("buyingpower: NewEngine: missing dependencies: %s", strings.Join(missing, ", "))
	}
	if d.Assets == nil {
		d.Assets = assets.NewRepository()
	}
	if d.Valuer == nil {
		d.Valuer = valuation.NewValuer()
	}
	return &Engine{
		clk: d.Clock, policies: d.Policies, prices: d.Prices, killSwitches: d.KillSwitches, reconciliation: d.Reconciliation,
		quoteAssetID: d.QuoteAssetID, assets: d.Assets, valuer: d.Valuer,
	}, nil
}

// Compute implements the fixed contract
// capital.BuyingPowerEngine.Compute(ctx, q, accountID string, purpose):
// it loads a Snapshot and evaluates it. Nothing is cached.
func (e *Engine) Compute(ctx context.Context, q db.Querier, accountID string, purpose Purpose) (BuyingPower, error) {
	aid, err := accounts.ParseAccountID(accountID)
	if err != nil || aid.IsZero() {
		return BuyingPower{}, errs.New(errs.CodeValidationFailed, "account_id must be a canonical uuid")
	}
	return e.ComputeFor(ctx, q, aid, purpose)
}

// ComputeFor is Compute with a typed account id.
func (e *Engine) ComputeFor(ctx context.Context, q db.Querier, accountID accounts.AccountID, purpose Purpose) (BuyingPower, error) {
	if !purpose.Valid() {
		return BuyingPower{}, errs.Newf(errs.CodeValidationFailed, "unknown purpose %q", purpose)
	}
	snap, err := e.LoadSnapshot(ctx, q, accountID)
	if err != nil {
		return BuyingPower{}, err
	}
	return e.Evaluate(ctx, q, snap, purpose)
}

// assetWork is the per-asset accumulator of Evaluate.
type assetWork struct {
	asset  assets.Asset
	policy valuation.AssetPolicy
	cls    valuation.Classification
	price  *money.Price
	// unvalued is set (to a StatusX constant) when the asset cannot be
	// marked at all; reason is the matching haircut reason.
	unvalued string
	reason   string
}

// Evaluate computes the output from a Snapshot. Policies and prices are
// read through q (the fakes ignore it). The result is deterministic for
// identical inputs and the same clock reading.
func (e *Engine) Evaluate(ctx context.Context, q db.Querier, snap Snapshot, purpose Purpose) (BuyingPower, error) {
	if !purpose.Valid() {
		return BuyingPower{}, errs.Newf(errs.CodeValidationFailed, "unknown purpose %q", purpose)
	}
	if !snap.QuoteAsset.IsStablecoin || snap.QuoteAsset.PegCurrency != valuation.USDPeg || snap.QuoteAsset.ID.IsZero() {
		return BuyingPower{}, errs.New(errs.CodeInternal, "buyingpower: quote asset must be a USD-pegged stablecoin")
	}
	now := e.clk.Now()
	out := BuyingPower{Purpose: purpose, AsOf: now}
	out.Restrictions = accountRestrictions(snap)

	var (
		portfolio, buying, available, reserved, pending, funding, withdrawals money.USD
		policyLines                                                           []string
	)
	for _, aid := range snap.assetIDs() {
		a, ok := snap.Assets[aid]
		if !ok {
			return BuyingPower{}, errs.New(errs.CodeInternal, "buyingpower: snapshot lacks asset metadata").WithField("asset_id", aid.String())
		}
		w, err := e.classify(ctx, q, snap, a, now)
		if err != nil {
			return BuyingPower{}, err
		}
		policyLines = append(policyLines, aid.String()+"="+w.policy.PolicyVersion)

		balance := snap.Balances[aid]
		held := balance.IsPositive()
		if w.unvalued != "" {
			if held {
				out.UnderlyingBalances = append(out.UnderlyingBalances, UnderlyingBalance{
					AssetID: aid, Symbol: a.Symbol, Decimals: a.Decimals, Quantity: balance, Status: w.unvalued,
				})
				out.Restrictions = append(out.Restrictions, assetRestriction(w, aid))
			}
			out.Haircuts = append(out.Haircuts, Haircut{AssetID: aid, FactorBPS: 0, Reason: w.reason})
			continue
		}

		mark := func(qty money.Quantity) (valuation.Mark, error) {
			return e.valuer.Mark(a, w.policy, qty, w.price, snap.QuoteAsset.Decimals)
		}
		full, err := mark(balance)
		if err != nil {
			return BuyingPower{}, err
		}
		if held {
			out.UnderlyingBalances = append(out.UnderlyingBalances, UnderlyingBalance{
				AssetID: aid, Symbol: a.Symbol, Decimals: a.Decimals, Quantity: balance,
				USDValue: full.Value, PriceRef: full.PriceRef, Status: balanceStatus(w),
			})
		}
		if portfolio, err = add(portfolio, full.Value); err != nil {
			return BuyingPower{}, err
		}

		net := balance.Sub(snap.Reserved[aid]).Sub(snap.Holds[aid])
		if net.IsNegative() {
			net = money.Quantity{}
		}
		netMark, err := mark(net)
		if err != nil {
			return BuyingPower{}, err
		}
		if buying, err = add(buying, netMark.Contribution); err != nil {
			return BuyingPower{}, err
		}
		if isSettlementAsset(a) {
			if available, err = add(available, netMark.Contribution); err != nil {
				return BuyingPower{}, err
			}
		}
		for _, acc := range []struct {
			dst *money.USD
			qty money.Quantity
		}{
			{&reserved, snap.Reserved[aid]},
			{&pending, snap.Pending[aid]},
			{&funding, snap.EligibleFunding[aid]},
			{&withdrawals, snap.Withdrawals[aid]},
		} {
			if !acc.qty.IsPositive() {
				continue
			}
			m, err := mark(acc.qty)
			if err != nil {
				return BuyingPower{}, err
			}
			if *acc.dst, err = add(*acc.dst, m.Value); err != nil {
				return BuyingPower{}, err
			}
		}
		if w.cls.Gate != valuation.GateEligible || w.cls.Factor < money.OneHundredPercent {
			out.Haircuts = append(out.Haircuts, Haircut{AssetID: aid, FactorBPS: w.cls.Factor, Reason: w.cls.Reason})
		}
	}

	// Account-scoped restrictions remove all buying power; any restriction
	// removes withdrawability. Figures do not depend on purpose.
	accountBlocked := false
	for _, r := range out.Restrictions {
		if r.Scope == ScopeAccount {
			accountBlocked = true
		}
	}
	if accountBlocked {
		buying, available = money.USD{}, money.USD{}
	}
	withdrawable, err := computeWithdrawable(available, funding, withdrawals)
	if err != nil {
		return BuyingPower{}, err
	}
	if len(out.Restrictions) > 0 {
		withdrawable = money.USD{}
	}
	for i := range out.Restrictions {
		out.Restrictions[i].Blocking = blocks(purpose, out.Restrictions[i].Scope)
	}

	out.PortfolioValue, out.BuyingPower, out.AvailableNow = portfolio, buying, available
	out.Reserved, out.Pending, out.Withdrawable = reserved, pending, withdrawable
	out.PolicyVersion = policyVersion(snap.QuoteAsset.ID, policyLines)
	return out, nil
}

// classify reads the policy and, when needed, the price for one asset.
func (e *Engine) classify(ctx context.Context, q db.Querier, snap Snapshot, a assets.Asset, now time.Time) (assetWork, error) {
	policy, err := e.policies.Current(ctx, q, a.ID, now)
	if err != nil {
		return assetWork{}, fmt.Errorf("buyingpower: policy for %s: %w", a.ID, err)
	}
	w := assetWork{asset: a, policy: policy, cls: valuation.Classify(a, policy)}
	switch {
	case policy.PolicyMissing:
		w.unvalued, w.reason = StatusPolicyMissing, valuation.ReasonPolicyMissing
	case w.cls.Gate == valuation.GateExcluded:
		w.unvalued, w.reason = StatusHalted, w.cls.Reason
	case w.cls.NeedsPrice():
		p, err := e.prices.Latest(ctx, q, a.ID, snap.QuoteAsset.ID, policy.MaxPriceAge, now)
		if err != nil {
			if errs.HasCode(err, errs.CodeStaleMarketData) {
				w.unvalued, w.reason = StatusStalePrice, StatusStalePrice
				break
			}
			return assetWork{}, fmt.Errorf("buyingpower: price for %s: %w", a.ID, err)
		}
		w.price = &p
	}
	return w, nil
}

func accountRestrictions(snap Snapshot) []Restriction {
	var out []Restriction
	switch snap.AccountStatus {
	case accounts.StatusActive:
	case accounts.StatusFrozen:
		out = append(out, Restriction{Code: RestrictionAccountFrozen, Detail: "account is FROZEN", Scope: ScopeAccount})
	case accounts.StatusRestricted:
		out = append(out, Restriction{Code: RestrictionAccountRestricted, Detail: "account is RESTRICTED: no new risk", Scope: ScopeAccount})
	case accounts.StatusClosed:
		out = append(out, Restriction{Code: RestrictionAccountClosed, Detail: "account is CLOSED", Scope: ScopeAccount})
	default:
		out = append(out, Restriction{Code: RestrictionAccountFrozen, Detail: fmt.Sprintf("account status %q is unknown; treated as FROZEN", snap.AccountStatus), Scope: ScopeAccount})
	}
	for _, ks := range snap.KillSwitches {
		scope := ScopeWithdrawal
		switch {
		case ks.BlocksNewRisk:
			scope = ScopeAccount
		case ks.BlocksWithdrawal:
		default:
			continue
		}
		detail := ks.Kind
		if ks.ScopeID != "" && ks.ScopeID != "*" {
			detail += " scope=" + ks.ScopeID
		}
		if ks.Reason != "" {
			detail += ": " + ks.Reason
		}
		out = append(out, Restriction{Code: RestrictionKillSwitch, Detail: detail, Scope: scope})
	}
	for _, rb := range snap.ReconciliationBlocks {
		detail := rb.Kind
		if rb.RecordID != "" {
			detail += " record=" + rb.RecordID
		}
		if rb.Detail != "" {
			detail += ": " + rb.Detail
		}
		out = append(out, Restriction{Code: RestrictionReconciliationRequired, Detail: detail, Scope: ScopeAccount})
	}
	return out
}

func assetRestriction(w assetWork, aid assets.AssetID) Restriction {
	r := Restriction{AssetID: aid, Scope: ScopeAsset}
	switch w.unvalued {
	case StatusPolicyMissing:
		r.Code, r.Detail = RestrictionPolicyMissing, "no effective asset policy; asset is not valued"
	case StatusHalted:
		r.Code, r.Detail = RestrictionAssetHalted, "asset excluded: "+w.reason
	default:
		r.Code, r.Detail = RestrictionStalePrice, fmt.Sprintf("no price within %s", w.policy.MaxPriceAge)
	}
	return r
}

func balanceStatus(w assetWork) string {
	if w.cls.Gate == valuation.GatePortfolioOnly {
		return StatusPortfolioOnly
	}
	if w.policy.StablecoinStatus != "" {
		return string(w.policy.StablecoinStatus)
	}
	return string(w.policy.Status)
}

// isSettlementAsset: a stablecoin of risk class SETTLEMENT needs no
// conversion to be deployed.
func isSettlementAsset(a assets.Asset) bool {
	return a.IsStablecoin && a.RiskClass == assets.RiskSettlement
}

// computeWithdrawable is min(available, max(0, funding − withdrawals)).
func computeWithdrawable(available, funding, withdrawals money.USD) (money.USD, error) {
	room, err := funding.Sub(withdrawals)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	if room.IsNegative() {
		room = money.USD{}
	}
	if room.Cmp(available) < 0 {
		return room, nil
	}
	return available, nil
}

func blocks(p Purpose, scope RestrictionScope) bool {
	switch p {
	case PurposeTrade, PurposeAgentDeploy:
		return scope == ScopeAccount
	case PurposeWithdrawal:
		return true
	}
	return false
}

// policyVersion hashes the engine version, the quote asset and the sorted
// (asset, policy_version) pairs consulted.
func policyVersion(quote assets.AssetID, lines []string) string {
	h := sha256.New()
	h.Write([]byte("engine=" + EngineVersion + "\n"))
	h.Write([]byte("quote=" + quote.String() + "\n"))
	for _, l := range lines { // already in asset-id order
		h.Write([]byte(l + "\n"))
	}
	return EngineVersion + ":" + hex.EncodeToString(h.Sum(nil))
}

func add(a, b money.USD) (money.USD, error) {
	s, err := a.Add(b)
	if err != nil {
		return money.USD{}, mapMoney(err)
	}
	return s, nil
}

func mapMoney(err error) error {
	if errors.Is(err, money.ErrOverflow) {
		return errs.Wrap(err, errs.CodeOverflow, "buyingpower: overflow")
	}
	return errs.Wrap(err, errs.CodeInternal, "buyingpower: arithmetic")
}
