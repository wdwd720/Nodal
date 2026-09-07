package httpapi

import (
	"context"

	"github.com/nodal/controlplane/internal/accounts"
	"github.com/nodal/controlplane/internal/assets"
	"github.com/nodal/controlplane/internal/credit"
	"github.com/nodal/controlplane/internal/money"
	"github.com/nodal/controlplane/internal/nativeasset"
	"github.com/nodal/controlplane/internal/nativemarket"
	"github.com/nodal/controlplane/internal/payout"
)

// Ports for the Nodal-native economy (gola.md PARTS XII-XXI).
//
// They follow the same rule as every other port here: one method per domain
// call, no transaction handling above this line, and a nil port answers
// UNSUPPORTED rather than pretending. A deployment that has not provisioned the
// internal economy says so; it does not return an empty balance.

// CreditsPort reads a Credit balance broken down by what may be withdrawn.
type CreditsPort interface {
	// Balance returns the breakdown under the deployment's current payout
	// policy. The policy, the caller's verification level and the active
	// capability set are resolved by the implementation, because they are
	// deployment facts rather than request facts.
	Balance(ctx context.Context, accountID accounts.AccountID) (credit.Balances, error)
}

// NativeAssetsPort creates and reads Nodal-native assets.
type NativeAssetsPort interface {
	Create(ctx context.Context, r CreateNativeAsset) (nativeasset.Asset, nativeasset.Verdict, error)
	Get(ctx context.Context, assetID assets.AssetID) (nativeasset.Asset, error)
	ListTradable(ctx context.Context, limit int) ([]nativeasset.Asset, error)
}

// CreateNativeAsset is the command behind POST /native-assets.
type CreateNativeAsset struct {
	AccountID         accounts.AccountID
	Name              string
	Symbol            string
	Description       string
	ImageURL          string
	MaxSupply         money.Quantity
	CreatorAllocation money.Quantity
	Decimals          uint8
	IdempotencyKey    string
	CorrelationID     string
}

// NativeMarketsPort prices and executes internal market trades.
type NativeMarketsPort interface {
	Market(ctx context.Context, marketID nativemarket.MarketID) (MarketView, error)
	Quote(ctx context.Context, r nativemarket.QuoteRequest) (nativemarket.Quote, error)
	Execute(ctx context.Context, r nativemarket.ExecuteRequest) (nativemarket.ExecuteResult, error)
}

// MarketView is a market together with the state and concentration a buyer
// needs to see before trading it (PART LIV).
type MarketView struct {
	Market  nativemarket.Market
	State   nativemarket.State
	Holders []nativemarket.Holding
}

// CirculatingSupply is what holders collectively hold: everything the curve
// has sold. It is derived here rather than stored, so it cannot disagree with
// the reserves it is computed from.
func (v MarketView) CirculatingSupply() money.Quantity {
	return v.Market.Curve.InitialAssetReserve.Sub(v.State.AssetReserve)
}

// PayoutsPort creates and reads payout requests.
type PayoutsPort interface {
	Create(ctx context.Context, r CreatePayout) (payout.Request, payout.Decision, error)
	Get(ctx context.Context, id payout.RequestID) (payout.Request, error)
	ListByAccount(ctx context.Context, accountID accounts.AccountID, limit int) ([]payout.Request, error)
}

// CreatePayout is the command behind POST /payouts.
type CreatePayout struct {
	AccountID      accounts.AccountID
	Amount         money.Quantity
	DestinationID  *payout.DestinationID
	IdempotencyKey string
	CorrelationID  string
}
