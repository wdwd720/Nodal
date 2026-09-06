package buyingpower

import (
	"time"

	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/money"
)

// The output types below are the PART 25 / FINANCIAL_MODEL §6 shape. They
// are declared here because internal/capital/buyingpower_types.go (the
// planned home of capital.BuyingPower, capital.Restriction, capital.Haircut,
// capital.UnderlyingBalance and capital.Purpose) did not exist when this
// package was written; the integrator unifies the two declarations. Field
// names match the documented output verbatim so the unification is
// mechanical.

// Purpose is the action the caller intends. It selects which restrictions
// are blocking; it never changes the figures.
type Purpose string

// Purposes. AGENT_DEPLOY (an agent intent drawing on a capital envelope)
// is treated exactly like TRADE by this engine; envelope limits are applied
// by internal/capital, not here.
const (
	PurposeDisplay     Purpose = "DISPLAY"
	PurposeTrade       Purpose = "TRADE"
	PurposeAgentDeploy Purpose = "AGENT_DEPLOY"
	PurposeWithdrawal  Purpose = "WITHDRAWAL"
)

// Valid reports whether p is a declared purpose.
func (p Purpose) Valid() bool {
	switch p {
	case PurposeDisplay, PurposeTrade, PurposeAgentDeploy, PurposeWithdrawal:
		return true
	}
	return false
}

// RestrictionCode is a stable, machine-readable reason.
type RestrictionCode string

// Restriction codes.
const (
	RestrictionAccountFrozen          RestrictionCode = "ACCOUNT_FROZEN"
	RestrictionAccountRestricted      RestrictionCode = "ACCOUNT_RESTRICTED"
	RestrictionAccountClosed          RestrictionCode = "ACCOUNT_CLOSED"
	RestrictionKillSwitch             RestrictionCode = "KILL_SWITCH"
	RestrictionReconciliationRequired RestrictionCode = "RECONCILIATION_REQUIRED"
	RestrictionStalePrice             RestrictionCode = "STALE_PRICE"
	RestrictionAssetHalted            RestrictionCode = "ASSET_HALTED"
	RestrictionPolicyMissing          RestrictionCode = "POLICY_MISSING"
)

// RestrictionScope says what a restriction zeroes.
type RestrictionScope string

// Restriction scopes.
const (
	// ScopeAccount: no new risk at all; buying_power and available_now are 0.
	ScopeAccount RestrictionScope = "ACCOUNT"
	// ScopeWithdrawal: trading may continue; withdrawable is 0.
	ScopeWithdrawal RestrictionScope = "WITHDRAWAL"
	// ScopeAsset: the named asset contributes nothing; withdrawable is 0.
	ScopeAsset RestrictionScope = "ASSET"
)

// Restriction is one reason the account cannot do everything it otherwise
// could. Blocking is true when the restriction blocks the requested Purpose.
type Restriction struct {
	Code     RestrictionCode  `json:"code"`
	Detail   string           `json:"detail"`
	AssetID  assets.AssetID   `json:"asset,omitzero"` // zero unless Scope is ScopeAsset
	Scope    RestrictionScope `json:"scope"`
	Blocking bool             `json:"blocking"`
}

// Haircut records why an asset contributes less than its full value.
type Haircut struct {
	AssetID   assets.AssetID `json:"asset"`
	FactorBPS money.BPS      `json:"factor_bps"` // effective collateral factor; 0 when excluded
	Reason    string         `json:"reason"`     // a valuation.Reason* code or STALE_PRICE
}

// UnderlyingBalance is one held asset with its mark.
type UnderlyingBalance struct {
	AssetID  assets.AssetID `json:"asset"`
	Symbol   string         `json:"symbol"` // display only
	Decimals uint8          `json:"decimals"`
	Quantity money.Quantity `json:"quantity"`
	USDValue money.USD      `json:"usd_value"`
	PriceRef string         `json:"price_ref"` // "face:USD", "<source>@<observed_at>", or "" when unvalued
	// Status is the valuation state: the stablecoin status (NORMAL,
	// DEGRADED) or asset status (ACTIVE, CLOSE_ONLY, DELISTING) for
	// contributing assets, PORTFOLIO_ONLY, HALTED, STALE_PRICE or
	// POLICY_MISSING otherwise.
	Status string `json:"status"`
}

// Balance status values not taken from a policy status.
const (
	StatusPortfolioOnly = "PORTFOLIO_ONLY"
	StatusHalted        = "HALTED"
	StatusStalePrice    = "STALE_PRICE"
	StatusPolicyMissing = "POLICY_MISSING"
)

// BuyingPower is the engine output (PART 25). All amounts are USD.
type BuyingPower struct {
	PortfolioValue     money.USD           `json:"portfolio_value"`
	BuyingPower        money.USD           `json:"buying_power"`
	AvailableNow       money.USD           `json:"available_now"`
	Reserved           money.USD           `json:"reserved"`
	Pending            money.USD           `json:"pending"`
	Withdrawable       money.USD           `json:"withdrawable"`
	UnderlyingBalances []UnderlyingBalance `json:"underlying_balances"`
	Haircuts           []Haircut           `json:"haircuts"`
	Restrictions       []Restriction       `json:"restrictions"`
	PolicyVersion      string              `json:"policy_version"`
	AsOf               time.Time           `json:"as_of"`
	Purpose            Purpose             `json:"purpose"`
}

// Blocked reports whether any restriction blocks the requested purpose.
func (b BuyingPower) Blocked() bool {
	for _, r := range b.Restrictions {
		if r.Blocking {
			return true
		}
	}
	return false
}
