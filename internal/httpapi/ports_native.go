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

	// Pricing is the policy that converts money into Credits. It is served so
	// a funding page can show the rate rather than deriving it: a second
	// implementation of this arithmetic in the browser would eventually
	// disagree with the server, and the server is the one that issues.
	Pricing(ctx context.Context) (credit.PricingPolicy, error)

	// StartPurchase opens a Credit purchase for an amount of MONEY.
	//
	// There is no Credits argument, here or anywhere above it. The quantity is
	// derived from Pricing inside the implementation, so a client cannot ask
	// for nine million by any route.
	StartPurchase(ctx context.Context, r StartCreditPurchase) (credit.StartedPurchase, error)

	// Purchase reads one purchase back. A funding page polls this rather than
	// trusting a provider redirect, which says the customer came back and not
	// that the money arrived.
	Purchase(ctx context.Context, id credit.FundingID) (credit.Funding, error)
}

// StartCreditPurchase is the command behind POST /credits/purchases.
type StartCreditPurchase struct {
	AccountID accounts.AccountID
	// AmountMinor is what the customer will pay, in minor units.
	AmountMinor    int64
	Currency       string
	IdempotencyKey string
	CorrelationID  string
}

// NativeAssetsPort creates and reads Nodal-native assets.
type NativeAssetsPort interface {
	Create(ctx context.Context, r CreateNativeAsset) (nativeasset.Asset, nativeasset.Verdict, error)
	Get(ctx context.Context, assetID assets.AssetID) (nativeasset.Asset, error)
	ListTradable(ctx context.Context, limit int) ([]nativeasset.Asset, error)
	// Submit moves the CREATOR's own DRAFT to PENDING_REVIEW. It is the
	// creator's act and nobody else's: an operator who could submit on their
	// behalf could launch a draft its creator was still editing, and the
	// economics stay editable until the asset goes live.
	Submit(ctx context.Context, accountID accounts.AccountID, assetID assets.AssetID) (nativeasset.Asset, error)
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
	Quote(ctx context.Context, r nativemarket.QuoteRequest) (NativeQuoteView, error)
	Execute(ctx context.Context, r nativemarket.ExecuteRequest) (NativeExecuteView, error)
}

// NativeQuoteView and NativeExecuteView pair a domain result with the scale of its asset
// side. The scale lives in the asset registry rather than on the market, so the
// domain types do not carry it and the transport layer would otherwise have to
// guess -- which is what F-44 was: the page rendered asset quantities at the
// Credit's six decimals, correct only while a creator happens to choose six.
type NativeQuoteView struct {
	Quote         nativemarket.Quote
	AssetDecimals int
}

// NativeExecuteView is the same for a completed trade.
type NativeExecuteView struct {
	Result        nativemarket.ExecuteResult
	AssetDecimals int
}

// MarketView is a market together with the state and concentration a buyer
// needs to see before trading it (PART LIV).
type MarketView struct {
	Market  nativemarket.Market
	State   nativemarket.State
	Holders []nativemarket.Holding
	// AssetDecimals is the scale of every asset quantity in this view: the
	// reserves, the circulating supply and each holder's balance. It is a fact
	// about the asset, and it is NOT the Credit scale -- the page showed all of
	// them at the Credit's six decimals, which is right only while a creator
	// happens to choose six (F-44).
	AssetDecimals int
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
	// Cancel withdraws the account's OWN request before it is submitted,
	// returning the reserved Credits to the exact lots they came from.
	//
	// Without it a user's Credits could be reserved with no way for that user
	// to release them: `payout.Cancel` existed and had no caller outside
	// tests, so only an operator could free them, and only by running their own
	// tool.
	Cancel(ctx context.Context, accountID accounts.AccountID, id payout.RequestID, reason string) (payout.Request, error)
}

// CreatePayout is the command behind POST /payouts.
type CreatePayout struct {
	AccountID      accounts.AccountID
	Amount         money.Quantity
	DestinationID  *payout.DestinationID
	IdempotencyKey string
	CorrelationID  string
}
