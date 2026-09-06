package capital

import (
	"context"

	"github.com/nodal/controlplane/internal/capital/buyingpower"
	"github.com/nodal/controlplane/internal/db"
)

// The buying-power output contract (FINANCIAL_MODEL §6 / PART 25) is defined
// once, in internal/capital/buyingpower, and re-exported here so that callers
// which think in terms of "capital" see a single type. The engine lives in the
// sub-package because it depends on valuation and positions, which must not
// depend on this package's reservation service.
type (
	// BuyingPower is the engine output: never persisted or cached as truth.
	BuyingPower = buyingpower.BuyingPower
	// UnderlyingBalance is one held asset with its mark.
	UnderlyingBalance = buyingpower.UnderlyingBalance
	// Haircut records why an asset contributes less than its full value.
	Haircut = buyingpower.Haircut
	// Restriction is one reason the account may deploy less than its balances suggest.
	Restriction = buyingpower.Restriction
	// RestrictionCode is a stable, machine-readable restriction reason.
	RestrictionCode = buyingpower.RestrictionCode
	// RestrictionScope says what a restriction zeroes.
	RestrictionScope = buyingpower.RestrictionScope
	// Purpose names the action buying power is being computed for.
	Purpose = buyingpower.Purpose
)

// Purposes (aliases of the engine's constants).
const (
	PurposeDisplay     = buyingpower.PurposeDisplay
	PurposeTrade       = buyingpower.PurposeTrade
	PurposeAgentDeploy = buyingpower.PurposeAgentDeploy
	PurposeWithdrawal  = buyingpower.PurposeWithdrawal
)

// BuyingPowerEngine computes BuyingPower for an account and purpose from
// ledger balances, reservation totals, withdrawal holds, pending deposits,
// asset policies, prices and account status (FINANCIAL_MODEL §6).
type BuyingPowerEngine interface {
	Compute(ctx context.Context, q db.Querier, accountID string, purpose Purpose) (BuyingPower, error)
}
